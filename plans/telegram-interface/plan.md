# Telegram Interface Implementation Plan

> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the Telegram transport as the second `runtime.Notifier` implementation: long-poll one allowlisted chat, forward raw text to the coordinator, and deliver Pi responses and all notice classes back as plain text.

**Smallest user-feedback slice:** Slice 1 delivers the outbound pipeline: the transport implemented as a `runtime.Notifier` renders all 12 outbound classes, chunks at 4096 UTF-16 code units, and delivers to a mock Bot API server with the design's retry, supersede, and hold rules.

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
| `resolve-worktree` | `prompt-required` | 2026-11-08 amendment | Confirmed the `maruina/telegram-interface` worktree and branch before edits |
| `feature-worktree` | `prompt-required` | 2026-11-08 amendment | Confirmed writes and the plan commit belong in the existing feature worktree, not `main` |
| `learning-lookup` | `prompt-required` | 2026-11-08 amendment | Re-ran with retry budget, restart notice, shutdown drain, mock server, and stub terms; 0 of 18 store sections matched |
| `learning-candidates` | `prompt-required` | Review findings changed the plan | Appended two candidates under `## Learning candidates` |
| `write` | `agent-selected` | 2026-11-08 amendment | Claim-precision and rejection-reasons rules on the amendment record and new task text |
| `resolve-worktree` | `prompt-required` | Task 7 plan path must map to its owning worktree | Post-PR-#11, moved the `maruina/telegram-interface` worktree branch onto the post-slice-1 `origin/main`; the trees were content-identical, so the move was history-only |
| `skill-loader` | `prompt-required` | Select execution skills before editing | Loaded `go-best-practices` (new Go files, testing and concurrency references read); `write` for the ledger prose; no Kubernetes, CLI, script, or Terraform triggers |
| `go-best-practices` | `skill-loader` | `poll.go` and the two test files created | Consumer-owned `handler` seam in the transport package; deterministic inter-poll observation through the mock server's block channel; at-least-once offset advanced only after a nil handler return; `-race`, `-shuffle=on`, `go fix -diff`, and gofmt clean before the commit |
| `write` | `agent-selected` | The plan ledger is a prose artifact | Applied to the Task 7 status notes |
| `go-best-practices` | `skill-loader` | 2026-11-11 Task 10 follow-up: typing indicator | The transport renders the new `runtime.NoticeWorking` as Telegram `sendChatAction typing` through a coalescing channel (capacity 1, excess drops, the next milestone re-pulses) drained by one transport-owned goroutine that exits with `Close`; it never enqueues a chat message, so the wording matrix is untouched; `sender` gained the `SendChatAction` method and the contract test pins pulse-to-wire with zero text deliveries |
| `go-best-practices` | `skill-loader` | 2026-11-11 prior-art study: typing refresh and failure hardening | ZeroClaw (`orchestrator/mod.rs` `spawn_scoped_typing_task`, `CHANNEL_TYPING_REFRESH_INTERVAL_SECS = 4`) and OpenClaw (`src/auto-reply/reply/typing.ts`, `typing-lifecycle.ts`, changelog 2026.2.26/2026.6.9) studied from source; adopted the scoped refresh (each pulse opens a scope that re-sends every 4s and idles after a 12s silent window, so the indicator survives long tool execution without an unbounded loop) and OpenClaw's consecutive-failure suppression (sendChatAction stops after 3 failures per scope, because its changelog pins repeated 401 loops as a Telegram abuse-enforcement and bot-deletion risk); timing lives in per-transport fields copied from package defaults so parallel tests shrink the cadence race-free |
| `go-best-practices` | `skill-loader` | 2026-11-10 Task 10 follow-up: coordinator logging improved | Added operation attribution (`operation started` with `input` prompt/steer-retry, `reserved command started` with the command name, `status query`) and agent-reply milestone logs (`agent started`, turn boundaries, compaction, auto-retry at info; per-`message_end` stays unlogged); four focused log tests; the full state-machine suite falsified a dropped `agent_settled` case in the first cut before commit |

## Planning alignment brief (confirmed 2026-11-08)

Confirmed decisions the user approved in chat:

- **Poll-loop ownership (decision 1).** Option B: the transport owns `getUpdates` via a small stdlib HTTP client; the library owns sends and decoded models. Rationale: the pinned library's `bot.Start` advances its internal offset before handlers run (at-most-once within a process, losing the design's chosen duplicate-rerun tradeoff) and retries `getUpdates` conflicts internally forever, which the design explicitly rejects. Evidence: `get_updates.go:63-104` in `github.com/go-telegram/bot@v1.27.0`.
- **Active `Status` wording (decision 2).** Rendered without `Elapsed` (`Working. Queue <PendingMessageCount>. Model <ModelID>.`), with idle and active discriminated on `State.IsStreaming` (pinned true mid-run by `internal/runtime/status_live_test.go`; deliberate limit: a run in its pre-streaming phase renders the idle wording). Rationale: `NoticeStatus` carries only `*rpc.State` (verified `coordinator_result.go:118`); the coordinator must not change; `Heartbeat` already carries elapsed while a run is active. Decorative deviation from the wording table, recorded here and pinned in render tests.
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
| Shutdown ordering: signal → poll loop stops → `Coordinator.Shutdown` → transport `Close` with a bounded drain (in-flight send plus the single-attempt `RestartNotice`) → exit 0 | `handleShutdown` emits `RestartNotice` before the loop closes (`coordinator.go:430`), so a delivery goroutine bound to the cancelled signal context can never deliver it; a transport-owned close deadline also bounds a blocked send inside the shutdown admission | Binding the delivery goroutine to the signal context — loses the restart notice and unbounds the exit |

### Implementation Constraints

- One third-party dependency only: `github.com/go-telegram/bot` pinned to `v1.27.0` in `go.mod` (no floating versions). This is the AGENTS.md amendment planned for slice 4; the constraint is still binding during implementation.
- The coordinator, its notice payloads, and the shutdown sequence must not change. The transport classifies nothing, parses no session files, and queues no Pi work.
- Never log or send message content outside allowed paths: unknown sender content is never echoed, `Dropped` (`rpc.QueueContents` steering and follow-up texts) is counted with `len`, not rendered; exhaustion logs metadata only.
- The pinned Pi version and `TRYGALLE_PI_TESTS`/harness settings stay untouched; transport tests are pure unit and httptest, so the CI short job covers them.
- Deliberate simplifications carry `deliberate:` comments naming the limit and upgrade path: the held-queue cap, the retry budget and backoff constants, the stall threshold, and the ack-window (crash between `HandleUserInput` return and offset advance re-runs one update after restart).

### Security Requirements

- Credentials enter only through environment variables (`TRYGALLE_TELEGRAM_TOKEN`, `TRYGALLE_TELEGRAM_ALLOWED_USER`); diagnostics never print the token, chat ID, or message content. The library already redacts the token in its own debug/error strings.
- Allowlist enforcement happens before any processing, including before the canned reply is sent.
- Replies go only to the private chat of the allowlisted user: updates from group or channel chats are dropped before any reply, even when the allowlisted user sent them, so responses never leak to other chat members.
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
| Text over 4096 UTF-16 code units | Chunk at rune boundaries, preferring line breaks | Chunker tests |
| Unsupported media update | One canned fixed-string reply; media content unread | Poll test |
| Send/poll shutdown | Poll loop exits on context cancel; the delivery goroutine stops at the transport's bounded `Close`, which runs after `Coordinator.Shutdown` so the best-effort `RestartNotice` (emitted before the loop closes, `coordinator.go:430`) gets its single attempt; held messages are lost with the process (design-accepted) | Composition test |
| Coordinator death mid-poll | Poll loop stops on the first non-nil `HandleUserInput` return without advancing the offset; the un-acked update replays after restart; the process exits 1 | Poll test |

### Rollout and Rollback

- Rollout: implement and test locally against the mock Bot API server, then manual operator runs with a throwaway bot in a private chat (text round trip, busy semantics, `/abort` mid-run, `/new`, downtime replay, unsolicited photo canned reply). `cmd/trygalle` is the new runnable surface; the REPL remains the dev surface.
- Rollback: the transport is additive behind the `Notifier` seam; reverting the wiring in `cmd/trygalle` to the REPL-only surface (or reverting the slice commits) restores the previous behavior. No data migration exists because the transport holds no durable state.
- Owner: Matteo.

### Acceptance requirements

### Requirement: Inbound forwarding with allowlist
The transport SHALL forward the exact text of every message from the allowlisted numeric user in a private chat to the coordinator's `HandleUserInput` in `update_id` order, and SHALL accept further updates while a run is active (`HandleUserInput` returns after RPC acceptance).

#### Scenario: Text from the allowed user
- GIVEN a mock Bot API returning one text message from the allowed user ID
- WHEN the transport polls it
- THEN the coordinator stub receives the exact text once and exactly this sequence

#### Scenario: Message from another user (negative)
- GIVEN a message from a numeric user ID outside the allowlist
- WHEN the transport polls it
- THEN no handler call, no outbound send, and no content-bearing log record occurs; only a rejection count is logged

#### Scenario: Allowed user in a group chat (negative)
- GIVEN a message from the allowlisted user ID arriving in a group chat
- WHEN the transport polls it
- THEN no handler call, no outbound send, and only a rejection count is logged

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
The transport SHALL advance the in-memory offset after `HandleUserInput` returns nil, so a crash between handoff and acknowledgement re-runs at most the most recent update after restart. A non-nil return (context cancellation or coordinator death; routing failures ack nil) SHALL stop the poll loop without advancing the offset, so the un-acked update replays after the restart.

#### Scenario: Confirmation window
- GIVEN an update that was handed to the handler but not yet acknowledged, followed by process death
- WHEN the process restarts against the same token state
- THEN the same update is polled again (at-least-once), duplicated at most once per crash window

### Requirement: Outbound render boundary
The transport SHALL produce the final outbound text for every class through one render function: Pi responses as received, and each of the 11 notice kinds using the pinned wording table with payload substitution.

#### Scenario: Notice wording matrix
- GIVEN the 11 notice kinds with populated payloads (including `Status` idle and active variants, both `AbortReport` variants, all three `NewSessionReport` variants, and `RestartNotice` with empty `Text`)
- WHEN the transport renders them
- THEN the output equals the pinned strings, and a `Dropped` payload contributes only a count (`len(Steering)+len(FollowUp)`), never its texts

#### Scenario: Response renders unchanged
- GIVEN a Pi response containing markup characters such as backticks
- WHEN rendered without `parse_mode`
- THEN the text is sent as received, with no escaping state

### Requirement: Message length chunking
The transport SHALL split outbound text over 4096 UTF-16 code units (Telegram's limit; astral-plane characters count as two) at rune boundaries (never splitting a multi-byte character), preferring line breaks, and SHALL send each chunk as one message.

#### Scenario: Long multi-byte response
- GIVEN a Pi response longer than 4096 UTF-16 code units with multi-byte characters around the boundary
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
`cmd/trygalle` SHALL validate env configuration fatally before starting, run the `runtime.Start` ladder, construct the coordinator with the transport, run the poll loop, and on SIGINT/SIGTERM stop the poll loop, run `Coordinator.Shutdown`, then close the transport under a bounded drain window that covers the in-flight send and the single-attempt `RestartNotice`, exiting 0 within the shutdown admission; `Notice`/`Response` calls after transport close are safe drops.

#### Scenario: Missing token
- GIVEN no `TRYGALLE_TELEGRAM_TOKEN` and valid pi arguments
- WHEN the binary starts
- THEN it prints a non-secret hint to stderr and exits 1

#### Scenario: Clean signal exit
- GIVEN a run against a mock pi and mock Bot API
- WHEN SIGTERM arrives
- THEN the poll loop stops, `Coordinator.Shutdown` bounds the exit, the transport's close delivers the restart notice's single attempt within the drain window, and the process exits 0

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
| Outbound wire format and failure classification (the real library end-to-end) | Mock Bot API server hosting the real `*bot.Bot` built with `WithServerURL` and `WithSkipGetMe` (both verified options in v1.27.0) | Integration tests through the library path: the wire format, plus 429-with-`retry_after`, 400, and 5xx responses asserting the transient/permanent classes and `retry_after` extraction — the library wraps errors with `%w` and typed sentinels (`raw_request.go`), so `errors.As` unwrapping is the production path under test |

Mock limitations: the mock server cannot reproduce Telegram's real update schema changes or auth edge subtleties; the manual throwaway-bot run covers those claims. Live Pi contract tests are gated as usual by `TRYGALLE_PI_TESTS` and are unaffected.

---

### Slice 1: Outbound pipeline (smallest user-feedback slice)
**Delivers:** the transport implemented as `runtime.Notifier`: rendering all 12 outbound classes, chunking, and delivering with retry/supersede/hold rules proven by package tests, including the real `*bot.Bot` against a mock Bot API server and the single-attempt restart notice.

### Task 1: Package scaffold and dependency pin
**Delivers:** `internal/telegram` compiles with the pinned dependency and the package constants (chunk limit, send retry budget and backoff cap, held-message cap, stall threshold).
**Blocked by:** None
**Traces to:** Dependency amendment decision confirmed in the alignment brief; Implementation Constraints
**Files:** `internal/telegram/telegram.go`, `go.mod`, `go.sum`

- [x] Add `github.com/go-telegram/bot v1.27.0` to `go.mod` and record `go.sum`.
- [x] Run `go list -m all`; expect exactly `github.com/maruina/trygalle` and `github.com/go-telegram/bot v1.27.0`.
- [x] Add package constants with `deliberate:` comments: `maxMessageUTF16CodeUnits = 4096` (Telegram counts the send limit in UTF-16 code units, not runes), `maxSendAttempts`, `sendBackoffCap`, `maxHeldTerminal = 50`, `pollStallFailures = 5`.
- [x] Run `go build ./...`; expect success.

### Task 2: Renderer with the pinned wording matrix
**Delivers:** one render function covering the response class and all 11 notice kinds, tested against the wording table.
**Blocked by:** Task 1
**Traces to:** Outbound render boundary
**Files:** `internal/telegram/render.go`, `internal/telegram/render_test.go`

- [x] Add `go test ./internal/telegram -run TestRenderNoticeWording` as the focused failing test (no renderer yet).
- [x] Implement the renderer: response passthrough; notice strings verbatim from the design table with these payload rules — `Status` idle uses `SessionID`/`MessageCount`/model with nil model as "unknown", and active discriminates on `State.IsStreaming` (decision 2); `Dropped` contributes `len(Steering)+len(FollowUp)` and never its texts; `RestartNotice` renders its fixed wording regardless of empty `Text`; `AbortReport`/`NewSessionReport` match the coordinator's exact marker strings (`no operation was active`, `session switch cancelled`) first, then discriminate dropped-vs-clean on `len(Steering)+len(FollowUp) > 0` — `Dropped` is a value struct that is always present (`rpc.QueueContents`, `internal/rpc/records.go:121`), so presence cannot discriminate.
- [x] Run the command; expect all 12 columns pinned green.
- [x] Commit with `feat: pin telegram outbound wording table`. Task 1's scaffold (`telegram.go`, `go.mod`, `go.sum`) rides in this commit because Task 1 defines no commit step.

### Execution note: Task 2 commit eae8e80 includes the Task 1 scaffold plus the pinned require; the `// indirect` marker clears when the library import lands in Task 4.

### Task 3: Chunker
**Delivers:** a 4096 UTF-16-code-unit chunker preferring line boundaries, splitting only at rune boundaries.
**Blocked by:** Task 1
**Traces to:** Text over 4096 UTF-16 code units
**Files:** `internal/telegram/chunker.go`, `internal/telegram/chunker_test.go`

- [x] Add `go test ./internal/telegram -run TestChunk` as the focused failing test.
- [x] Implement; table tests cover: exactly 4096-unit output, over-limit with multi-byte characters (no split rune), astral-plane characters counting as two units (a message of 4096 emoji runes chunks, because Telegram would reject it with 400 and the permanent-drop class would lose the content), line-break preference over mid-line hard splits, trailing empty lines, the chunk count for a long Pi response, and delivery splitting at rune boundaries.
- [x] Run the command; expect green, with no byte-boundary split case. First run found the line-break preference re-splitting a remainder that already fit whole; fixed by preferring a break only when a hard cut is needed.
- [x] Commit with `feat: add telegram outbound chunker`.

### Task 4: Delivery loop with hold, supersede, and failure classes
**Delivers:** the single-owner delivery goroutine implementing the retry/ambiguity/exhaustion/supersede/cap rules behind the non-blocking `Notifier`.
**Blocked by:** Tasks 2, 3
**Traces to:** Non-blocking notifier, Retry-ambiguity-exhaustion policy, Order-preserving delivery with supersede
**Files:** `internal/telegram/delivery.go`, `internal/telegram/telegram.go`, `internal/telegram/config.go`, `internal/telegram/delivery_test.go`, `internal/telegram/notifier_contract_test.go`

- [x] Define the `sender` one-method interface (one `SendMessage(ctx, *bot.SendMessageParams) (*models.Message, error)` method) satisfied by `*bot.Bot`; assert with `var _ sender = (*bot.Bot)(nil)`.
- [x] Add the seam-focused failing tests: non-blocking `Notice` under a sender that blocks forever, FIFO order, supersede of held `Status`/`Heartbeat`, `retry_after` honored without budget use, transient-hold-then-deliver, timeout-drop, permanent-drop, exhaustion-drop, cap-eviction.
- [x] Implement: `Config` (token, allowed user, poll timeout) with validated constructor, mutex-protected bounded queue + signal channel, one delivery goroutine; `Response`/`Notice` enqueue-only; `RestartNotice` bypasses holds with a single attempt; metadata-only logging of retries/exhaustion/eviction.
- [x] Run `go test ./internal/telegram -race`; expect green.
- [x] Run `go test ./internal/runtime ./internal/telegram -race`; expect green (the runtime seam does not change).
- [x] Commit with `feat: telegram outbound delivery pipeline`. Commit e66c8ec; `go fix -diff ./...` runs clean and the pinned `// indirect` marker cleared as the library import landed.

### Slice 1 execution notes

- Supersede also covers the in-flight head: a `.supersede` enqueue marks an undelivered in-flight same-class message dead via its pointer, so the test scenario "older `Status` never sent" holds even when the head popped before the newer `Status` arrived. A send already on the wire can still complete (unfixable at that point); the deliberate limit is noted in `delivery.go`.
- Transient failures keep the message as the in-flight head instead of re-queuing, so headship is stable across backoff waits and supersede is observable; a `.dead` check runs at the top of every delivery loop iteration.
- `Retry-after` waits never increment `attempts` (budget untouched); infinite-flood retry is bounded only by `retry_after` — deliberate, noted at the flood case.
- HTTP 5xx classify as proven transient via a typed `telegramServerError` injected by a RoundTripper (the serverStatusTransport), because the library's default branch wraps such responses in an unclassified error string. The typed error carries the status code only; the response body is discarded.
- The default classifier outcome is ambiguous-drop, honoring the design's duplicate guard for timeout, decode, and unknown errors.
- `Close` marks `closed` and `stopped` together: enqueue becomes a safe drop, held messages are discarded (logged once as a count), and once-messages (the restart notice) drain with a single attempt while the retry context remains alive; the retry context cancels after the goroutine exits or the Close window ends.

### Execution (skills loaded and used during implementation)

| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `resolve-worktree` | `prompt-required` | Plan path from another worktree | Resolved the plan to the `maruina/telegram-interface` worktree and switched context; all commands ran there |
| `skill-loader` | `prompt-required` | Execution template requires it before editing | Routed skills by affected files; recorded this provenance record |
| `feature-worktree` | `prompt-required` | Writes must not land on `main` | Confirmed the existing `maruina/telegram-interface` worktree as the write target (branch `maruina/telegram-interface`) |
| `go-best-practices` | `skill-loader` | `.go` files created in `internal/telegram` | Goroutine lifetime and stop signals, mutex discipline, `errors.Is`/`errors.As` classification, goroutine-leak hygiene via stub cleanup, interface satisfaction assertions, table tests |
| `write` | `agent-selected` | Plan ledger precision | Applied claim-vs-hedge and rejection-reasons-first rules on execution notes |
| `codebase-research` | `agent-selected` | Render/delivery correctness depends on the pinned seam | Verified `Notice` payload fields, coordinator marker strings (`internal/runtime/coordinator_result.go`), `HandleUserInput` return semantics, and the library's error wrapping at the send boundary |

Tasks 5–6 execution (2026-11-08 run):| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `resolve-worktree` | `prompt-required` | Plan path from another worktree | Resolved to the `maruina/telegram-interface` worktree; all commands ran there |
| `skill-loader` | `prompt-required` | Execution template requires it before editing | Routed skills by affected files |
| `feature-worktree` | `prompt-required` | Writes must not land on `main` | Confirmed the existing `maruina/telegram-interface` worktree (clean tree, committed lifecycle docs) as the write target |
| `go-best-practices` | `skill-loader` | `.go` test and delivery files touched | Red-green discipline on Task 6, table tests, goroutine-safe server log, no new dependency, race runs as verification |
| `write` | `agent-selected` | Plan ledger precision | Claim-vs-hedge and evidence-first rules on the Task 5–6 execution notes |

### Amendment record (2026-11-08, from /verify on slice 1)

- Verification found the Test Strategy row "Outbound wire format and failure classification (the real library end-to-end)" with no owning task: no test used `httptest`, `bot.New`, or `WithServerURL`. Task 5 now owns that row.
- Verification found the pinned single-attempt claim unimplemented: `deliver()` has no `once`-specific handling, so `outcomeRetry`/`outcomeFlood` retried the restart notice within the send budget, and `TestCloseDeliversRestartNoticeAndDropsHeld` used a stub that succeeds on the first call, so no test observed the claim. Task 6 makes the code match the wording pinned in Task 4, the Key Decisions table, and the *Process composition and shutdown* scenario.
- Previously listed Tasks 5–9, all unexecuted, shifted to 7–11; their content and blocked-by chains are otherwise changed only by renumbering. Executed Tasks 1–4 keep their numbers and evidence.

### Task 5: Outbound wire format and failure classes through the real library
**Delivers:** integration tests through the real `*bot.Bot` against a mock Bot API server, pinning the outbound wire format and the 429-with-`retry_after`, 400/403/404, and 5xx classifications through `errors.As` on the library's wrapped errors.
**Blocked by:** Task 4
**Traces to:** Retry-ambiguity-exhaustion policy (HTTP 5xx clause); Test Strategy row "Outbound wire format and failure classification (the real library end-to-end)"
**Files:** `internal/telegram/outbound_test.go`

- [x] Build the test helper: an `httptest` mock Bot API server plus a real `*bot.Bot` constructed with `WithServerURL` and `WithSkipGetMe` (both verified options in v1.27.0), stored as the `sender`.
- [x] Add the focused failing test asserting the wire format (form fields carrying `chat_id` set to the allowed user and the exact text, with no `parse_mode` field) and the failure classes: 5xx retries as proven transient until the server accepts, delivering exactly once; 429 with `retry_after` waits and retries without consuming the send-retry budget; 400, 403, and 404 drop permanently with no retry; classification unwraps the library's errors with `errors.As` (`raw_request.go` wraps with `%w`; 429 returns the typed `retry_after` sentinel).
- [x] Run `go test ./internal/telegram -race`; expect green, with `New`, `serverStatusTransport`, and `telegramServerError` covered through the real constructor (production `telegram.go` wires `serverStatusTransport` into `New`).
- [x] Commit with `test: telegram outbound wire format and failure classes through the library`. Commit 9d70cfc.

Task 5 execution notes: the helper builds `bot.New` with the same wiring production `New` uses (an `http.Client` carrying `serverStatusTransport`, `WithSkipGetMe`), so `serverStatusTransport` and `telegramServerError` are exercised through the real constructor path; `New` itself cannot accept a server URL, so production-only injection is composition-level (Task 9's `New` call) and the wiring equality is by construction here. The delivery pipeline already implemented every class in Task 4, so the library-path tests pinned green on re-run — their value is that the suite can now false on a wire or classification regression; the only red run came from a wrong backoff expectation in the new test itself (1s then 2s, per `sendBackoff`), not from production code. Verified against the library source: 429 is returned as the direct `*bot.TooManyRequestsError` (typed field, no wrap), and 400/403/404 wrap `ErrorBadRequest`/`ErrorForbidden`/`ErrorNotFound` sentinels with `%w`. The mock server logs parsed multipart fields and a success-answer count, so "delivered exactly once" reads off the server, not the transport.

### Task 6: Single-attempt restart notice
**Delivers:** `deliver()` gives once-messages exactly one send attempt; a failed attempt drops the notice with a metadata-only log and never resends, matching the Task 4 wording and the *Process composition and shutdown* scenario.
**Blocked by:** Task 4
**Traces to:** Process composition and shutdown ("single-attempt `RestartNotice`"); Key Decisions "RestartNotice bypasses the hold queue (single enqueue, never held)"
**Files:** `internal/telegram/delivery.go`, `internal/telegram/delivery_test.go`

- [x] Add the focused failing test: extend the close-drain coverage with a stub whose first attempt fails with a proven transient error (connection reset); expect exactly one call, no delivery, and no content in the log. First run red as required: the pre-fix stub saw 5 calls (full budget burn) and no once-drop record.
- [x] Implement the smallest change: `deliver()` treats `outcomeRetry` and `outcomeFlood` for a `msg.once` message as a terminal drop with metadata-only logging; backoff and flood-wait behavior is unchanged for every other class.
- [x] Run `go test ./internal/telegram -race` and `go test ./internal/runtime ./internal/telegram -race`; expect green.
- [x] Commit with `fix: make the telegram restart notice single attempt`. Commit b26e62a.

Slice 1 execution notes for Tasks 5–6: both tasks verified clean after implementation, `gofmt -l ./cmd ./internal` empty and `go vet ./...` clean. The two once-drop branches share the same log wording; a helper was declined because each branch sits inline in the outcome switch and stays three lines each.

Slice 1 follow-up (2026-11-08, from /verify finding F1): the retry record (`telegram send retry`, `delivery.go`) joins the observed logs — `TestTransientFailureHoldsThenDelivers` now constructs the transport with a captured logger and asserts three `send retry` records for the three retried attempts plus no message content in the captured records. The red run removed the log line and the new count assertion failed with 0 records; no production change was needed.

F1 follow-up execution (2026-11-08 run):| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `resolve-worktree` | `prompt-required` | Follow-up names the worktree explicitly | Confirmed path, branch `maruina/telegram-interface`, HEAD `eda6be40`, and a clean tree before editing |
| `skill-loader` | `prompt-required` | Execution template requires it before editing | Routed skills by affected files (`.go` test only; no Kubernetes, `.sh`, Terraform, CLI, or docs changes) |
| `feature-worktree` | `prompt-required` | Writes must not land on `main` | Continued in the existing feature worktree, base checkout stays on `main` |
| `go-best-practices` | `skill-loader` | `.go` test file touched | Red-green on the focused test; race, shuffle, and repeat runs as verification |
| `write` | `agent-selected` | Plan ledger precision | Claim-vs-hedge and evidence rules on the follow-up note above |

### Slice 2: Inbound pipeline
**Delivers:** poll loop over the mock Bot API with offset-0 replay, ack-after-handoff, allowlist, raw forwarding including reserved-command text, a canned reply for non-text updates, and the 409/401/429/stall failure classes.

### Task 7: Poll loop, offset discipline, and forwarding
**Delivers:** long-poll loop with an injectable server URL, offset advance after handoff, update-order forwarding, and allowlist enforcement.
**Blocked by:** Task 4 (transport type exists; the internal seam remains independent)
**Traces to:** Inbound forwarding with allowlist, Downtime replay from offset 0, Ack-after-handoff offset, Reserved command text passes through raw
**Files:** `internal/telegram/poll.go`, `internal/telegram/poll_test.go`, `internal/telegram/mockserver_test.go`

- [x] Implement the mock Bot API server helper (httptest with scripted `getUpdates` batches and a captured `sendMessage` log) and the `handler` one-method interface (`HandleUserInput(ctx, text) error`) satisfied by `*runtime.Coordinator`. Verified: `mockserver_test.go` gives `newPollServer` (each scripted entry answers one `getUpdates` call wrapped in the `ok` envelope; an optional per-call block channel lets a test observe the loop between two polls), update wire fixtures, `newBotAPI`, and the `handler` seam with `var _ handler = (*runtime.Coordinator)(nil)` in `poll.go`.
- [x] Add the mock-server-focused failing tests: offset-0 backlog replay, forwarding order, allowlist drop (no content in logs), non-private-chat drop, `/status`-text forwarding verbatim, the at-least-once window (the offset advances in memory after a nil `HandleUserInput` return), and the coordinator-death exit (a non-nil handler error stops the loop without advancing the offset; routing failures ack nil, so non-nil means context cancellation or coordinator death). Verified `poll_test.go`: `TestPollReplaysBacklogInOrder` (backlog order, `/status` verbatim, offsets `[0 4]`, one backlog-replay record, no content in logs), `TestPollDropsAllowlistViolationsWithoutContent` (stranger, group, and stranger-voice drops — including no canned reply for the stranger's voice note, pinning allowlist-before-reply at user-identity and `chat.type` level — with three rejection records and offsets `[0 5]`), `TestPollAnswersNonTextUpdatesWithOneCannedReply` (voice note and callback query each get one canned reply to the allowed chat; handler receives nothing), and `TestPollHandlerErrorStopsLoopWithoutAdvancing` (sentinel error propagates with `errors.Is`, only the first update forwarded, final requested offset stays `0`). Red run: build failure on the new seam, then one server-side bug (the mock answered a batch outside the `ok` envelope) before implementation.
- [x] Implement: stdlib JSON `getUpdates` client (client timeout = poll timeout + buffer, `allowed_updates` unset, no `drop_pending_updates`), updates decode into `models.Update`, allowlist check before processing with replies restricted to `chat.Type == "private"` (group and channel chats drop with a rejection count), offset in memory advanced only after a nil `HandleUserInput` return — a non-nil return stops the loop and propagates on the done channel so `cmd/trygalle` exits 1 and the restart replays the un-acked update — and non-text updates classified centrally to a canned fixed-string reply (sent once per update). Verified: `poll.go` implements `poller` with `url` injectable for tests and the production base `https://api.telegram.org`; `getUpdates` posted as urlencoded `offset`/`timeout`; malformed or `ok: false` responses error with status and `error_code` context (failure classes are Task 8); non-message update kinds get the canned reply without a per-kind identity check, covered by a `deliberate:` comment pointing that identity work at the future media seam.
- [x] Run `go test ./internal/telegram -race`; expect green. Verified green ×3, plus the full module at `go test -race -shuffle=on -count=1 ./...`; `go vet ./...`, `gofmt -l ./cmd ./internal`, and `go fix -diff ./...` clean (`maps.Copy` modernization applied).
- [x] Commit with `feat: telegram inbound poll loop with allowlist`. Commit 43bf665.

Task 7 execution notes: deterministic between-poll observation displaced two naive designs — blocking a `getUpdates` response at the wrong call index starved pending updates, and the offset assertion is meaningful only on the call after the batch completes, so tests pin that call against `offset = lastUpdateID + 1` instead of per-update values. The canned string avoids the wording matrix entirely (outside the notice taxonomy); `nonTextReply` is one unpinned constant asserted verbatim by test. Non-message kinds (`callback_query` and friends) carry no shared sender identity, so their canned replies are not allowlist-gated in this slice; the limit is recorded in code as `deliberate:` and is the natural input to the future media seam's identity work.

### Task 8: Transport-level connection-failure classes
**Delivers:** the design's poll-failure taxonomy is proven against the mock server.
**Blocked by:** Task 7
**Traces to:** Loud configuration and conflict failures, Polling-stall visibility, 429 handling
**Files:** `internal/telegram/poll.go`, `internal/telegram/poll_test.go`

- [x] Add the failing mock-server tests for 409 exit (no further poll), persistent 401 exit, `retry_after` wait-then-repoll, and the stall warning at the 5-failure threshold. Verified `poll_test.go`: `TestPollConflictExitsWithoutFurtherPoll` (typed exit, attribute `code` 409, offsets `[0]`, one `poll configuration conflict` record), `TestPollUnauthorizedIsFatal` (five scripted 401s; the loop still exits on the first, typed 401), `TestPollBackpressureWaitsAndRepolls` (one injected wait of exactly the response's 7s, the repoll requests offset 0 again so backpressure does not advance the offset, the post-backpressure update reaches the handler), and `TestPollStallWarnsOnceAtThreshold` (five 500s through the production `serverStatusTransport` path, exactly one `polling stall` record, five waits of `pollBackoff`, then the success poll delivers normally). Red run: build failure on the new types and injected wait seam, then two mock-schedule bugs — an error entry and a batch entry originally consumed the same schedule index, and the stall test blocked the success call itself — both test-side.
- [x] Implement the failure classes: typed wrap for 409/401 special done-channel exit; 429 wait from the response's `retry_after` parameter; consecutive-failure counter and single stall log; brief backoff elsewhere. Verified: `pollConflictError` (409/401, classified on the body's `error_code` because the production path's `serverStatusTransport` has already converted 5xx statuses to `telegramServerError`) and `pollFloodError` (429 with `parameters.retry_after`) are returned typed from `getUpdates` through `classifyPollError`; `Run` exits on the conflict class (Task 9's composition reads the type when wiring the done channel), waits the `retry_after` under the injected `wait` seam for the flood class without advancing the stall counter, and backs off `pollBackoff` (1s constant, `deliberate:`) with a stall warning logged once on the fifth consecutive recoverable failure; the counter resets on the next success. `newPoller` gained the injected `wait` parameter, defaulting to `defaultWait`.
- [x] Run `go test ./internal/telegram -race`; expect green. Verified green ×3, plus the full module at `go test -race -shuffle=on -count=1 ./...`; `go vet ./...`, `gofmt -l ./cmd ./internal`, and `go fix -diff ./...` clean (`errors.AsType` modernization applied).
- [x] Commit with `feat: telegram poll failure classes with loud conflict exit`. Commit 4a3d404.

Task 8 execution notes: the mock server's per-call schedule became the fix point twice — first the error-vs-batch double indexing, then the block placement, because blocking the call that carries the success batch starves the very update the assertion waits on. The stall test exercises the real client path (mock 500s pass through `serverStatusTransport`, which turns 5xx into `telegramServerError` at the `client.Do` layer), so the classification and the network-layer class are both held by one test each. `classifyPollError` intentionally trusts the body's `error_code` over the HTTP status: every non-5xx status in this path arrives inside an `ok:false` envelope.

### Slice 3: Process composition
**Delivers:** the runnable `cmd/trygalle` binary joining config, startup ladder, coordinator, transport, and shutdown with the exit-code contract.

### Task 9: Composition with env validation and signals
**Delivers:** `cmd/trygalle` starts, validates fatally, races the poll loop and the coordinator, and shuts down on signals.
**Blocked by:** Tasks 4, 8
**Traces to:** Process composition and shutdown
**Files:** `cmd/trygalle/main.go`, `cmd/trygalle/main_test.go`

- [x] Add composition-focused failing tests: missing `TRYGALLE_TELEGRAM_TOKEN` and missing `TRYGALLE_TELEGRAM_ALLOWED_USER` each exit 1 with a non-secret hint; non-numeric allowed-user exits 1; usage exit 2. Verified `cmd/trygalle/main_test.go` (in-package, like the REPL's tests, because a `package main` cannot be imported): `TestMissingTokenExitsFatalWithHint` (message names `TRYGALLE_TELEGRAM_TOKEN`, never secret material), `TestMissingAllowedUserExitsFatalWithHint`, `TestNonNumericAllowedUserExitsFatal`, `TestUsageExitsTwo` (missing `--session-dir` prints the usage text), `TestUnexpectedArgumentExitsUsage`; the fatal-vs-usage split is asserted through `errors.Is(err, errUsage)`. Red run: build failure on the new command seam before implementation.
- [x] Implement: flags mirroring the REPL (`--pi-bin`, `--session-dir`, `--no-continue`, `--heartbeat-interval` 5m, `--shutdown-timeout` 30s); env-only token/allowed-user/poll-timeout (default 30s); `runtime.Start` for pi RPC with trygalle's transport as the `Notifier` (enqueue-only until delivery runs); shutdown ordering — the signal stops the poll loop, `Coordinator.Shutdown(context.Background())` runs next, then the transport's bounded `Close` covers the in-flight send and the single-attempt `RestartNotice` before exit 0, with `Notice`/`Response` after close as safe drops; poll-loop fatal errors (409/401 and non-nil handler returns) propagated on the done channel; exit 0 for clean and signal exits, 1 for fatal runtime errors, 2 for usage. Verified: new `cmd/trygalle` mirrors the REPL's `command` shell (testable `command(ctx, argv, getenv, stdout, stderr)`, `errUsage` kind, exit-code switch in `main`) and env validation precedes any network or pi work; an invalid `TRYGALLE_TELEGRAM_POLL_TIMEOUT` also exits fatal; the `done channel` from the plan is realized structurally — `Transport.RunPoll` runs the unexported poller on the main goroutine and its return value *is* the done channel (nil = signal, error = fatal), so `cmd/trygalle` exits 1 by wrapping it; a poll-fatal returns immediately without the shutdown ordering (fail loud: `deliberate:` in the code; a restart replays into the same visible failure), and `Close` is bounded by `--shutdown-timeout`.
- [x] Run `go build ./...`, `go vet ./...`, `go test -race -short ./...`; expect all green. The transport tests are pure unit and httptest, so the CI short job covers them and no `TRYGALLE_PI_TESTS` change is needed. Verified: also the full module at `go test -race -shuffle=on -count=1 ./...`; `gofmt -l ./cmd ./internal` and `go fix -diff ./...` clean.
- [x] Commit with `feat: trygalle production target joining coordinator and telegram transport`. Commit 2dd3698.

Task 9 execution notes: the novel decision is the goroutine budget — the poll loop takes the main goroutine instead of the plan's done-channel channel wiring, because `Run` already blocks exactly where the channel fence would sit: its nil return is the signal, its error return is the fatal. `Close`'s bound reuses `--shutdown-timeout` rather than a new constant; the coordinator's own sequence is bounded by the same value, so the worst-case shutdown stretches to twice the flag in the blocked-send corner — recorded as acceptable until the operation-evidence says otherwise. The signal context deliberately does *not* drive `runtime.Start` or `Shutdown`; both run under `context.Background()` so a SIGINT mid-ladder cannot leak a half-started pi child.

### Task 10: Manual end-to-end against a throwaway bot
**Delivers:** design's manual acceptance run recorded.
**Blocked by:** Task 9
**Traces to:** Rollout and Rollback (operator acceptance), all behavior requirements at a real boundary
**Files:** none (operator run)

- [x] Run with a throwaway bot token and private chat: text round trip; steering during a run (a second text merges into the run's single response); `/status` mid-run; `/abort` mid-run; `/new`; downtime replay after a stop-start; an unsolicited photo replying with the canned string; kill-and-restart during a run to observe the ack window's duplicate re-run. Verified 2026-11-11 on the local machine against the real Bot API and the pinned pi 1.5.2, driven by the operator (sender) with the bridge process and log timeline driven from the worktree (log timestamps below, times CET):
  - *Text round trip* — 13:51:47 `operation started` → 13:51:52 settled; the response arrived in the chat.
  - *Steering mid-run* — the 13:52:50 story run absorbed a second text as queued steering: four turn cycles, no second `operation started`, one settle at 13:53:52.
  - *`/status` mid-run* — `status query` at 13:53:xx; chat reply `Working. Queue 0. Model glm-5.3-flash.` matches the pinned wording.
  - *`/abort` mid-run* — `reserved command started command=/abort` → `operation aborted`; chat reply `Aborted. Dropped 0 queued inputs.`
  - *`/new` mid-run* — twice (14:11:37 and 14:12:22): `operation settled during a reserved command; outcome deferred` → `discarding the outcome replaced by a reserved command` → `session superseded`; chat reply `New session started.` is the single terminal outcome.
  - *Downtime replay* — three `backlog replay` events (13:51:46, 14:09:39, 14:30:22), each replaying updates that arrived while no poller held the connection.
  - *Unsolicited photo* — exactly one canned reply (`nonTextReply` wording) at 14:10; success is log-silent by design.
  - *Kill-and-restart ack window* — two attempts. A SIGKILL ~20 s after handling replayed nothing: the next poll had already confirmed the update server-side, empirically bounding the window at one round trip. The second attempt raced the handler return with a log watcher: `operation started` at 14:39:03.130, SIGKILL at 14:39:03.150 — inside the designed window — and the update still never replayed: the confirming poll outran the kill on the keep-alive connection. Conclusion recorded for the design: the at-least-once window is real but effectively sub-20 ms; the offset-0 replay mechanism itself is separately proven by the three downtime replays, and the handler-error-stops-without-advancing behavior is pinned by the Task 7 unit test.
  - Incidental live evidence: a restart overlapping a graceful shutdown produced a real 409, and the conflicted poller exited 1 with `poll configuration conflict` (Task 8's fail-loud semantics at the real boundary).
- [x] Record evidence in the slice's pull request or completion report; no token, chat ID, or message content may be committed. Verified: the evidence above carries timestamps and log lines only; the bot token lives in a 0600 env file outside the repository and never entered chat or logs; chat identifiers and message texts are excluded; the bridge-side user id appears only as the config value the tests already pin.

### Slice 4: Documentation and final verification
**Delivers:** repository docs carry the dependency amendment and runnable surface; full verification green.

### Task 11: Documentation updates
**Delivers:** AGENTS.md amendment and README runnable-surface update.
**Blocked by:** Task 10
**Traces to:** Documentation impact and AGENTS.md maintenance rule
**Files:** `AGENTS.md`, `README.md`

- [x] AGENTS.md: layout table rows for `internal/telegram` and `cmd/trygalle`; amend the stdlib-only paragraph to permit exactly the pinned `github.com/go-telegram/bot` dependency; add the trap that the library's only polling surface advances offsets before handlers run, so the transport-owned poll loop is load-bearing, not incidental. Verified: the stdlib bullet now scopes stdlib-only to `internal/rpc`, `internal/runtime`, and `internal/harness`, pins `go-telegram/bot` to `internal/telegram`, and states that pin changes move plan records and `go.mod` in one change; the new trap bullet names `bot.Start`'s handler-run-before-offset-advance and conflict-as-retryable behavior against the transport-owned loop.
- [x] README.md: add the Telegram quickstart (required env vars, flags, throwaway-bot setup steps, secrecy rule). Keep wording aligned to the design's operator intent; remove any section that duplicates AGENTS.md build commands. Verified: the quickstart is a how-to (BotFather, private chat, numeric user id, the two required env vars, `go run ./cmd/trygalle`); the secrecy rule sits on the token step; the old Development command block that duplicated AGENTS.md is gone, leaving only the pointer.
- [x] Run every command in AGENTS.md's Build and test section; expect the outcomes it documents, with `go fix -diff ./...` printing nothing. Verified: `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...`, `go test -race -short ./...` green; `go fix -diff ./...` printed nothing; `gofmt -l ./cmd ./internal` empty.

Task 11 execution note: Task 11's plan ordering waits on Task 10's operator run, and the run is still pending — the docs describe the committed runnable surface, so they were drafted ahead of the acceptance evidence, and the feature-level acceptance row (Task 10's recorded checklist) stays open in the final-verification section until the run happens. No content depends on the run's outcome.

### Final verification
- [x] `go build ./...`, `go vet ./...`, `gofmt -l ./cmd ./internal`, `go fix -diff ./...`, `go test -race -shuffle=on ./...` all green. Verified 2026-11-11 after the typing-refresh commit: module-wide green incl. live tier on the pinned pi 1.5.2; `go vet` and `go fix -diff` clean; `gofmt -l ./cmd ./internal` empty.
- [x] `go list -m all` still shows exactly one third-party module, pinned. Verified: `github.com/go-telegram/bot v1.27.0` and the module root only.
- [x] Feature-level acceptance: the full wording matrix and mock-server lifecycle tests green, the single third-party dependency still pinned, and the manual throwaway-bot checklist from Task 10 recorded. Verified: Task 10's checklist is recorded above with timestamps; the wording matrix and poll lifecycle tests are green in the final suite state; the single dependency is pinned.

## Learning candidates

- 2026-11-08: A pinned library's only polling surface can invalidate a design's offset and conflict semantics read straight from the Bot API spec — always probe the library's poll loop before planning offset/failure behavior on that endpoint. Evidence: `github.com/go-telegram/bot@v1.27.0 get_updates.go:63-104` (offset advance in `atomic.StoreInt64(&b.lastUpdateID, upd.ID)` before queueing; conflict treated as retryable) and design "Polling lifecycle and inbound replay" in `plans/telegram-interface/design.md`.
- 2026-11-08: A Test Strategy row without an owning task ships no coverage — the outbound library-path row existed through all of slice 1, yet no task scheduled it, so wire format, 5xx classification, and `retry_after` extraction had zero tests behind a green suite. Evidence: plans/telegram-interface/plan.md Test Strategy table versus Tasks 1–4; /verify finding F1 (no `httptest`, `bot.New`, or `WithServerURL` on the outbound test path).
- 2026-11-08: A pinned prose claim no test observes can silently miss the code — three plan places pinned the restart notice at "single attempt", `deliver()` never special-cased `once`, and the close-drain test stub succeeded on the first call, so the suite could not falsify the claim. Evidence: internal/telegram/delivery.go `deliver()` retry branches versus plans/telegram-interface/plan.md Task 4; /verify finding N1.
