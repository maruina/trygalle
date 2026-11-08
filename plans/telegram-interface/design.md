# Telegram interface design

Status: Approved

Date: 2026-11-08

Scope: Detailed design of Trygalle's Telegram transport, follow-up #2 of the approved high-level design (`plans/high-level-design/design.md`). The Pi environment and image, Kubernetes deployment, and operability designs remain separate follow-ups.

## Summary

Trygalle's Telegram transport is the second implementation of the `runtime.Notifier` seam after the REPL's `stdoutNotifier`. It receives text messages from one allowlisted Telegram chat through long polling, forwards every non-reserved input to the runtime coordinator unchanged, and delivers Pi's responses and system notices back as plain Telegram text.

The transport owns Telegram adaptation only: polling lifecycle, offset discipline, the numeric allowlist, outbound delivery with bounded retry and hold, and rendering. Reserved-command classification, operation state, and completion detection stay in the coordinator. The transport never repeats a Pi operation, never parses Pi session files, and never queues Pi work.

The Go library `go-telegram/bot` is the transport's one permitted third-party dependency, as a pinned, transport-only dependency. This is the first amendment to the repository's standard-library-only constraint.

## Problem

The runtime design proved the coordinator contract against stdin and left Telegram-visible presentation open: message wording for the 11 notice kinds and the response, `/status` field completeness, delivery failure behavior, and the polling lifecycle. Each of those deferrals lands here. Before this component exists, Trygalle works only in a terminal; Telegram is the whole reason for the bridge.

## User and audience

Matteo is the sole Telegram user, operator, and maintainer, reaching the bridge from one phone and one chat. Secondary readers are reviewers of the public `trygalle` repository who need to see the delivery and replay rules without access to the private harness.

## Goals

- Forward text from one allowlisted numeric user ID into the coordinator and return Pi's final response.
- Drive long polling independently of any active Pi operation so `/abort` and `/status` stay reachable mid-run.
- Deliver every outbound message class with a single retry owner, bounded retry, and hold-for-reconnect for terminal classes.
- Replay Telegram's pending update backlog after downtime so nothing sent while the bridge was unavailable is silently lost.
- Render all 12 outbound classes through one boundary so the output dialect can change in one place.
- Detect and visibly report polling liveness failures, including the duplicate-poller conflict, instead of running as a silent zombie.
- Leave the inbound media adapter seam defined but unimplemented, matching the beta's text-only scope.

## Non-goals

- Media input or output handling in the beta (screenshots, voice notes, document uploads)
- Streaming edits, typing indicators, message reactions, or Telegram command menus as interactive surfaces
- Bridging Pi extension UI dialogs to Telegram interactions
- A command catalog or dynamic command mirroring
- Metrics, tracing, dashboards, or alerting (follow-up #5)
- Any change to coordinator semantics, notice payload structure, or the shutdown sequence
- A durable outbound queue or retry ledger across restarts
- Webhooks and externally reachable networking

## Context reviewed

The coordinator and notifier seam is implemented and pinned by contract tests:

- `internal/runtime/notices.go` defines `Notifier` (`Response(ctx, text)` and `Notice(ctx, Notice)`) and the closed set of 11 `NoticeKind` values with structured payloads: `Status` (State), `SteerAcknowledged`, `ExtensionHandled`, `EmptyResponse`, `FailedRun` (Text), `AbortReport` (Dropped), `NewSessionReport` (Dropped), `SessionResumeFailed` (Text), `Heartbeat` (Elapsed, LastActivity), `RoutingFailure` (Text), `RestartNotice` (Text).
- Neither `Notifier` method returns an error; delivery outcome is invisible upstream by construction. The coordinator's guarantee ends at producing an outcome; delivering it is transport-owned.
- `Coordinator.HandleUserInput` classifies `/new`, `/status`, and `/abort` internally; the transport passes raw text only. Because `HandleUserInput` returns after RPC acceptance (prompt accepted, steered, or rejected), the transport can loop over poll batches without waiting for a run to settle.
- `rpc.State` (`internal/rpc/records.go`) reports session ID and file, streaming and compaction flags, pending and total message counts, and the selected model — the data available for `/status` wording.
- REPL flags pin two constants this design inherits: heartbeat interval (default 5m) and shutdown timeout (default 30s).

Telegram Bot API behavior this design relies on:

- `getUpdates` long polling holds the request open up to `timeout` seconds and returns updates in ascending `update_id` order within a batch.
- Telegram deletes an update once a later poll requests an `offset` beyond it. The offset lives in the caller's memory; Telegram retains undelivered updates up to 24 hours.
- `drop_pending_updates` exists as a deliberate "discard the backlog" switch; not using it is a replay decision, not a default.
- Sends may be rejected with HTTP 429 plus `parameters.retry_after` (flood control) or HTTP 409 when another poller holds the same token.
- Text messages are limited to 4096 characters.

Advisory learning lookup: 18 sections of `Datadog/Learnings.md` scanned against telegram, long polling, bot API, Go library, and rate limit terms; zero matches, no material guidance (skipped source recorded).

Prior-art evidence used for the two contested behavioral decisions:

- **OpenClaw** (`docs.openclaw.ai/concepts/retry`, issues #53895 and #64235): retries sends only on 429 or provable non-delivery; never replays ambiguous sends; their durable queue plus a second retry owner produced duplicate sends until the queue became the single retry owner.
- **Hermes** (`NousResearch/hermes-agent` issue #2910): a Telegram send failure during reply delivery surfaced to the user as a silent hang; their fix retries transient send failures and notifies on exhaustion.
- **ZeroClaw** (`zeroclaw-labs/zeroclaw`, `crates/zeroclaw-channels/src/telegram.rs`): starts at offset 0 with no `drop_pending_updates`, so restart replays the backlog at-least-once; an acknowledgment queue owns the offset, transient failures hold it, and only permanent failures advance it.
- **Go library health** (checked 2026-11-08): `go-telegram/bot` v1.27.0, MIT, weekly activity, zero third-party dependencies in its `go.mod`; `go-telegram-bot-api` (tgbotapi) unreleased since v5.5.1, December 2021; `tucnak/telebot` unreleased since v3.3.6, June 2024.

## Skills loaded and used

| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `skill-loader` | `prompt-required` | Brainstorm template requires it | Routed domain skills for each stage; defines this provenance table |
| `learning-lookup` | `prompt-required` | Advisory guidance before design decisions | Read `Datadog/Learnings.md` via `learning-sections`; zero matches on the derived terms |
| `learning-opportunities` | `prompt-required` | Coaching default | Prediction-question pattern governed the decision sequence |
| `tavily-search` | `agent-selected` | OpenClaw and Hermes prior-art behavior | Sourced retry, queue, and replay evidence with links |
| `codebase-research` | `agent-selected` | Correctness depends on implemented seam behavior | Mapped `internal/runtime`, `internal/rpc`, and the REPL caller; verified notice payloads, `rpc.State`, and `HandleUserInput` return semantics |
| `feature-worktree` | `prompt-required` | Writes must not land on `main` | Created `maruina/telegram-interface` worktree from `origin/main`; all writes and the commit happen there |
| `write` | `agent-selected` | Durable public design prose | Applied concision, claim-vs-hedge, and rejection-reasons-first rules |

## Assumptions

- The bot token and the allowed numeric user ID are runtime secrets supplied by environment configuration; local runs use a throwaway bot and a private chat.
- One Telegram account is the only sender in the allowlisted chat; the single-replica StatefulSet removes concurrent pollers.
- Telegram's 4096-character limit counts characters; Go string slicing on byte boundaries can split a multi-byte character, so splitter tests must force UTF-8 boundaries.
- The production `cmd/trygalle` binary composition (runtime `Start` ladder, coordinator, transport) belongs to this follow-up; image and Kubernetes artifacts remain follow-ups #3 and #4.
- Pi RPC does not accept inline media attachments in the pinned version; the future media seam writes files to a Pi-readable directory and references the path in the prompt text. Verification is deferred to the media follow-up.

## System context

```text
Allowed Telegram user
        |
        | Telegram Bot API (getUpdates long polling, sendMessage)
        v
trygalle (cmd/trygalle)
  telegram transport
    - polling loop and offset
    - allowlist
    - rendering boundary
    - delivery retry/hold queue
        |  (text in, raw)
        |  (Response / Notice out)
        v
runtime coordinator (unchanged)
  - reserved command classification
  - one active operation
```

## Component design

### Package and dependency amendment

`internal/telegram` contains the polling loop, delivery pipeline, rendering boundary, and configuration. It depends on `internal/runtime` and `go-telegram/bot`, not on `internal/rpc`.

The repository constraint "Go standard library only" is amended to permit exactly one third-party dependency, `go-telegram/bot`, pinned to a specific released version (`go.mod` directive, no floating versions). The amendment reflects the parent design's known future use cases: photo and voice support would require multipart upload, file-ID persistence, and MIME handling that a maintained library already owns. `go.mod` otherwise remains dependency-free.

### Settings

Environment-only configuration, consistent with the secrets-must-not-be-committed rule:

| Setting | Meaning | Failure when absent |
|---|---|---|
| `TRYGALLE_TELEGRAM_TOKEN` | Bot API token | Fatal startup error, non-secret diagnostic |
| `TRYGALLE_TELEGRAM_ALLOWED_USER` | Allowed sender's numeric ID | Fatal startup error |
| `TRYGALLE_TELEGRAM_TIMEOUT` | Long-poll timeout seconds | Defaults to 30s |

Tuning knobs (retry budget, hold deadlines) are constants in the package, not configuration surface, until beta evidence demands otherwise.

### Polling lifecycle and inbound replay

The transport polls `getUpdates` in a loop, independent of coordinator state, so an active run never blocks `/abort`.

- Startup polls from offset 0 without `drop_pending_updates`. Telegram's backlog (up to 24 hours) replays into the resumed Pi session. Downtime is self-announcing: while the bot is down there are no replies, and the replay itself is the visible backflow. No additional "you were away" notice kind is introduced, keeping the coordinator's notice taxonomy closed.
- Updates are forwarded in `update_id` order. The first non-reserved message starts a run; later messages in the same drain steer into the active run and merge into that run's single final response, which is the steering semantics the runtime design defines. Reserved commands follow their own coordinator paths (`/status` while busy works; `/new` and an additional `/abort` follow busy-report rules).
- The offset advances in memory after an update is handed to `HandleUserInput`. A crash between processing and confirming can re-run the most recent message after restart. The window is bounded by one update; a duplicated Deployment restart is idempotent in effect. A durable acknowledgment ledger would remove the window only by reopening the rejected durable-state branch and is a priority for later revision only if a re-run matters in practice.
- A message from a user ID not on the allowlist is dropped without a response or a log of its content; unknown content is never echoed, even in errors.
- Non-text updates (photos, voice notes, documents, stickers) receive one canned, unformatted reply — a transport-level message outside the notice taxonomy — and are otherwise ignored. This covers unsolicited media explicitly instead of the parent's silent behavior, matching the bridge's silence-reads-as-broken principle.
- The Telegram client surfaces updates as raw text. Telegram's slash-command menu is a client-side convenience; the transport forwards whatever text arrives, and Pi's own syntax governs whether it means anything.

### Connection failure semantics

| Failure | Behavior |
|---|---|
| HTTP 409 on `getUpdates` | Treat as a configuration error (another poller holds the token). Log the conflict and exit non-zero, so Kubernetes restarts into the same visible failure rather than two pollers racing. Recovery is operator action (a stray local process or a second deployment), not retry. |
| HTTP 429 with `retry_after` on `getUpdates` | Wait `retry_after`, then repoll. This is Telegram's own backpressure protocol. |
| Repeated poll failures | After N consecutive failures, log a polling-stall diagnostic. Detection is the transport's contribution to liveness; where it lands operationally belongs to follow-up #5. |
| Network error mid-poll | Back off briefly and continue the loop; the handler's own poll timeout bounds the wait. Long-poll requests use a client timeout larger than the requested `timeout` so a stalled connection cannot pin the loop. |

### Outbound delivery

The delivery pipeline is one chain: message construction, rendering, chunking, sending. It retries sends, never Pi work.

```text
Runtime Response / Notice
        |
        | render (12 classes to plain text)
        v
plaintext renderer (single boundary)
        |
        v
chunker (4096-character limit, line-boundary preference, UTF-8-safe)
        |
        v
single-owner send loop
  - hold-for-reconnect on transient failure
  - honor retry_after on 429
  - log-only after exhaustion
```

- **Rendering boundary.** All 12 outbound classes pass through one `render(message) string` function before sending, so a later dialect change (beta dialect is plain text, no `parse_mode`) touches one function. Pi responses render as received; notice classes render from the wording table below.
- **Chunking.** Text over 4096 characters splits at a character (never byte) boundary, preferring line breaks. No escape state exists in plain text; splitter tests still pin UTF-8 boundary behavior because the limit counts characters in the Telegram API contract.
- **Retry classes.** Terminal classes (the response, plus `SteerAcknowledged`, `ExtensionHandled`, `EmptyResponse`, `FailedRun`, `AbortReport`, `NewSessionReport`, `SessionResumeFailed`, `RoutingFailure`) retry with backoff and hold in memory until the connection returns, ordered FIFO as produced. Recurring classes supersede instead of accumulate: a newer pending `Status` or `Heartbeat` replaces an older undelivered one, bounding retained memory without a configurable cap. `RestartNotice` stays best-effort within the shutdown deadline per the runtime design and is never held.
- **Exhaustion.** When retries exhaust, the transport logs metadata (message class, size, attempts) without content and drops the message. A secondary "delivery failed" notification is rejected: the parent design requires log-only behavior, and a failed send reacting to a failed send usually fails again.
- **Duplicate risk.** Like OpenClaw, the transport rescinds replays on ambiguous sends: no resend after a timeout that may have delivered. Only errors proven not to have reached Telegram (connection reset before a response, 429, 5xx) retry. Telegram sends carry no idempotency key, so ambiguous-retry duplication is a design hazard rather than a library default.
- **Order and escalation.** Delivery preserves the production order of outcomes: a response emitted after a steer acknowledgment is delivered after it, per Telegram chat ordering.

### Response and notice wording

The response class renders Pi's final text as received. Notice wording addresses the operator directly and stays short; the strings below are the reviewed defaults and live in `internal/telegram` as constants. Bracketed placeholders are filled from the notice payload.

| Notice | Draft wording | Payload used |
|---|---|---|
| `Status` (idle) | `'No active operation. Session <SessionID>, <MessageCount> messages, model <ModelID>.'` | `State` |
| `Status` (active) | `'Working: <Elapsed> on latest operation. Queue <PendingMessageCount>. Model <ModelID>.'` | `State`, elapsed |
| `SteerAcknowledged` | `'Queued into the current run.'` | — |
| `ExtensionHandled` | `'Handled by the extension; no run started.'` | — |
| `EmptyResponse` | `'Pi finished without producing text.'` | — |
| `FailedRun` | `'Run failed: <Text>'` | `Text` |
| `AbortReport` (dropped) | `'Aborted. Dropped <n> queued inputs.'` | `Dropped` count |
| `AbortReport` (idle) | `'No operation was active.'` | — |
| `NewSessionReport` (dropped) | `'New session. Dropped <n> queued inputs.'` | `Dropped` count |
| `NewSessionReport` (cancelled) | `'Session switch cancelled; current session kept.'` | — |
| `NewSessionReport` (clean) | `'New session started.'` | — |
| `SessionResumeFailed` | `'Could not resume the last session (<Text>); started fresh.'` | `Text` |
| `Heartbeat` | `'Still working — the run started <Elapsed> ago.'` | `Elapsed` |
| `RoutingFailure` | `'Trygalle routing failed: <Text>. The prior input was not lost mid-run; recheck with /status.'` | `Text` |
| `RestartNotice` | `'Restarting; active operation aborted.'` (best effort during shutdown) | `Text` |

`Status` wording derives at render time from `rpc.State` plus the coordinator's elapsed accounting for the active case; rendering stays transport-side and the coordinator is unchanged.

### Wording policy rules

- Notice classes never embed prompt content or Pi output; every payload field the coordinator sends is lifecycle metadata, so redaction is structural rather than a content filter (follow-up #5 owns the logging policy boundary). Notice strings that can carry a cause (`FailedRun`, `RoutingFailure`, `SessionResumeFailed`) stay short and diagnostic, quoting Pi's error text only where Pi generated it.
- No wording introduces a second command syntax. Only the three reserved commands exist.

### Shutdown behavior

The coordinator's shutdown sequence is unchanged. On `ctx` cancellation the transport stops the poll loop, drains nothing, emits no new notices, and exits within the runtime's timeout admission. Held-but-undelivered terminal messages at shutdown are inherently lost with the process; the accepted parent tradeoff (`RestartNotice` best effort, never held) is already in force. Any answer lost this way lives in the resumed Pi session.

### Production process composition

`cmd/trygalle` sequences the components this follow-up exercises:

1. Load environment configuration.
2. Run the runtime `Start` ladder: spawn Pi, establish RPC, resume the latest session, complete validation.
3. Construct the coordinator with the Telegram transport as its `Notifier`.
4. Start the polling loop.

Pi stdio plumbing, startup validation, and the in-process heartbeat come from follow-up #1 implementations. Image and Kubernetes objects remain follow-ups #3 and #4.

### Future media seam

The transport detects non-text update shapes centrally and passes them to one `handleMedia(update)` hook that currently sends the canned unsupported reply. The future design:

- Downloads through `getFile`, writes to a Pi-readable directory, and references the path in the prompt text; no RPC change is anticipated, but the pinned Pi version's attachment support gets re-verified before implementation.
- Outbound media is a `Response`-adjacent render variant, substituting the image send for text when a future Pi extension produces one.

Both paths stay inside the transport without a coordinator change.

## Alternatives considered

### Hand-roll the Telegram client on the standard library
Merit: keeps the repository dependency-free and the wire surface ~300 lines of boring code.
Decision: rejected. The parent design's known future media use cases are where hand-rolling stops being boring (multipart, file IDs, MIME), and a maintained library with zero transitive dependencies bounds the supply-chain cost to one package. The Pi RPC precedent (stdlib client) differs because Pi's surface has no media future.

### `go-telegram-bot-api` (tgbotapi) or `tucnak/telebot`
Merit: both are the historical default choices with larger communities.
Decision: rejected on health evidence: tgbotapi's latest release is December 2021 and telebot's is June 2024. Bot API changes since those releases would land on Trygalle instead of the library.

### HTML or MarkdownV2 outbound rendering now
Merit: proper code-block rendering makes daily triage output more readable in chat.
Decision: rejected for the beta. With no `parse_mode`, the renderer and chunker have no escape state, so correctness costs almost nothing at the stage where the wording table itself is the reviewable surface. The rendering boundary keeps the dialect swappable in one function; the accepted limit is raw backticks in chat.

### Durable outbound queue across restarts
Merit: no answer is ever lost between Pi and Telegram, regardless of crash timing, without operator re-asks.
Decision: rejected. The parent design restricts runtime state to memory, the loss window is the seconds between settle and delivery, and the answer lives on in the resumed Pi session (ask again in-chat to recover). OpenClaw's durable queue is also the machinery behind their duplicate-send regression. Revisit trigger: losing an answer at a restart matters in practice.

### Drop pending updates at startup (`drop_pending_updates`)
Merit: matches the parent's no-queue philosophy; a restart implies stale context, and unprocessed messages can arrive hours old.
Decision: rejected. Unserved messages while the bot is down are invisible loss (copy-paste erosion), the runtime's steering already absorbs multi-message replay, and ZeroClaw's offset-from-zero design is direct prior art at similar scale.

### Fix 409 conflicts by retry blind
Merit: transient conflicts resolve when the other poller dies.
Decision: rejected. Auto-retry turns a misconfiguration into a silent zombie, exactly the failure class this design exists to make loud.

## Failure behavior

| Failure | High-level behavior |
|---|---|
| Token or allowlist absent | Fatal startup error with a non-secret diagnostic |
| 409 on getUpdates | Log and exit non-zero; Kubernetes restarts into the visible failure |
| 429 on getUpdates | Honor `retry_after`, repoll |
| Poll stall | Log a polling-stall diagnostic after consecutive failures |
| Send failure, proven transient | Retry with backoff; hold the message for reconnect |
| Send failure after exhaustion | Log metadata; drop content |
| Send failure, ambiguous | Drop, never resend (duplicate guard) |
| Pi run finishing with undelivered pending replies | Deliver all held replies in production order |
| Unsupported media input | Canned unsupported reply; content stays unread |
| Message text over 4096 characters | UTF-8-safe chunking before send |

## Operability

Log philosophy matches the runtime design: lifecycle metadata, no message or Pi content. Events worth logged surfaces: startup config validation, poll-loop start and stop, offset and backlog replay extents, allowlist rejections (count, not content), delivery retries and exhaustion by message class, 409 and 429 occurrences, stall warnings, and shutdown requests. All events connect by the coordinator's existing request attribution where present.

The transport adds no routes, dashboards, or metrics. Follow-up #5 owns how these log events map into acceptance and alerting; the polling-stall diagnostic exists here so there is something to alert on later.

## Security and data handling

- The token and the allowed user ID enter through environment configuration; nothing personal or secret is committed. Manifest examples use placeholders only (follow-up #4 owns Kubernetes Secret wiring).
- Allowlist enforcement happens before any processing; unauthorized senders never appear in logs with content.
- Renders carry metadata, never prompt bodies or Pi output; rejects quote nothing. Canned replies are fixed strings.
- Public-repository hygiene from the parent design carries over unchanged: no bot handles, no chat IDs, no live tokens in tests, examples, or image metadata.
- Long polling is egress-only; nothing listens. The bot token is the only authentication secret in the transport layer; model credentials stay in follow-up #3's image design.

## Testing strategy

- **Consumer contract:** unit tests drive `Notifier` directly with a housing matrix for all 11 notice kinds and the response class, asserting the wording table output and payload substitution.
- **Delivery pipeline:** table-driven tests for chunk boundaries (4096 character limit, multi-byte splits, trailing empty lines, absence of byte-boundary splits), the supersede rule for `Status` and `Heartbeat`, FIFO order for terminal classes, and the per-failure response classes (transient holds, `retry_after` handling, exhaustion, ambiguous-drop).
- **Poll lifecycle:** integration tests against a mock Bot API HTTP server: long poll cycles, offset advancement, backlog replay from offset 0, 409, 429, poll-stall detection, and allowlist drop.
- **Rendering boundary:** render tests pin the 12-column wording table as the reviewed strings; changing wording requires touching exactly these tests.
- **End to end:** manual runs against a throwaway bot chat before real credentials: text round trip, busy semantics, `/abort` mid-run, `/new`, downtime replay, and an unsolicited photo's canned reply.

## Rollout and rollback

Same shape as follow-up #1: implement and test locally against a mock Bot API, then a throwaway bot in a private chat; bind to an existing `cmd/trygalle` composition. Ideally a throwaway bot is exercised by its own operator manual test before build.

Rollback is the previous image and configuration; session PVC is preserved. No data migration, since the transport holds no durable state.

## Open questions

1. Pin strategy: this design pins `go-telegram/bot` to a concrete released version; the upgrade gate (contract tests green against the new release, release notes reviewed) matches the pinning philosophy — exact current version is fixed at implementation, not in this document.
2. `Status` wording for the idle-with-previous-session case (after restart, before any run) — the wording table covers the active case fully; the idle case has two natural variants and the better one becomes apparent at implementation review.
3. Whether the canned unsupported-media reply distinguishes photo from voice note — draft says one string; a two-string split is a styling decision for implementation review.
4. Which consecutive-failure count trips the polling-stall diagnostic — a constant chosen in implementation with the mock-server tests, not a tunable.

## Self-review notes

Findings incorporated during the skeptical review:

- The wording table was added after review; it is the follow-up's largest new surface and leaving it entirely to implementation would have made the design not reviewable.
- The single-retry-owner rule (one transport, never the coordinator) was checked structurally: the notifier seam carries no error return, making coordinator-side retries impossible by construction.
- The offset-conflict posture (409 = misconfiguration, fail loud) was verified against the runtime design's fatal-failure doctrine ("pause as process/protocol integrity failure") to avoid a zombie poller pattern Hermes and ZeroClaw both treat as a first-class failure.
- Durable state checks: the earlier accepted hold-FIFO excludes `RestartNotice` by parent rule; the batch-by-rate admission formed its bounded-size claim, and the document records both refinements.

Deferred alternatives recorded with triggers:

- HTML/MarkdownV2 rendering: triggered by beta fatigue with raw backticks.
- Durable outbound queue: triggered by a restart-related answer loss in practice.
- Media adapter implementation: triggered by the media follow-up, after Pi attachment support is re-verified against the pinned Pi version.

No blocking design issue remains. Unresolved questions are bounded to implementation-review details.
