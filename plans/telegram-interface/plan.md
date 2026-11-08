# Telegram Interface Implementation Plan

> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the Telegram transport as the second `runtime.Notifier` implementation: long-poll one allowlisted chat, forward raw text to the coordinator, and deliver Pi responses and all notice classes back as plain text.

**Smallest user-feedback slice:** Slice 1 delivers the outbound pipeline: the transport implemented as a `runtime.Notifier` renders all 12 outbound classes, chunks at 4096 characters, and delivers to a mock Bot API server with the design's retry, supersede, and hold rules.

**Out of Scope:** From `plans/telegram-interface/design.md` non-goals: media input/output implementation (seam only, canned reply), HTML/MarkdownV2 dialect, durable outbound queue, `drop_pending_updates`, metrics/tracing/alerting (follow-up #5), any coordinator semantics, notice-payload, or shutdown-sequence change, webhooks, and the image/Kubernetes designs (follow-ups #3 and #4).

**Architecture:** Package `internal/telegram` implements `runtime.Notifier` with non-blocking enqueue behind one delivery goroutine (render → chunk → send with hold-FIFO, supersede, and typed failure classes), plus a transport-owned `getUpdates` poll loop (offset-0 replay, ack-after-handoff offset, allowlist, 409/401 fatal). `go-telegram/bot` v1.27.0 owns everything outbound (`SendMessage`, and future media); the inbound poll endpoint is a ~120-line stdlib HTTP client because the library's only polling surface advances offsets before handlers run and retries conflicts internally. `cmd/trygalle` composes env config → `runtime.Start` → `runtime.NewCoordinator(…, transport)` → poll loop → signal shutdown.

**Tech Stack:** Go 1.27, standard library, one pinned third-party dependency: `github.com/go-telegram/bot v1.27.0` (verified: zero transitive dependencies, `go.mod` targets Go 1.18).

---

## Skills loaded and used

| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `skill-loader` | `prompt-required` | Plan template requires it | Routed language and domain skills; confirmed no Kubernetes surface, so no k8s skills |
| `resolve-worktree` | `prompt-required` | Absolute design path supplied | Resolved to the `maruina/telegram-interface` worktree and switched context |
| `feature-worktree` | `prompt-required` | Writes must not land on `main` | Confirmed the existing feature worktree as the write target |
| `learning-lookup` | `prompt-required` | Advisory lookup before feasibility | Re-ran with retry/idempotency/shutdown terms; zero material matches; design's earlier 18-section zero-match scan stands |
| `codebase-research` | `agent-selected` | Correctness depends on implemented seam behavior | Verified `Notifier` seam and payloads, `HandleUserInput` single-caller semantics, notice call sites and contexts, `Shutdown`, REPL composition |
| `go-best-practices` | `skill-loader` | `.go` files will be created | Concurrency/cancellation, goroutine lifetime, error classification, test-seam guidance applied to the plan's components |
| `cli-best-practices` | `skill-loader` | New `cmd/trygalle` CLI | Flag env split, exit-code contract, no secrets in output |
| `write` | `agent-selected` | Durable plan prose | Applied concision, claim-vs-hedge, and rejection-reasons-first rules |

## Planning alignment brief (confirmed 2026-11-08)

Confirmed decisions the user approved in chat:

- **Poll-loop ownership (decision 1).** Option B: the transport owns `getUpdates` via a small stdlib HTTP client; the library owns sends and decoded models. Rationale: the pinned library's `bot.Start` advances its internal offset before handlers run (at-most-once within a process, losing the design's chosen duplicate-rerun tradeoff) and retries `getUpdates` conflicts internally forever, which the design explicitly rejects. Evidence: `get_updates.go:63-104` in `github.com/go-telegram/bot@v1.27.0`.
- **Active `Status` wording (decision 2).** Rendered without `Elapsed` (`Working. Queue <PendingMessageCount>. Model <ModelID>.`). Rationale: `NoticeStatus` carries only `*rpc.State` (verified `coordinator_result.go:118`); the coordinator must not change; `Heartbeat` already carries elapsed while a run is active. Decorative deviation from the wording table, recorded here and pinned in render tests.
- **Smaller recommendations:** constant cap on held terminal messages (oldest dropped, log-only), persistent HTTP 401 treated like 409 as a fatal configuration error, nil `Model` renders "unknown", one canned non-text string, 429 retries do not consume the send-retry budget, polling-stall diagnostic trips after 5 consecutive poll failures.
- **Slice split:** four slices (outbound pipeline, inbound polling, composition, docs and final verification).

## Implementation Contract

### Components Affected

| Component | Files | Responsibility | Verification |
|---|---|---|---|
| Outbound pipeline | `internal/telegram/telegram.go`, `render.go`, `chunker.go`, `delivery.go`, `config.go` | `runtime.Notifier` implementation: non-blocking enqueue, render boundary, chunker, single-owner send loop, hold/supersede rules | Contract and pipeline tests in the same package |
| Inbound pipeline | `internal/telegram/poll.go`, `telegram.go` | `getUpdates` long-poll loop, offset discipline, allowlist, non-text canned reply, 409/401/429/stall handling | Mock Bot API server tests |
| Process composition | `cmd/trygalle/main.go` | Env config, `runtime.Start` ladder, coordinator construction with the transport, signal shutdown, exit codes | Composition unit tests; manual run against a throwaway bot |
| Dependency amendment | `go.mod`, `go.sum` | Exactly one third-party dependency, pinned | `go list -m all` shows only the module root and `go-telegram/bot` |
| Documentation | `AGENTS.md`, `README.md` | Dependency-rule amendment, layout rows, runnable surface | Docs review |

### Key Decisions

| Decision | Rationale | Rejected alternative |
|---|---|---|
| Transport-owned `getUpdates` (Option B) | Preserves ack-after-handoff offset, fail-loud 409, and stall detection exactly as designed; library `models` types still decode updates so types stay single-sourced | Library `bot.Start` long polling — less code, but library-owned offset and silent conflict-forever contradict the design's visible-failure posture |
| Active `Status` rendered without elapsed | No elapsed source exists in the payload; coordinator changes are a non-goal; `Heartbeat` covers elapsed liveness | Transport-side "last forwarded input" timestamp — duplicates coordinator operation attribution |
| Non-blocking `Response`/`Notice` | The coordinator event-loop goroutine calls both synchronously (verified call sites); a blocking send would stall the only consumer of the client event channels | Synchronous send inside the seam call — stalls the coordinator during delivery |
| Library owns sends through a narrow `sender` seam | Design mandates the library for the wire surface and future media; the seam keeps the send loop testable against a stub without library behavior | Stdlib `sendMessage` calls — reopens the hand-rolled client the design rejected |
| Constant caps over configuration | Design: tuning knobs are package constants until beta evidence demands otherwise | Env-configurable retry/stall knobs — unreviewed configuration surface |
| `RestartNotice` bypasses the hold queue (single enqueue, never held) | Parent rule: best-effort within the shutdown deadline | Holding it with terminal classes — loses the best-effort guarantee in the shutdown window |

### Implementation Constraints

- One third-party dependency only: `github.com/go-telegram/bot` pinned to `v1.27.0` in `go.mod` (no floating versions). This is the AGENTS.md amendment planned for slice 4; the constraint is still binding during implementation.
- The coordinator, its notice payloads, and the shutdown sequence must not change. The transport classifies nothing, parses no session files, and queues no Pi work.
- Never log or send message content outside allowed paths: unknown sender content is never echoed, `Dropped` (`rpc.QueueContents` steering and follow-up texts) is counted with `len`, not rendered; exhaustion logs metadata only.
- The pinned Pi version and `TRYGALLE_PI_TESTS`/harness settings stay untouched; transport tests are pure unit and httptest, so the CI short job covers them.
- Deliberate simplifications carry `deliberate:` comments naming the limit and upgrade path: the held-queue cap, the retry budget and backoff constants, the stall threshold, and the ack-window (crash between `HandleUserInput` return and offset advance re-runs one update after restart).

### Security Requirements

- Credentials enter only through environment variables (`TRYGALLE_TELEGRAM_TOKEN`, `TRYGALLE_TELEGRAM_ALLOWED_USER`); diagnostics never print the token, chat ID, or message content. The library already redacts the token in its own debug/error strings.
- Allowlist enforcement happens before any processing, including before the canned reply is sent.
- Renders carry lifecycle metadata only; no notice embeds prompt bodies or Pi output beyond the coordinator-supplied diagnostic strings for `FailedRun`, `RoutingFailure`, and `SessionResumeFailed` (Pi-generated causes).
- Long polling is egress-only; nothing listens on the host.

### Observability Requirements

- Structured lifecycle logs through the injected `*slog.Logger`, metadata only: startup config validation result, poll-loop start/stop, offset and backlog replay extents (offset numbers, not content), allowlist rejection counts, delivery retries and exhaustion by message class, 409/401/429 occurrences, stall warnings, shutdown requests.
- No metrics, dashboards, or alerting — follow-up #5 owns that mapping; this plan only keeps the events log-friendly.

### Failure Modes to Handle

| Failure | Behavior | Verification |
|---|---|---|
| Token or allowed-user missing/invalid | Fatal startup error with a non-secret diagnostic | Composition unit test |
| 409 on `getUpdates` | Exit 1; the process does not poll again | Mock-server test |
| Persistent 401 on `getUpdates` | Same fatal path as 409 (configuration error) | Mock-server test |
| 429 with `retry_after` | Wait `retry_after`, then retry; does not consume the send-retry budget | Mock-server test |
| Repeated poll failures | After 5 consecutive failures, log a polling-stall warning and continue | Mock-server test |
| Send failure, proven transient (connection reset before response, 429, 5xx) | Backoff retry while the budget remains | Delivery test |
| Send failure, ambiguous (client timeout after the request was written, decode error, context cancellation) | Drop, never resend | Delivery test |
| Send failure, permanent (400/403/404) | Drop with log, no retry | Delivery test |
| Retry budget exhausted | Log metadata (class, size, attempts), drop content | Delivery test |
| Held queue over cap | Drop the oldest held terminal message with log | Delivery test |
| Text over 4096 characters | Chunk at rune boundaries, preferring line breaks | Chunker tests |
| Unsupported media update | One canned fixed-string reply; media content unread | Poll test |
| Send/poll shutdown | Poll loop exits on context cancel; held messages are lost with the process (design-accepted) | Composition test |

### Rollout and Rollback

- Rollout: implement and test locally against the mock Bot API server, then manual operator runs with a throwaway bot in a private chat (text round trip, busy semantics, `/abort` mid-run, `/new`, downtime replay, unsolicited photo canned reply). `cmd/trygalle` is the new runnable surface; the REPL remains the dev surface.
- Rollback: the transport is additive behind the `Notifier` seam; reverting the wiring in `cmd/trygalle` to the REPL-only surface (or reverting the slice commits) restores the previous behavior. No data migration exists because the transport holds no durable state.
- Owner: Matteo.

### Acceptance requirements

### Requirement: Inbound forwarding with allowlist
The transport SHALL forward the exact text of every message from the allowlisted numeric user to the coordinator's `HandleUserInput` in `update_id` order, and SHALL accept further updates while a run is active (`HandleUserInput` returns after RPC acceptance).

#### Scenario: Text from the allowed user
- GIVEN a mock Bot API returning one text message from the allowed user ID
- WHEN the transport polls it
- THEN the coordinator stub receives the exact text once and exactly this sequence

#### Scenario: Message from another user (negative)
- GIVEN a message from a numeric user ID outside the allowlist
- WHEN the transport polls it
- THEN no handler call, no outbound send, and no content-bearing log record occurs; only a rejection count is logged

#### Scenario: Reserved command text passes through raw
- GIVEN the text `/status` from the allowed user
- WHEN the transport polls it
- THEN the transport forwards `/status` verbatim with no classification of its own

### Requirement: Downtime replay from offset 0
The transport SHALL poll from update offset 0 at startup without `drop_pending_updates`, so Telegram's retained backlog replays in order after a restart.

#### Scenario: Backlog replay
- GIVEN the mock Bot API holds a backlog of two updates at process start
- WHEN the transport's first poll runs
- THEN both updates are forwarded in ascending `update_id` order before newer polling continues

### Requirement: Ack-after-handoff offset
The transport SHALL advance the in-memory offset after `HandleUserInput` returns, so a crash between handoff and acknowledgement re-runs at most the most recent update after restart.

#### Scenario: Confirmation window
- GIVEN an update that was handed to the handler but not yet acknowledged, followed by process death
- WHEN the process restarts against the same token state
- THEN the same update is polled again (at-least-once), duplicated at most once per crash window

### Requirement: Outbound render boundary
The transport SHALL produce the final outbound text for every class through one render function: Pi responses as received, and each of the 11 notice kinds using the pinned wording table with payload substitution.

#### Scenario: Notice wording matrix
- GIVEN the 11 notice kinds with populated payloads (including `Status` idle and active variants, the three `AbortReport` and `NewSessionReport` variants, and `RestartNotice` with empty `Text`)
- WHEN the transport renders them
- THEN the output equals the pinned strings, and a `Dropped` payload contributes only a count (`len(Steering)+len(FollowUp)`), never its texts

#### Scenario: Response renders unchanged
- GIVEN a Pi response containing markup characters such as backticks
- WHEN rendered without `parse_mode`
- THEN the text is sent as received, with no escaping state

### Requirement: Message length chunking
The transport SHALL split outbound text over 4096 characters at rune boundaries (never splitting a multi-byte character), preferring line breaks, and SHALL send each chunk as one message.

#### Scenario: Long multi-byte response
- GIVEN a Pi response longer than 4096 characters with multi-byte characters around the boundary
- WHEN the transport renders and delivers it
- THEN Telegram receives adjacent chunks without a broken rune, preferring line-break boundaries

### Requirement: Non-text replies
The transport SHALL answer media and other non-text updates with one canned fixed string, without reading or storing the media content.

#### Scenario: Unsolicited photo
- GIVEN a photo message from the allowed user arrives on the poll
- WHEN the transport processes the update
- THEN exactly one canned reply is sent and the photo bytes are never downloaded or logged

### Requirement: Non-blocking notifier
`Response` and `Notice` MUST return promptly regardless of delivery state, because the coordinator event loop calls them synchronously on its only goroutine.

#### Scenario: Slow network does not stall the coordinator
- GIVEN a sender that blocks indefinitely on the request
- WHEN the coordinator's loop goroutine calls `Notice`
- THEN the call returns before the send completes and the message is delivered later by the delivery goroutine

### Requirement: Order-preserving delivery with supersede
The delivery pipeline SHALL send messages FIFO in production order; a pending undelivered `Status` or `Heartbeat` SHALL replace an older held one of the same kind; held terminal messages SHALL be bounded by a constant cap with oldest-drop.

#### Scenario: Response after steer acknowledgement, with superseded status
- GIVEN a held `Status`, then `SteerAcknowledged`, then a response, and finally a newer `Status`, all enqueued while the sender is blocked
- WHEN the network recovers
- THEN the older `Status` is never sent, and the remaining messages are sent FIFO: `SteerAcknowledged`, response, newest `Status` once

### Requirement: Retry, ambiguity, and exhaustion policy
The transport SHALL retry sends only when the failure proves non-delivery (connection reset before the response, 429 with `retry_after`, HTTP 5xx), SHALL wait `retry_after` for 429 without consuming the retry budget, and SHALL drop ambiguous or permanent failures after logging metadata (class, size, attempts) without content.

#### Scenario: Transient holds
- GIVEN a send that fails with a connection reset
- WHEN the transport retries within budget until the mock server accepts
- THEN the message is delivered exactly once and the retries appear in logs as metadata

#### Scenario: Timeout drops (negative)
- GIVEN a send whose connection times out after the request was fully written
- WHEN the failure is observed
- THEN the message is dropped without a resend, because a dup message is the design hazard

### Requirement: Loud configuration and conflict failures
The transport SHALL exit non-zero (via its done channel) when `getUpdates` returns 409 (another poller) or a persistent 401 (invalid token), instead of retrying silently.

#### Scenario: Duplicate poller
- GIVEN the mock Bot API answers `getUpdates` with HTTP 409
- WHEN the poll loop observes it
- THEN the loop stops, the done channel carries the error, and no further poll is attempted

### Requirement: Polling-stall visibility
The transport SHALL log a stall diagnostic after a fixed number (5) of consecutive poll failures, detection only.

#### Scenario: Continuous poll errors
- GIVEN the mock Bot API returns network-level failures for six consecutive poll attempts
- WHEN the loop continues
- THEN a stall warning is logged once around the threshold, with class metadata only

### Requirement: Process composition and shutdown
`cmd/trygalle` SHALL validate env configuration fatally before starting, run the `runtime.Start` ladder, construct the coordinator with the transport, run the poll loop, and on SIGINT/SIGTERM execute `Coordinator.Shutdown` while the transport stops without emitting new notices, exiting 0 within the shutdown admission.

#### Scenario: Missing token
- GIVEN no `TRYGALLE_TELEGRAM_TOKEN` and valid pi arguments
- WHEN the binary starts
- THEN it prints a non-secret hint to stderr and exits 1

#### Scenario: Clean signal exit
- GIVEN a run against a mock pi and mock Bot API
- WHEN SIGTERM arrives
- THEN the poll loop stops, `Coordinator.Shutdown` bounds the exit, and the process exits 0

## Test Strategy

Every requirement maps to a seam; the mock Bot API server (httptest, configurable status codes and delayed bodies) is the real system boundary under test:

| Requirement | Seam | Mechanism |
|---|---|---|
| Render boundary, wording matrix | Transport implementing `runtime.Notifier` (existing seam; no new seam) | Table-driven contract driving the `Notifier` methods directly |
| Non-blocking seam | Transport type with a blocked sender stub | Goroutine-safe unit test asserting prompt return from a loop-like call site |
| Delivery pipeline | `sender` one-method interface satisfied by `*bot.Bot` (new seam; justified because the transport is the design-mandated single retry owner and determinism needs to stub it) | Stub sender returning status codes, `retry_after` values, and network errors |
| Chunking | Pure functions | Rune-boundary table tests |
| Non-text replies | Mock Bot API server | Poll integration test asserting exactly one canned send per non-text update |
| Poll lifecycle, allowlist, 409/401/429, stall, replay, ack window | Transport-owned `getUpdates` HTTP client pointed at the mock server (base URL injectable) | Integration through the real HTTP path |
| Composition and env validation | Process entry as a testable function over injected env map and stdio | Unit tests for validation and wiring; manual throwaway-bot run covers real end to end |
| Outbound wire format (the real library end-to-end) | Mock Bot API server hosting the real `*bot.Bot` built with `WithServerURL` and `WithSkipGetMe` (both verified options in v1.27.0) | One integration test through the library path |

Mock limitations: the mock server cannot reproduce Telegram's real update schema changes or auth edge subtleties; the manual throwaway-bot run covers those claims. Live Pi contract tests are gated as usual by `TRYGALLE_PI_TESTS` and are unaffected.

---

### Slice 1: Outbound pipeline (smallest user-feedback slice)
**Delivers:** the transport implemented as `runtime.Notifier`: rendering all 12 outbound classes, chunking, and delivering to a mock Bot API with retry/supersede/hold semantics proven by package tests.

### Task 1: Package scaffold and dependency pin
**Delivers:** `internal/telegram` compiles with the pinned dependency and the package constants (chunk limit, send retry budget and backoff cap, held-message cap, stall threshold).
**Blocked by:** None
**Traces to:** Dependency amendment decision confirmed in the alignment brief; Implementation Constraints
**Files:** `internal/telegram/telegram.go`, `go.mod`, `go.sum`

- [ ] Add `github.com/go-telegram/bot v1.27.0` to `go.mod` and record `go.sum`.
- [ ] Run `go list -m all`; expect exactly `github.com/maruina/trygalle` and `github.com/go-telegram/bot v1.27.0`.
- [ ] Add package constants with `deliberate:` comments: `maxMessageRunes = 4096`, `maxSendAttempts`, `sendBackoffCap`, `maxHeldTerminal = 50`, `pollStallFailures = 5`.
- [ ] Run `go build ./...`; expect success.

### Task 2: Renderer with the pinned wording matrix
**Delivers:** one render function covering the response class and all 11 notice kinds, tested against the wording table.
**Blocked by:** Task 1
**Traces to:** Outbound render boundary
**Files:** `internal/telegram/render.go`, `internal/telegram/render_test.go`

- [ ] Add `go test ./internal/telegram -run TestRenderNoticeWording` as the focused failing test (no renderer yet).
- [ ] Implement the renderer: response passthrough; notice strings verbatim from the design table with these payload rules — `Status` idle uses `SessionID`/`MessageCount`/model with nil model as "unknown"; `Status` active is "Working. Queue <PendingMessageCount>. Model <ModelID>." (decision 2); `Dropped` contributes `len(Steering)+len(FollowUp)` and never its texts; `RestartNotice` renders its fixed wording regardless of empty `Text`; `AbortReport`/`NewSessionReport` prefer the `Text`-marker variants (coordinator emits internal markers) then the `Dropped`-present variant.
- [ ] Run the command; expect all 12 columns pinned green.
- [ ] Commit with `feat: pin telegram outbound wording table`.

### Task 3: Chunker
**Delivers:** a 4096-character (rune) chunker preferring line boundaries, UTF-8-safe.
**Blocked by:** Task 1
**Traces to:** Text over 4096 characters
**Files:** `internal/telegram/chunker.go`, `internal/telegram/chunker_test.go`

- [ ] Add `go test ./internal/telegram -run TestChunk` as the focused failing test.
- [ ] Implement; table tests cover: exactly 4096-rune output, over-limit with multi-byte characters (no split rune), line-break preference over mid-line hard splits, trailing empty lines, the chunk count for a long Pi response, and delivery splitting at rune boundaries.
- [ ] Run the command; expect green, with no byte-boundary split case.
- [ ] Commit with `feat: add telegram outbound chunker`.

### Task 4: Delivery loop with hold, supersede, and failure classes
**Delivers:** the single-owner delivery goroutine implementing the retry/ambiguity/exhaustion/supersede/cap rules behind the non-blocking `Notifier`.
**Blocked by:** Tasks 2, 3
**Traces to:** Non-blocking notifier, Retry-ambiguity-exhaustion policy, Order-preserving delivery with supersede
**Files:** `internal/telegram/delivery.go`, `internal/telegram/telegram.go`, `internal/telegram/config.go`, `internal/telegram/delivery_test.go`, `internal/telegram/notifier_contract_test.go`

- [ ] Define the `sender` one-method interface (one `SendMessage(ctx, *bot.SendMessageParams) (*models.Message, error)` method) satisfied by `*bot.Bot`; assert with `var _ sender = (*bot.Bot)(nil)`.
- [ ] Add the seam-focused failing tests: non-blocking `Notice` under a sender that blocks forever, FIFO order, supersede of held `Status`/`Heartbeat`, `retry_after` honored without budget use, transient-hold-then-deliver, timeout-drop, permanent-drop, exhaustion-drop, cap-eviction.
- [ ] Implement: `Config` (token, allowed user, poll timeout) with validated constructor, mutex-protected bounded queue + signal channel, one delivery goroutine; `Response`/`Notice` enqueue-only; `RestartNotice` bypasses holds with a single attempt; metadata-only logging of retries/exhaustion/eviction.
- [ ] Run `go test ./internal/telegram -race`; expect green.
- [ ] Run `go test ./internal/runtime ./internal/telegram -race`; expect green (the runtime seam does not change).
- [ ] Commit with `feat: telegram outbound delivery pipeline`.

### Slice 2: Inbound pipeline
**Delivers:** poll loop over the mock Bot API with offset-0 replay, ack-after-handoff, allowlist, raw forwarding including reserved-command text, a canned reply for non-text updates, and the 409/401/429/stall failure classes.

### Task 5: Poll loop, offset discipline, and forwarding
**Delivers:** long-poll loop with an injectable server URL, offset advance after handoff, update-order forwarding, and allowlist enforcement.
**Blocked by:** Task 4 (transport type exists; the internal seam remains independent)
**Traces to:** Inbound forwarding with allowlist, Downtime replay from offset 0, Ack-after-handoff offset, Reserved command text passes through raw
**Files:** `internal/telegram/poll.go`, `internal/telegram/poll_test.go`, `internal/telegram/mockserver_test.go`

- [ ] Implement the mock Bot API server helper (httptest with scripted `getUpdates` batches and a captured `sendMessage` log) and the `handler` one-method interface (`HandleUserInput(ctx, text) error`) satisfied by `*runtime.Coordinator`.
- [ ] Add the mock-server-focused failing tests: offset-0 backlog replay, forwarding order, allowlist drop (no content in logs), `/status`-text forwarding verbatim, and the at-least-once window (the offset advances in memory after `HandleUserInput` returns).
- [ ] Implement: stdlib JSON `getUpdates` client (client timeout = poll timeout + buffer, `allowed_updates` unset, no `drop_pending_updates`), updates decode into `models.Update`, allowlist check before processing, offset in memory advanced post-ack, non-text updates classified centrally to a canned fixed-string reply (sent once per update).
- [ ] Run `go test ./internal/telegram -race`; expect green.
- [ ] Commit with `feat: telegram inbound poll loop with allowlist`.

### Task 6: Transport-level connection-failure classes
**Delivers:** the design's poll-failure taxonomy is proven against the mock server.
**Blocked by:** Task 5
**Traces to:** Loud configuration and conflict failures, Polling-stall visibility, 429 handling
**Files:** `internal/telegram/poll.go`, `internal/telegram/poll_test.go`

- [ ] Add the failing mock-server tests for 409 exit (no further poll), persistent 401 exit, `retry_after` wait-then-repoll, and the stall warning at the 5-failure threshold.
- [ ] Implement the failure classes: typed wrap for 409/401 special done-channel exit; 429 wait from the response's `retry_after` parameter; consecutive-failure counter and single stall log; brief backoff elsewhere.
- [ ] Run `go test ./internal/telegram -race`; expect green.
- [ ] Commit with `feat: telegram poll failure classes with loud conflict exit`.

### Slice 3: Process composition
**Delivers:** the runnable `cmd/trygalle` binary joining config, startup ladder, coordinator, transport, and shutdown with the exit-code contract.

### Task 7: Composition with env validation and signals
**Delivers:** `cmd/trygalle` starts, validates fatally, races the poll loop and the coordinator, and shuts down on signals.
**Blocked by:** Tasks 4, 6
**Traces to:** Process composition and shutdown
**Files:** `cmd/trygalle/main.go`, `cmd/trygalle/main_test.go`

- [ ] Add composition-focused failing tests: missing `TRYGALLE_TELEGRAM_TOKEN` and missing `TRYGALLE_TELEGRAM_ALLOWED_USER` each exit 1 with a non-secret hint; non-numeric allowed-user exits 1; usage exit 2.
- [ ] Implement: flags mirroring the REPL (`--pi-bin`, `--session-dir`, `--no-continue`, `--heartbeat-interval` 5m, `--shutdown-timeout` 30s); env-only token/allowed-user/poll-timeout (default 30s); `runtime.Start` for pi RPC with trygalle's transport as the `Notifier` (enqueue-only until delivery runs); `Coordinator.Shutdown(context.Background())` after the run loop ends; poll-loop fatal errors propagated on the done channel; exit 0 for clean and signal exits, 1 for fatal runtime errors, 2 for usage.
- [ ] Run `go build ./...`, `go vet ./...`, `go test -race -short ./...`; expect all green. The transport tests are pure unit and httptest, so the CI short job covers them and no `TRYGALLE_PI_TESTS` change is needed.
- [ ] Commit with `feat: trygalle production target joining coordinator and telegram transport`.

### Task 8: Manual end-to-end against a throwaway bot
**Delivers:** design's manual acceptance run recorded.
**Blocked by:** Task 7
**Traces to:** Rollout and Rollback (operator acceptance), all behavior requirements at a real boundary
**Files:** none (operator run)

- [ ] Run with a throwaway bot token and private chat: text round trip; steering during a run (a second text merges into the run's single response); `/status` mid-run; `/abort` mid-run; `/new`; downtime replay after a stop-start; an unsolicited photo replying with the canned string; kill-and-restart during a run to observe the ack window's duplicate re-run.
- [ ] Record evidence in the slice's pull request or completion report; no token, chat ID, or message content may be committed.

### Slice 4: Documentation and final verification
**Delivers:** repository docs carry the dependency amendment and runnable surface; full verification green.

### Task 9: Documentation updates
**Delivers:** AGENTS.md amendment and README runnable-surface update.
**Blocked by:** Task 8
**Traces to:** Documentation impact and AGENTS.md maintenance rule
**Files:** `AGENTS.md`, `README.md`

- [ ] AGENTS.md: layout table rows for `internal/telegram` and `cmd/trygalle`; amend the stdlib-only paragraph to permit exactly the pinned `github.com/go-telegram/bot` dependency; add the trap that the library's only polling surface advances offsets before handlers run, so the transport-owned poll loop is load-bearing, not incidental.
- [ ] README.md: add the Telegram quickstart (required env vars, flags, throwaway-bot setup steps, secrecy rule). Keep wording aligned to the design's operator intent; remove any section that duplicates AGENTS.md build commands.
- [ ] Run every command in AGENTS.md's Build and test section; expect the outcomes it documents, with `go fix -diff ./...` printing nothing.

### Final verification
- [ ] `go build ./...`, `go vet ./...`, `gofmt -l ./cmd ./internal`, `go fix -diff ./...`, `go test -race -shuffle=on ./...` all green.
- [ ] `go list -m all` still shows exactly one third-party module, pinned.
- [ ] Feature-level acceptance: the full wording matrix and mock-server lifecycle tests green, the single third-party dependency still pinned, and the manual throwaway-bot checklist from Task 8 recorded.

## Learning candidates

- 2026-11-08: A pinned library's only polling surface can invalidate a design's offset and conflict semantics read straight from the Bot API spec — always probe the library's poll loop before planning offset/failure behavior on that endpoint. Evidence: `github.com/go-telegram/bot@v1.27.0 get_updates.go:63-104` (offset advance in `atomic.StoreInt64(&b.lastUpdateID, upd.ID)` before queueing; conflict treated as retryable) and design "Polling lifecycle and inbound replay" in `plans/telegram-interface/design.md`.
