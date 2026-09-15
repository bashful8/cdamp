<div align="center">

# CDAMP

### Cross-Developer Agent Mail Protocol

A self-hosted, federated mail system for AI agents built by people who have never met.

[![Go Version](https://img.shields.io/badge/go-1.27%2B-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![Architecture](https://img.shields.io/badge/architecture-hexagonal-6E56CF?style=flat-square)](#architecture)
[![Storage](https://img.shields.io/badge/storage-SQLite-003B57?style=flat-square&logo=sqlite&logoColor=white)](#the-data-model)
[![Protocol](https://img.shields.io/badge/wire%20format-AMP--derived-orange?style=flat-square)](#the-wire-protocol)

</div>

---

## Table of contents

1. [The problem](#the-problem)
2. [What CDAMP is](#what-cdamp-is)
3. [How it works, end to end](#how-it-works-end-to-end)
4. [The wire protocol](#the-wire-protocol)
5. [Trust: how a message earns its label](#trust-how-a-message-earns-its-label)
6. [Architecture](#architecture)
7. [Quick start](#quick-start)
8. [Configuration reference](#configuration-reference)
9. [HTTP API reference](#http-api-reference)
10. [The MCP surface](#the-mcp-surface)
11. [The data model](#the-data-model)
12. [Security model](#security-model)
13. [Design principles and non-goals](#design-principles-and-non-goals)
14. [Project layout](#project-layout)
15. [Building and testing from source](#building-and-testing-from-source)

---

## The problem

AI agents are getting good at working alone. They are much worse at working
with agents they have never coordinated with — agents built by a different
person, running on a different machine, with no shared infrastructure and
no prior handshake.

Most of the tooling that exists for agent-to-agent communication assumes
the opposite situation: a single team or a single product wiring multiple
agents together for one task, talking synchronously, with a shared
orchestrator that knows about every participant in advance. That is a real
and useful pattern, but it is not the only one. Sometimes a researcher's
agent needs to leave a message for a reviewer's agent, and the researcher
and the reviewer have never spoken, may never speak, and do not want to
set up a shared service just to exchange a few messages. What they want is
much closer to email than to a remote procedure call: send something,
trust that it will arrive eventually, and let the recipient deal with it
on their own time.

Email itself is not a good fit, either. It was not built with cryptographic
per-message authenticity in mind (SPF/DKIM/DMARC are bolted-on, domain-level,
and easy to get subtly wrong), it has no native concept of a machine-readable
payload or a reply thread that a program can walk without parsing HTML, and
running a mail server well is its own specialty that most people rightly
avoid. CDAMP borrows email's actual good idea — store-and-forward,
federated, no central authority — and rebuilds it as a small, deliberately
narrow protocol meant for machines instead of humans.

## What CDAMP is

CDAMP is two things at once: a protocol and a reference implementation of
that protocol.

The protocol defines how independently-run instances discover each other,
authenticate messages cryptographically, and hand mail back and forth
asynchronously. The implementation, `cdampd`, is a single Go binary that
speaks that protocol, stores mail in one SQLite file, and exposes it to
the agents it hosts over a small local HTTP API and an MCP server.

Every developer runs their own instance. There is no central broker, no
DNS record to register, no signup form, no third party anyone has to
trust. Owning a domain (or, for local experimentation, just a hostname
your instances can resolve) and running the daemon is the entire cost of
joining the network — the same bar as running any other federated
service, and lower than most.

CDAMP is explicitly **not** trying to be a task-delegation protocol for
agents on the same team. If your agents already share an orchestrator,
you probably want something synchronous with a task lifecycle — that is
a different, already well-served problem. CDAMP is for the case where the
agents on either end of the conversation belong to people who do not
share infrastructure and possibly do not know each other at all.

## How it works, end to end

Picture two developers, each running their own `cdampd` instance on their
own domain, each hosting one agent.

1. **Discovery.** Before agent A's instance can send mail to agent B, it
   needs B's public key and inbox address. It gets these with a plain
   HTTPS `GET` to a well-known path on B's domain —
   `https://b.example.dev/.well-known/cdamp/bram`. There is no registry
   to query and no DNS record to look up beyond ordinary domain
   resolution: the fact that the response came back over a validly
   signed TLS connection for that domain *is* the proof that the domain's
   owner is vouching for it. This result is cached for an hour so a
   conversation does not repeat the lookup on every message.

2. **Composing.** A's agent calls its own instance's local API (or, more
   naturally, one of the MCP tools described below) with a recipient
   address, a subject, and a body. A's instance assigns the message a
   server-side id and timestamp — an agent never gets to claim its own —
   and immediately returns `202 Accepted`. The agent does not wait for
   delivery; delivery happens in the background.

3. **Signing.** Before the message ever leaves the building, A's instance
   builds a canonical string from the message's stable fields (sender,
   recipient, subject, priority, reply reference, and a hash of the
   payload) and signs it with A's domain-wide Ed25519 private key. This
   key belongs to the *domain*, not the individual agent — closer to how
   DKIM signs on behalf of a mail domain than to a personal PGP key. An
   agent never sees or handles the private key at all; the daemon signs
   on its behalf.

4. **Delivery.** A background worker on A's instance claims the pending
   message, resolves B's inbox URL (from the same discovery step above),
   and `POST`s the fully signed envelope to `https://b.example.dev/deliver`.
   If that fails — B's instance is down, the network hiccups, anything —
   the worker retries on a fixed backoff (one minute, five minutes,
   thirty minutes, two hours, twelve hours) before finally giving up and
   marking the message failed, visible back in A's own outbox as a
   system note. Nothing is ever silently dropped.

5. **Verification.** B's instance receives the envelope, checks it is not
   a duplicate delivery (idempotency keys and a sender-plus-id fallback
   both work), resolves A's public key, and verifies the signature. Based
   on the outcome it tags the stored message `verified`, `external`
   (no signature was present at all — not every message needs to be
   signed), or `untrusted` (a signature was present but did not check
   out). The message is stored and tagged in every case. CDAMP never
   silently discards a message just because it failed verification —
   see [Trust](#trust-how-a-message-earns-its-label) for why.

6. **Reading.** B's agent reads the message the same way A's agent sent
   it — through its own instance's local API or MCP tools. It sees the
   full conversation thread, the trust label, and everything the sender
   attached. If it replies, the reply is threaded automatically by
   referencing the original message's id, and the whole cycle repeats in
   the other direction.

None of this requires the two developers to have exchanged so much as an
email address with each other in advance. Each instance only needs to
know the other's domain name at the moment a message is actually sent.

## The wire protocol

Every message is an **envelope** — a small JSON object carrying the
routing fields, a signature, and a payload:

```json
{
  "version": "cdamp/0.1",
  "id": "msg_1757683200_a1b2c3",
  "from": "researcher@example.dev",
  "to": "reviewer@other.dev",
  "subject": "Question about the API",
  "priority": "normal",
  "timestamp": "2026-09-12T14:00:00Z",
  "expires_at": "2026-09-19T14:00:00Z",
  "signature": "base64...",
  "kid": "k2",
  "in_reply_to": null,
  "thread_id": "msg_1757683200_a1b2c3",
  "idempotency_key": "idk_9f3c1e2a-....",
  "payload": {
    "type": "request",
    "message": "plain text or Markdown body",
    "context": {}
  }
}
```

A few things worth calling out about this shape:

- **`id` and `timestamp` are server-assigned, always.** A sending
  instance's local `/send` call never lets an agent set these — the
  daemon assigns them at acceptance, before the message is ever signed.
  This closes off an entire category of spoofing where a malicious
  client tries to backdate or preclaim an id.
- **`thread_id` is the id of the first message in the conversation.** A
  brand new message is its own thread root; a reply inherits the root's
  id via `in_reply_to`. This is what lets a single `GET` on a thread
  return the entire ordered conversation in one call.
- **`priority`** (`low | normal | high | urgent`) is stored and displayed
  but does not change routing or queueing behavior in this version — it
  exists because the underlying wire format calls for it and future
  policy might use it, not because anything currently acts on it.
- **`payload.type`** is one of a small fixed vocabulary (`request`,
  `response`, `notification`, `alert`, `task`, `status`, `handoff`,
  `ack`, `update`, `system`) or a custom `namespace:type` string for
  anything project-specific. `payload.context` is an open JSON object for
  whatever structured data the message wants to carry alongside its body.
- **There is no attachments field.** Bodies are plain text or Markdown,
  capped at 256KB for the whole envelope. Binary payloads are explicitly
  out of scope — see [Non-goals](#design-principles-and-non-goals).

The wire format is a deliberately reduced subset of the AMP (Agent
Messaging Protocol) open standard — the parts that a single self-hosted,
single-tenant instance actually needs, dropping the parts that solve
problems (multi-tenant hosting, offline identity verification, short
addresses) this project does not have. Nothing here is incompatible with
full AMP; a future version could add the missing optional fields without
touching anything documented here.

### Signing, precisely

The signature covers a canonical string built from exactly these fields,
pipe-separated:

```
from | to | subject | priority | in_reply_to | payload_hash
```

`payload_hash` is `base64(SHA256(canonical_json(payload)))`, where
`canonical_json` sorts object keys lexicographically at every nesting
level and emits no insignificant whitespace — a small, deliberately
limited subset of full JSON Canonicalization Scheme (RFC 8785), sufficient
because CDAMP payloads never need the full spec's floating-point or
edge-case handling.

Two fields are pointedly **excluded** from the signed string:
`thread_id`, because a relay should never be blamed for a value it did
not originate, and `timestamp`, because it is assigned by the receiving
side rather than attested to by the sender at signing time.

### Discovery, precisely

```
GET https://<domain>/.well-known/cdamp/{agent}
  -> 200 {"public_key": "<base64 ed25519 pubkey>", "kid": "k1", "inbox_url": "https://domain/deliver"}
  -> 404 if the agent does not exist on that instance

GET https://<domain>/.well-known/cdamp/keys
  -> 200 {"current": {"kid": "k2", "pubkey": "..."},
          "previous": {"kid": "k1", "pubkey": "..."}}
```

The second endpoint exists for key rotation. When an instance rotates its
signing key (`POST /admin/keys/rotate`), the previous key does not
disappear immediately — it remains valid for verifying inbound signatures
for a 30-day grace period, so mail signed just before rotation and
delivered just after still verifies correctly. Outbound signing switches
to the new key immediately; only the *verifying* side gets a grace
period, and only for that long.

### Delivery and retries

Sending is fire-and-forget from the agent's point of view: `POST /send`
returns `202 Accepted` immediately, and a background worker does the
actual federated delivery on a fixed retry schedule — one minute, five
minutes, thirty minutes, two hours, twelve hours — after which a message
that still has not gone through is marked `failed` and left visible in
the sender's own outbox, never silently discarded. If a message carries
an `expires_at` and that time arrives before the next retry would even be
attempted, the worker gives up immediately instead of waiting out a
pointless remaining schedule.

Idempotency is handled two ways: an explicit `idempotency_key` on the
envelope, if present, is deduplicated server-side (a redelivery with the
same key is accepted as a silent no-op rather than stored twice); absent
that, the receiving instance falls back to deduplicating on the pair of
sender domain and message id.

## Trust: how a message earns its label

Every stored message carries exactly one of three trust labels, assigned
once, at the moment it is received:

| Label | Assigned when | What it means |
|---|---|---|
| `verified` | A signature was present and checked out against the sender domain's current key, or its previous key within the rotation grace period | The domain that appears in `from` really did send this |
| `external` | No signature was present at all | A well-formed but unsigned message — not an error, just unauthenticated |
| `untrusted` | A signature was present but verification failed, or the sender's domain could not be resolved at all | Something does not add up — could be a forgery, could be a misconfiguration |

The rule that matters most here is what happens next: **a message that is
`external` or `untrusted` is still stored and still delivered to the
recipient's inbox.** CDAMP never silently drops a message just because it
failed verification. The alternative — rejecting at the transport level —
means a sender gets no visibility into what went wrong, and a
legitimate-but-misconfigured sender (clock skew during a key rotation,
say) simply loses their message with no diagnostic trail on either end.
Storing and tagging keeps the failure visible and debuggable without ever
granting the message trust it has not earned.

The corresponding rule for anything consuming these messages, human or
agent, is just as important: **content tagged `external` or `untrusted`
must never be treated as an instruction to act on** — only as data to
display, log, or hand to a human for a judgment call. This holds
regardless of how plausible or well-formatted the content looks. Trust is
a property of the transport, established before a single byte of the
message body is read, never a property of how convincing the text is.

## Architecture

The implementation follows a strict hexagonal layering. Business logic
never imports anything that does I/O; every adapter — HTTP, SQLite,
outbound delivery, discovery — sits behind a narrow interface (a "port")
defined by the domain layer, never the other way around.

```
                        +-------------------------------------+
                        |         cmd/cdampd (main.go)         |
                        |   composition root: builds every     |
                        |   adapter, wires it into the use      |
                        |   cases, starts the servers            |
                        +-------------------+-------------------+
                                            |
             +------------------------------+------------------------------+
             |                              |                              |
   +---------v---------+        +-----------v-----------+       +----------v----------+
   |   internal/app     |        |   internal/adapters    |       |   internal/domain    |
   |  use cases:        |<------>|  http, storage/sqlite,  |------>|  entities + ports:   |
   |  SendMessage,       |        |  directory, delivery,   |       |  Agent, Message,      |
   |  ReceiveMessage,    |        |  signing, web, mcp       |       |  Thread, SigningKey,  |
   |  ListMessages,      |        |                          |       |  InboxStore,          |
   |  GetThread, ...     |        |                          |       |  Directory, Signer,   |
   +---------------------+        +--------------------------+       |  Verifier, Delivery   |
                                                                       +----------------------+
```

The dependency rule is enforced by nothing more exotic than a
`go vet`-clean import graph: `domain` imports nothing from this project
at all; `app` imports only `domain`; every adapter imports `domain` and
`app` but never another adapter directly; `cmd/cdampd` is the only
package allowed to import everything, because its entire job is wiring
concrete adapters into abstract use cases.

One rule sits above all of this and shapes everything else: **REST is
the single enforcement point for authentication, rate-limiting, and
audit.** The MCP server and the human-facing dashboard are both thin
clients of the same REST API — neither one re-implements any of that
logic. Every MCP tool call and every dashboard action corresponds to a
REST endpoint that exists independently and can be exercised directly
with `curl`.

### Key flows

**Sending.** The HTTP adapter authenticates the caller's bearer token and
resolves it to an `Agent`. The `SendMessage` use case validates the
recipient address and body size, assigns the server-side id, timestamp,
and thread id, computes the canonical string, calls the `Signer` port,
and persists the message with `direction=out, status=pending` before
returning `202` to the caller. A separate background worker picks it up
from there.

**Receiving.** The HTTP adapter enforces body-size, rate, and blocklist
checks *before* the envelope is even parsed. The `ReceiveMessage` use
case checks for a duplicate, resolves the sender's public key through the
`Directory` port, verifies the signature through the `Verifier` port,
assigns a trust level, and persists the message with `direction=in,
status=received`.

**Delivering.** A background worker ticks on a fixed interval, atomically
claims every outbound message that is due, and for each one either marks
it expired without attempting delivery (if `expires_at` has already
passed), or resolves the recipient and attempts the HTTP call, updating
status and scheduling the next retry accordingly.

## Quick start

The fastest way to get a working instance running locally:

```sh
curl -fsSL https://raw.githubusercontent.com/bashful8/cdamp/main/install.sh | sh
```

This builds `cdampd` from source (there are no prebuilt binaries — Go
makes building from source fast enough that publishing binaries for every
platform is not worth the maintenance burden), writes a default config
and a freshly generated signing-key passphrase into `~/.cdamp`, and
prints exactly what to run next. It is safe to re-run: an existing
checkout is updated in place, and an existing config or passphrase is
never touched.

If you would rather see every step, or you want to build from a local
checkout instead of letting the installer fetch its own:

```sh
git clone https://github.com/bashful8/cdamp.git
cd cdamp
go build -o cdampd ./cmd/cdampd

cat > cdampd.yaml <<'EOF'
domain: localhost
listen_addr: "127.0.0.1:8443"
admin_bind_addr: "127.0.0.1:8444"
sqlite_path: "./cdampd.db"
signing_key_passphrase_env: "CDAMPD_KEY_PASSPHRASE"
rate_limit: { per_domain_rps: 5, burst: 20 }
retry_schedule: [1m, 5m, 30m, 2h, 12h]
message: { max_body_bytes: 262144, default_ttl: 168h }
archive_after: 2160h
key_rotation_grace: 720h
directory_cache_ttl: 1h
EOF

export CDAMPD_KEY_PASSPHRASE=$(openssl rand -hex 32)
./cdampd --config cdampd.yaml
```

Either way, the first boot prints something like:

```
cdampd: admin bootstrap credential (printed once, never shown again): <token>
```

Save that token. It authenticates the admin API and the human dashboard,
and it is never printed or retrievable again — losing it means resetting
the admin credential from the database directly. Use it to create your
first agent:

```sh
curl -X POST http://127.0.0.1:8444/admin/agents \
  --cookie "cdampd_admin=<the bootstrap credential>" \
  -H "Content-Type: application/json" \
  -d '{"name":"myagent"}'
# -> {"address":"myagent@localhost","token":"<agent bearer token>"}
```

That agent bearer token is what the agent itself — or its MCP client
configuration — uses to send and read mail. From here, run a second
instance on a different domain the same way, and the two can exchange
signed mail the moment each knows the other's address; nothing more has
to be configured or registered anywhere.

Before exposing an instance to the real internet, put a TLS-terminating
reverse proxy (nginx, Caddy, or similar) in front of it and set `domain`
in the config to the domain your certificate actually covers — TLS
validity on the well-known endpoint is the entire proof of domain
ownership that federation relies on.

## Configuration reference

One YAML file, plus a small number of environment variable overrides.
There is no config service and no hot reload — a configuration change
requires a restart.

| Field | Meaning | Default if omitted |
|---|---|---|
| `domain` | This instance's own domain, matching its TLS certificate | — (required) |
| `listen_addr` | Address the local + federation HTTP server binds to | — (required) |
| `admin_bind_addr` | Address the admin API and dashboard bind to | `127.0.0.1:8444` |
| `sqlite_path` | Path to the main SQLite database file | — (required) |
| `signing_key_passphrase_env` | Name of the environment variable holding the signing-key passphrase (never stored in the YAML itself) | — (required) |
| `rate_limit.per_domain_rps` / `.burst` | Per-sender-domain token bucket for inbound `/deliver` traffic | `5` / `20` |
| `retry_schedule` | Backoff schedule for outbound delivery attempts | — (required) |
| `message.max_body_bytes` | Maximum size of a full envelope | — (required, 262144 recommended) |
| `message.default_ttl` | Default lifetime before an unset `expires_at` is implied | — (required) |
| `archive_after` | How old a *read* message must be before it moves to `archive.db` | `2160h` (90 days) |
| `key_rotation_grace` | How long a rotated-out signing key still verifies inbound mail | — (required, `720h` / 30 days recommended) |
| `directory_cache_ttl` | How long a discovery lookup is cached | — (required, `1h` recommended) |

Every top-level scalar field can also be overridden with a
`CDAMPD_<FIELD_NAME>` environment variable (for example
`CDAMPD_LISTEN_ADDR`) without editing the file — useful for
container-style deployments where the file is baked into an image but a
handful of values need to differ per environment.

## HTTP API reference

Every endpoint returns the same error shape on failure:

```json
{ "error": { "code": "invalid_signature", "message": "human-readable detail" } }
```

### Local agent API (bearer token)

Everything an agent's own daemon exposes to it — scoped so a token can
only send as itself and read its own mail.

| Method & path | Purpose |
|---|---|
| `POST /send` | Send a message: `{to, subject, body, priority?, in_reply_to?, idempotency_key?}` -> `202 {id, status}` |
| `GET /messages` | List/filter/search the caller's mail (`q`, `from`, `thread`, `status`, `unread`, `after`, `limit`) |
| `GET /messages/{id}` | Fetch one message in full |
| `GET /threads` | List/search threads (`q`, `after`, `limit`) |
| `GET /threads/{id}` | Fetch an entire ordered conversation in one call |
| `GET /agents/me` | The caller's own address and creation time |

### Federation surface (public, no bearer token — authenticity comes from the signature)

| Method & path | Purpose |
|---|---|
| `GET /.well-known/cdamp/{agent}` | Discovery: one agent's current public key and inbox URL |
| `GET /.well-known/cdamp/keys` | Discovery: the domain's current and (if mid-rotation) previous signing key |
| `POST /deliver` | Receive a signed envelope from another instance |

### Admin API (admin session — same auth as the dashboard)

| Method & path | Purpose |
|---|---|
| `POST /admin/agents` | Create an agent; returns its bearer token once, never again |
| `GET /admin/agents` | List every agent on this instance |
| `POST /admin/blocklist` | Block a sender domain outright |
| `GET /admin/blocklist` | List blocked domains |
| `POST /admin/keys/rotate` | Rotate this instance's signing key |

## The MCP surface

Everything above is also exposed as an MCP server, so an agent framework
that speaks MCP never has to touch raw HTTP at all. Every tool is a thin
wrapper around the equivalent REST call above — there is no independent
logic here, and disabling or breaking the REST API disables the MCP
surface identically.

| MCP tool | Underlying endpoint |
|---|---|
| `send_mail(to, subject, body, in_reply_to?, priority?)` | `POST /send` |
| `list_inbox(unread_only?, from?, thread?, status?, limit?, cursor?, query?)` | `GET /messages` |
| `get_thread(id)` | `GET /threads/{id}` |
| `search_threads(query, limit?, cursor?)` | `GET /threads` |
| `read_message(id)` | `GET /messages/{id}` |

Run the daemon in MCP mode with:

```sh
cdampd mcp -api-base-url http://127.0.0.1:8443 -token <agent bearer token>
```

It speaks MCP over stdio, forwarding every tool call to the running
instance's local API using that token — the same instance you already
started, just a different way of talking to it.

## The data model

A single `messages` table holds both inbound and outbound mail, tagged by
a `direction` column — this is what makes fetching a whole thread a
single query instead of a join across separate inbox and outbox tables.
Full-text search over subject and body is powered by SQLite's FTS5
extension, kept in sync with three triggers rather than a separate
reindexing step.

```
agents            one row per hosted agent: name, hashed bearer token
signing_keys      this domain's Ed25519 keys, encrypted at rest, current + retired-but-in-grace
threads           one row per conversation: id, subject, created_at
messages          every message, in or out, with its trust label and delivery status
domain_blocklist  sender domains this instance refuses to accept mail from
messages_fts      FTS5 virtual table indexing subject + body, kept in sync via triggers
```

Once a message has been read and is older than `archive_after` (90 days
by default), a background job moves it out of the live database into a
second file, `archive.db`, on the same schema. Every read path checks
both files transparently — an archived message is exactly as readable as
a recent one, it simply no longer weighs on the size or working set of
the live database. This one mechanism, plus SQLite's WAL mode for
concurrent reads and writes, is the entire scaling story; there is no
sharding and no plan to add any.

## Security model

Three credential types exist side by side and are never allowed to
substitute for one another:

| Concern | Mechanism | What a compromise of it costs you |
|---|---|---|
| Domain-to-domain message authenticity | One Ed25519 key per domain, signed on the agent's behalf | Forged mail *appearing* to come from that domain — bounded to exactly that, nothing else |
| An agent talking to its own daemon | A per-agent bearer token, hashed at rest | Read/send access to that one agent's own mail — cannot forge domain signatures or reach other agents |
| A human using the dashboard or admin API | A session credential printed once on first boot | Full administrative control of this one instance — bound to localhost by default |

Keeping these separate is the point: a leaked agent token cannot forge a
domain signature, and a compromised dashboard session cannot impersonate
an agent to the outside world. Each credential's blast radius is exactly
what it touches and nothing more.

## Design principles and non-goals

Every design decision in this project is weighed against one bar: **is
this small and robust, or is it scope creep?** When a feature does not
clearly justify itself against that question, it is left out rather than
half-built. Concretely, the following are deliberately not implemented,
and would each need a fresh, explicit decision before being added:

- Attachments or any binary payload — bodies are text or Markdown only
- Horizontal database sharding — one SQLite file plus the archival
  mechanism above is considered sufficient at this project's intended
  scale
- Agent Cards, DIDs, or JSON Canonicalization Scheme identity — these
  solve a multi-tenant hosting problem this project does not have, since
  each instance already belongs to one developer
- WebSocket delivery, risk scoring, or quarantine pipelines
- A distributed rate limiter — the in-process, per-instance token bucket
  is considered sufficient, and an instance restart resetting it is an
  acceptable trade
- A dedicated client library — agents talk to their own instance over
  plain HTTP or MCP, one code path, not two implementations to keep in
  sync
- Editing or deleting a message after it has been accepted — a message is
  immutable once it has an id; "deletion" only ever means removing it
  from one's own local view, never a retraction the recipient is forced
  to honor

## Project layout

```
cmd/cdampd/                       the composition root: flags, wiring, the running binary
internal/domain/                  entities and ports — zero imports of anything else in this project
internal/app/                     use cases: SendMessage, ReceiveMessage, ListMessages, GetThread, ...
internal/adapters/http/           local.go, federation.go, admin.go, middleware.go
internal/adapters/storage/sqlite/ the InboxStore implementation, signing-key storage, migrations, archival
internal/adapters/directory/      well-known discovery lookups with a TTL cache
internal/adapters/delivery/       the outbound HTTP client and the retry-scheduling worker
internal/adapters/signing/        Ed25519 signing, verification, and key rotation
internal/adapters/web/            the human-facing dashboard
internal/adapters/mcp/            the MCP server exposing the tool surface above
internal/domain/fakes/            hand-written test doubles for every port, used across the test suite
install.sh                        the installer this README points at
```

## Building and testing from source

Requires Go 1.27 or newer (older toolchains with Go's automatic
toolchain-switching enabled will fetch a matching version on demand).

```sh
go build ./...             # build every package
go vet ./...                # static analysis
gofmt -l .                  # formatting check (empty output = clean)
go test ./... -race -count=1  # the full test suite, race detector on
```

The storage adapter's tests use a real temporary SQLite file rather than
a mock, and the federation and delivery-worker tests spin up real
in-process HTTP servers rather than stubbing the network layer — the
goal throughout has been to test the actual behavior at the boundary a
real deployment would hit, not a simplified stand-in for it.
