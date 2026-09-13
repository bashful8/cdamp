package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"cdamp/internal/domain"
	"cdamp/internal/domain/fakes"
)

func TestCreateAgent_HappyPath(t *testing.T) {
	store := fakes.NewInboxStoreFake()

	res, err := CreateAgent(context.Background(), store, "example.dev", CreateAgentRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("CreateAgent returned error: %v", err)
	}
	if res == nil {
		t.Fatal("CreateAgent returned nil result with nil error")
	}

	// (b) Address is built correctly from the local domain.
	if want := "alice@example.dev"; res.Address != want {
		t.Errorf("Address = %q, want %q", res.Address, want)
	}
	if res.Token == "" {
		t.Fatal("Token is empty")
	}

	// (a) the generated token round-trips through the exact hash scheme
	// FindAgentByTokenHash lookups would use (plain SHA-256, hex-encoded
	// — the same scheme internal/adapters/http/middleware.go's
	// hashBearerToken uses).
	sum := sha256.Sum256([]byte(res.Token))
	wantHash := hex.EncodeToString(sum[:])

	agent, err := store.FindAgentByTokenHash(context.Background(), wantHash)
	if err != nil {
		t.Fatalf("FindAgentByTokenHash(%q) returned error: %v", wantHash, err)
	}
	if agent.Name != "alice" {
		t.Errorf("resolved agent.Name = %q, want %q", agent.Name, "alice")
	}
	if agent.TokenHash != wantHash {
		t.Errorf("agent.TokenHash = %q, want %q", agent.TokenHash, wantHash)
	}
}

func TestCreateAgent_DuplicateName_ReturnsConflict(t *testing.T) {
	store := fakes.NewInboxStoreFake()

	if _, err := CreateAgent(context.Background(), store, "example.dev", CreateAgentRequest{Name: "alice"}); err != nil {
		t.Fatalf("first CreateAgent returned error: %v", err)
	}

	// (c) a duplicate-name conflict is surfaced as a distinguishable
	// error, not swallowed or treated as success.
	_, err := CreateAgent(context.Background(), store, "example.dev", CreateAgentRequest{Name: "alice"})
	if err == nil {
		t.Fatal("second CreateAgent with duplicate name returned nil error, want a wrapped domain.ErrConflict")
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("err = %v, want errors.Is(err, domain.ErrConflict)", err)
	}
}

func TestCreateAgent_TokenNeverLogged(t *testing.T) {
	store := fakes.NewInboxStoreFake()

	// (d) the plaintext token never appears in anything resembling a log
	// call within this function: redirect the default slog logger to a
	// buffer for the duration of the call and assert the returned token
	// never appears in whatever (if anything) was written to it.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	res, err := CreateAgent(context.Background(), store, "example.dev", CreateAgentRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("CreateAgent returned error: %v", err)
	}

	if logged := logBuf.String(); strings.Contains(logged, res.Token) {
		t.Errorf("plaintext token %q found in log output: %q", res.Token, logged)
	}
}

func TestCreateAgent_TwoCallsProduceDifferentTokens(t *testing.T) {
	store := fakes.NewInboxStoreFake()

	res1, err := CreateAgent(context.Background(), store, "example.dev", CreateAgentRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("first CreateAgent returned error: %v", err)
	}
	res2, err := CreateAgent(context.Background(), store, "example.dev", CreateAgentRequest{Name: "bob"})
	if err != nil {
		t.Fatalf("second CreateAgent returned error: %v", err)
	}

	if res1.Token == res2.Token {
		t.Errorf("two separate CreateAgent calls produced the same token %q", res1.Token)
	}
}
