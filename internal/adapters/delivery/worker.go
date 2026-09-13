package delivery

import (
	"context"
	"log/slog"
	"time"

	"cdamp/internal/domain"
)

// claimLimit bounds how many messages one tick claims, so a single tick
// can't monopolize the DB connection pool. Not pinned by any doc — same
// judgment-call category as client.go's deliverTimeout; the
// cdamp-federation skill's own suggested value ("e.g. 50").
const claimLimit = 50

// tickInterval is the fixed period between claim attempts. Not pinned by
// any doc — the cdamp-federation skill's own suggested value ("e.g. every
// 10s"), same judgment-call category as deliverTimeout/claimLimit.
const tickInterval = 10 * time.Second

// Worker is the background outbound-delivery loop: it repeatedly claims
// due messages, resolves each recipient, hands off to Delivery, and
// records the outcome via MarkDelivered/MarkFailed with backoff per
// retrySchedule.
type Worker struct {
	store         domain.InboxStore
	directory     domain.Directory
	delivery      domain.Delivery
	retrySchedule []time.Duration

	// now is an injectable time source, overridable only from this
	// package's own tests, so backoff-schedule math can be exercised
	// without a real sleep — mirrors directory.go's own `now` field
	// exactly. Defaults to time.Now in NewWorker.
	now func() time.Time
}

// NewWorker returns a Worker that claims from store, resolves via
// directory, delivers via delivery, and schedules retries per
// retrySchedule (pass cfg.RetrySchedule at wiring time — task 3 — this
// constructor takes the resolved slice directly, not a *config.Config,
// mirroring client.go/directory.go's pattern of plain typed params over a
// whole config struct).
func NewWorker(store domain.InboxStore, directory domain.Directory, delivery domain.Delivery, retrySchedule []time.Duration) *Worker {
	return &Worker{
		store:         store,
		directory:     directory,
		delivery:      delivery,
		retrySchedule: retrySchedule,
		now:           time.Now,
	}
}

// Run blocks, performing one claim-and-process pass immediately, then
// again every tickInterval, until ctx is canceled — at which point Run
// returns. A ClaimPending failure is logged via slog.Default() (no new
// constructor dependency; log/slog per 04-BUILD-PLAN.md's Engineering
// practices) and the loop continues to the next tick rather than
// crashing the daemon.
func (w *Worker) Run(ctx context.Context) {
	if err := w.runOnce(ctx); err != nil {
		slog.Default().Error("delivery worker: claim pending failed", "error", err)
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil {
				slog.Default().Error("delivery worker: claim pending failed", "error", err)
			}
		}
	}
}

// runOnce performs exactly one ClaimPending call and processes every
// returned message. Unexported but directly callable from worker_test.go
// (same package) — this is the method backoff-schedule tests call
// directly, never Run, so tests never depend on tickInterval's real 10s
// value. Returns the ClaimPending error, if any (Run logs it; a direct
// test can assert on it too).
func (w *Worker) runOnce(ctx context.Context) error {
	messages, err := w.store.ClaimPending(ctx, claimLimit)
	if err != nil {
		return err
	}

	for _, m := range messages {
		w.process(ctx, m)
	}
	return nil
}

// process handles exactly one claimed message: the expiry pre-check,
// resolve, deliver, and outcome-recording steps, in the order
// 02-ARCHITECTURE.md's Delivery worker flow and the cdamp-federation
// skill both specify.
func (w *Worker) process(ctx context.Context, m *domain.Message) {
	// Expiry pre-check: if the message has already expired, give up
	// immediately per 01-PROTOCOL.md's Expiry section — no Resolve or
	// Deliver call at all.
	if m.ExpiresAt != nil && !w.now().Before(*m.ExpiresAt) {
		if err := w.store.MarkFailed(ctx, m.ID, nil, "expired"); err != nil {
			slog.Default().Error("delivery worker: mark failed (expired)", "message_id", m.ID, "error", err)
		}
		return
	}

	// Only inboxURL is used — pubkey/kid are for verifying inbound
	// signatures (ReceiveMessage's job), not needed for an outbound
	// delivery, which signs with the local domain's own key via Delivery.
	_, _, inboxURL, err := w.directory.Resolve(ctx, m.To)
	if err == nil {
		err = w.delivery.Deliver(ctx, m, inboxURL)
	}

	if err == nil {
		if markErr := w.store.MarkDelivered(ctx, m.ID); markErr != nil {
			slog.Default().Error("delivery worker: mark delivered", "message_id", m.ID, "error", markErr)
		}
		return
	}

	w.recordFailure(ctx, m, err)
}

// recordFailure runs the backoff decision for m after a resolve/deliver
// failure err, per this task's spec: schedule the next attempt from
// retrySchedule indexed by m.Attempts, giving up early ("expired") if the
// next scheduled attempt would land after m.ExpiresAt, or terminally
// (nextAttempt=nil) once the schedule is exhausted.
func (w *Worker) recordFailure(ctx context.Context, m *domain.Message, deliverErr error) {
	reason := deliverErr.Error()
	if m.Attempts < len(w.retrySchedule) {
		candidate := w.now().Add(w.retrySchedule[m.Attempts])
		if m.ExpiresAt != nil && candidate.After(*m.ExpiresAt) {
			// The next scheduled attempt would land after expiry — per
			// 01-PROTOCOL.md's Expiry section, give up now rather than
			// waiting out the rest of the schedule pointlessly. reason is
			// fixed to "expired" here (not err's text) — the terminal
			// state is caused by expiry, not by whatever this attempt's
			// error was.
			if err := w.store.MarkFailed(ctx, m.ID, nil, "expired"); err != nil {
				slog.Default().Error("delivery worker: mark failed (expired)", "message_id", m.ID, "error", err)
			}
			return
		}
		if err := w.store.MarkFailed(ctx, m.ID, &candidate, reason); err != nil {
			slog.Default().Error("delivery worker: mark failed", "message_id", m.ID, "error", err)
		}
		return
	}

	// Schedule exhausted (this was the 6th attempt, index 5, out of
	// range): terminal failure, no further retries. reason is this
	// attempt's actual error text.
	if err := w.store.MarkFailed(ctx, m.ID, nil, reason); err != nil {
		slog.Default().Error("delivery worker: mark failed (exhausted)", "message_id", m.ID, "error", err)
	}
}
