# Pi runtime and RPC design
Status: Approved

Date: 2026-10-03

Scope: Detailed design of Trygalle's Pi runtime boundary, follow-up #1 of the approved high-level design (`plans/high-level-design/design.md`). The Telegram interface, Pi environment and image, Kubernetes deployment, and operability designs remain separate follow-ups.

## Summary
Trygalle's Go process owns one long-lived Pi subprocess and speaks Pi's JSONL RPC protocol over its standard input and output. This design defines the operation state machine on top of that protocol: how Telegram messages become RPC commands, how Trygalle knows an operation is complete, what the user sees in every terminal state, how sessions resume after restarts, and how the process behaves under shutdown and protocol failure.

The central rule: Pi is the authority on session and run state. Trygalle routes every input through the `prompt` RPC command, delegates queueing and "latest session" selection to Pi, and never reimplements session-file parsing, command expansion, or completion detection as competing sources of behavior. Trygalle's coordinator tracks operations for attribution and visibility only.

## Problem
The high-level design left the Pi runtime boundary's mechanics open: prompt and slash-command completion semantics, busy behavior, session resume after clean and abrupt restarts, timeouts, malformed protocol data, and the exact RPC contract. Each of those deferrals lands here. The bridge's correctness core is this component: every Telegram-visible outcome depends on the state machine defined in this document.

## User and audience
Matteo is the sole user, operator, and maintainer. Secondary readers are reviewers of the public `trygalle` repository who need to understand the RPC contract without access to the private harness.

## Goals
- Route every non-reserved Telegram message into Pi through one RPC command.
- Detect operation completion from the event stream without racing Pi's own state.
- Produce a visible Telegram outcome for every terminal state of an operation.
- Resume the latest session after pod restart with a bounded degraded fallback.
- Shut down deterministically under Kubernetes signal semantics.
- Keep the coordinator stateless: Pi owns queues, sessions, and "latest."
- Pin the consumed protocol behaviors as contract tests against the installed Pi version.

## Non-goals
- Telegram presentation: formatting, message splitting, delivery retries, notice wording (follow-up #2)
- Container image, dotfiles mapping, working directory and project trust (follow-up #3)
- Kubernetes objects, grace periods, probes (follow-up #4)
- Log event schema and redaction policy (follow-up #5)
- Bridging extension dialogs to Telegram interactions
- Streaming response edits or typing indicators
- A request queue, session database, or second conversation store inside Trygalle

## Context reviewed
- Pi documentation installed with the pinned CLI: `rpc.md`, `rpc-commands.md`, `json.md`, `rpc-extension-ui.md`, `sessions.md`, `session-format.md`, `how-pi-works.md`, `slash-commands.md`, `keybindings.md`.
- Installed Pi implementation (compiled `dist/`): `agent-session.js` prompt/steer/compaction paths, `session-manager.js` session loading and discovery, `interactive-mode.js` TUI input handling, `rpc-mode.js` command dispatch. Used to verify documented behavior and to answer questions the documentation leaves open.
- Live protocol experiment against Pi 1.0.1: a temporary extension command synchronously called `pi.sendMessage(..., { triggerTurn: true })`; a local mock model kept the test hermetic. Across five runs, `agent_start` preceded the RPC `prompt` response with disposition `handled`. A variant that scheduled the call 25 ms after the handler returned emitted `handled` first and `agent_start` later.
- Prior art: OpenClaw gateway documentation (messages, agent runtime, session concepts), Hermes Agent messaging documentation, `badlogic/pi-telegram` (README and source), `atharva-again/tandoor` (README), and the Pi Durable announcement post.
- Advisory learning store: one matched section ("Decouple data-plane operation from control-plane availability via local state retention"); not materially applicable to a single local subprocess and recorded here for provenance only.

### Confirmed protocol facts
Verified against documentation and, where noted, the installed implementation:

- `prompt` accepts an optional `streamingBehavior` field; the value `"steer"` queues the message into an active run (implementation-verified in `rpc-mode.js`).
- `prompt` response `data.disposition` is `"started"`, `"queued"`, or `"handled"`. `"handled"` means an extension command or input handler consumed the prompt and no run started for that prompt; an extension command may still start its own run through `pi.sendMessage()` (implementation-verified in `agent-session.js`). For Pi 1.0.1, when the awaited extension command handler directly calls `pi.sendMessage(..., { triggerTurn: true })`, `agent_start` is emitted before the `handled` response. The implementation path and a five-run live experiment verify this ordering; the contract suite must pin it. A 25 ms `setTimeout` variant emits `handled` first, so fire-and-forget work scheduled after the handler returns is outside this attribution contract.
- `steer` and `follow_up` error when the text is an extension command (implementation-verified).
- `prompt` throws while compaction is in progress; `steer` queues during compaction (implementation-verified).
- `agent_end` closes one low-level run and may be followed by retries, compaction recovery, steering, or follow-ups. `agent_settled` means Pi has no remaining automatic work.
- `abort` aborts the current operation and responds after the session is idle. `abort` continues queued steering messages when they remain in the session; interactive Esc behavior is `clear_queue` before `abort`.
- `clear_queue` removes queued steering and follow-up messages and returns their text.
- `new_session` starts a fresh session and can be canceled by a `session_before_switch` extension event handler, returning `{"cancelled": true}`.
- `get_last_assistant_text` returns the last assistant text in the session, or `null` if none exists.
- Session loading skips malformed JSONL lines; session discovery is best-effort and excludes unreadable files without failing other sessions (implementation-verified in `session-manager.js`).
- `--continue` resumes the most recent session for the current working directory. `--session-dir` selects the session directory.
- Extension UI dialog methods (`select`, `confirm`, `input`, `editor`) block the extension until answered or until an optional extension-supplied timeout auto-resolves. Fire-and-forget methods (`notify`, `setStatus`, `setWidget`, `setTitle`, `set_editor_text`) expect no response.
- Malformed JSON commands produce a `parse` response without a request ID. Closing stdin requests an orderly shutdown; Pi completes after the current command or after the active run settles.

## Deviations from the high-level design
This design changes two approved decisions and adds one. All three follow one principle: **parity with the Pi TUI**, because the user's mental model of Pi comes from the TUI, and every divergence between the TUI and Telegram becomes an unexplained behavior difference for the same harness.

1. **Prompts during an active operation steer into the run instead of being rejected as busy.** The parent approved reject-busy. Prior art (OpenClaw, Hermes, `pi-telegram`, tandoor) either queues or steers; none rejects. TUI parity: typing while Pi works is steering by default. Rejection also blocked mid-run corrections, which are the highest-value input during a run.
2. **`/new` during an active operation executes immediately instead of being rejected as busy.** The TUI handles `/new` before its streaming check, so it executes mid-run; over RPC the same `new_session` command provides identical behavior.
3. **A steering acknowledgment is sent to Telegram.** The parent design did not define feedback for accepted input. Without it, an accepted message during a run is indistinguishable from an ignored one (the "talking to a wall" failure: no streaming, no progress, silent acceptance). Trygalle system notices are a distinct message category so the user can tell Trygalle messages from Pi responses.

The parent design's beta success criteria change accordingly: "a second prompt receives a busy response" becomes "a second prompt is steered into the active run and answered in the final response," and "`/new` returns busy" becomes "`/new` supersedes the active operation."

## Design overview
### Process model and startup
Trygalle starts Pi as `pi --mode rpc --continue --session-dir /data/pi/sessions` (exact flag set finalized with the image design). The event listener is installed before the first command is sent, because a fast completion can emit events before a late subscriber attaches; Pi's own `promptAndWait` follows the same rule.

Startup validation is the detector for every startup failure mode:

1. Send `get_state`; require a success response with a session identifier under a 60-second deadline.
2. Send `get_commands`; require a success response with a non-empty list. Log category counts (extensions, prompt templates, skills) without names; the expected-inventory comparison happens outside the application.
3. Log the resumed session identifier from `get_state` so a silent fallback to an older session is visible in diagnostics.

If validation fails or Pi exits, stop that process before retrying once with the same command line (transient causes). Close stdin, wait under a bounded deadline, and kill the child if it does not exit; never overlap Pi processes that share a session directory. A second consecutive failure starts Pi without `--continue`: a fresh session, a loud system notice to the user ("session resume failed; started a fresh conversation"), and a log entry with the failure. If the degraded start also fails, the cause is not the session; Trygalle exits and Kubernetes restarts the pod, matching the parent's fatal behavior.

### Session resume
Pi owns the definition of "latest": `--continue` resumes the most recent session for the current working directory. Trygalle never lists, ranks, or parses session files. Two consequences fall out for free: after `/new`, the new session is the most recent, so the parent's "the new session becomes the one that resumes after restart" holds without code; and the working directory (pinned by the image design) defines the candidate session set.

Abrupt termination is tolerated by Pi itself: session loading skips malformed lines, so a partial trailing line from a mid-save kill is dropped, and a wholly unreadable file is excluded from discovery while other sessions remain reachable. The degraded ladder above covers the rarer case where resume is fatal for another reason.

### Message routing
One rule, no coordinator-state branching:

| Input | RPC action |
|---|---|
| Any non-reserved Telegram text | `prompt` with `streamingBehavior: "steer"` |
| `prompt` fails with the compaction error | Retry once with bare `steer` |
| Retry also fails | System notice; never a silent drop |
| `/status` | `get_state`, human-readable summary (fields owned by follow-up #2) |
| `/abort` | `clear_queue`, then `abort`, then report dropped texts |
| `/new` | `clear_queue`, then `new_session`; report superseded operation |

`prompt{steer}` is correct in every state because Pi decides: idle runs the message (`"started"`), an active run queues it as steering (`"queued"`), and an extension command executes immediately even mid-run (`"handled"`) — the exact TUI behavior. Coordinator state is therefore not a routing input and cannot race Pi's own state. The bare-`steer` fallback exists because `prompt` rejects input during compaction while `steer` queues; delegating the queue to Pi keeps Trygalle stateless, which is why the fallback does not hold the message in Trygalle memory instead.

`/abort` mirrors TUI Esc exactly: `clear_queue` before `abort`, because `abort` alone continues queued steering messages. The returned dropped texts are reported to the user rather than restored to an editor, because Telegram already holds them in the user's own history. The command stays named `/abort` (intent, not a TUI key); the design documents the Esc equivalence.

`/new` mirrors the TUI's immediate execution: the active operation is superseded, not rejected. `clear_queue` runs first so no steering messages survive the switch. A `session_before_switch` extension handler may cancel the switch (`{"cancelled": true}`); Trygalle reports the cancellation and keeps the current session.

### Operation state machine
An operation is the run started by a user message plus everything steered into it. One coordinator event loop is the sole consumer of Pi events and UI requests and owns operation state. It also observes process exit. `HandleUserInput` routes a command and returns after its RPC response; it does not wait for `agent_settled`, so the REPL can accept more input while a run is active. Concurrent state changes are serialized by the coordinator, while Pi remains the authority on queueing and run state.

States and transitions:

| State | Entry | Exit |
|---|---|---|
| Idle | Startup complete, or terminal state delivered | `prompt` accepted (`"started"`) or `"handled"` with a preceding `agent_start` |
| Running | Run started for the operation | `agent_settled`, failure classification, abort, or `/new` supersede |
| Superseded | `new_session` succeeded mid-run | Idle after reporting |

Completion rules:

- **`"started"` / `"queued"`:** the response means acceptance only. The operation is complete at `agent_settled`.
- **`"handled"`:** no run started for this prompt, but an extension handler may synchronously start a run through `pi.sendMessage()`. For the pinned Pi 1.0.1 path, `agent_start` for that run arrives before the `handled` response. If the coordinator observed that event before the response, track the run to `agent_settled`; otherwise complete the handled input immediately without querying previous assistant text. Contract-test this ordering. Fire-and-forget work scheduled after an extension handler returns is outside operation attribution and is logged as unassociated; revisit only if Pi adds a request-to-run correlation mechanism.
- **Retries, compaction, steering, follow-ups:** all covered by waiting for `agent_settled`; `agent_end` alone is never the completion signal.

Final text source: after `agent_settled`, send `get_last_assistant_text`. It is authoritative and returns `null` when no assistant text exists; treat an empty string as no text as well. Do not reconstruct text from the event stream; the coordinator uses events only for terminal-state classification (assistant `stopReason` and error messages). One guard: call `get_last_assistant_text` only when a run actually settled for this operation. After `"handled"` with no run observed before the response, the query would return the previous run's text; send the empty/handled notice directly instead.

### Terminal states
Every terminal state produces a visible Telegram outcome. The interface is not the TUI: nothing is shown unless it is sent, so silence is indistinguishable from a broken bridge.

| Terminal state | Telegram outcome |
|---|---|
| Settled with text | The final response text |
| Settled, no text | System notice: Pi returned no text |
| Failed run (assistant `stopReason: "error"`) | Failure notice with Pi's error message, no automatic retry |
| Aborted via `/abort` | Abort result plus dropped-message report |
| Superseded via `/new` | New-session result plus dropped-message report |
| Steer accepted | Short system notice acknowledging queueing |
| Routing failure | System notice; input is never dropped silently |

### Heartbeat
There is no watchdog deadline. A model call may legitimately take minutes, and an automatic abort would kill legitimate work; `/abort` is the only operation terminator, matching TUI parity (Esc is the user's timeout).

While an operation is active, the coordinator emits a heartbeat system notice every 5 minutes (one configurable constant): elapsed time, the type and age of the last observed event (metadata only, no content), and an `/abort` hint. The RPC client records the type and arrival time of every event, including the per-token updates it does not deliver to the coordinator, so the activity signal is nearly free. A long silence appears as "no activity for X minutes" in the same notice; the judgment and the abort stay with the user.

### Shutdown
On SIGTERM (rollouts, evictions):

1. If an operation is active: `clear_queue`, then `abort`, then wait for idle under the shutdown deadline.
2. Close Pi's stdin to request an orderly shutdown.
3. Wait for Pi to exit under the same deadline; exit 0 either way. The resume design tolerates mid-run death by construction, so shutdown time is never traded for tidiness.
4. Best-effort system notice ("restarting; active operation aborted") if Telegram delivery succeeds within the deadline; delivery never blocks shutdown.

When idle: close stdin and wait. Kubernetes grace-period tuning is owned by the Kubernetes design; this component requires only that the deadline is shorter than the pod's grace period.

### Extension UI
The private harness uses extension dialogs; over RPC there is no human at a terminal to answer them. If a blocking dialog were ignored, the extension would block forever, the run would never settle, and the bot would hang mid-operation.

- Answer every dialog request (`select`, `confirm`, `input`, `editor`) immediately with `cancelled: true` — the equivalent of pressing Esc on the dialog.
- Log the request method only; never log dialog content, which can hold sensitive text.
- Fire-and-forget requests (`notify`, `setStatus`, `setWidget`, `setTitle`, `set_editor_text`) are logged as metadata, not delivered.

Bridging dialogs to Telegram replies or buttons is the recorded future feature that restores interactive parity.

### Protocol and framing
- Write one complete JSON object per record, LF-terminated. Read stdout as a byte stream split only on LF; strip an optional preceding CR. Do not use a line reader that splits on Unicode separators (Node `readline` splits on `U+2028`/`U+2029`, which are valid inside JSON strings). No line-length limit.
- Treat stdout as protocol data only; stderr is logged separately and never parsed as protocol.
- Honor stdin backpressure when writing; read stdout continuously so Pi is never stalled.
- An unparseable or non-conforming line on stdout is a protocol integrity failure: log metadata (no content) and exit; the pod restart rebuilds the stream. Attribution cannot be trusted after protocol corruption. Pi's own `parse` error responses — Pi rejecting a malformed command from Trygalle — are logged, not fatal; they indicate a Trygalle bug, not a broken stream.

## Smallest user-feedback slice
This design document is the slice. It unblocks `/plan` for the Pi runtime component and encodes the contract-test list that later implementation slices run against. No smaller artifact produces reviewable decisions for this component, and no code slice can be safely cut before the state machine and protocol facts are pinned.

Deliberately deferred with triggers: implementing the coordinator (needs this design approved and planned), the Telegram notice wording (needs the Telegram design), and the Pi Durable migration path (needs upstream stability).

## Success criteria and validation
- Every state-machine transition produces a defined Telegram outcome; no terminal state is silent.
- All 13 contract tests pass against the pinned Pi version; every fact in "Confirmed protocol facts" that the documentation does not state is covered by a test rather than an assumption.
- Busy semantics behave identically to the TUI: mid-run messages steer, mid-run extension commands execute, `/abort` and `/new` match Esc and `/new` behavior.
- Pod restart resumes the latest session; a resume failure degrades to a fresh session with one restart, not a crash loop.
- SIGTERM during an active run ends deterministically within the shutdown deadline.

Validation is the contract-test suite plus manual end-to-end checks in the Talos cluster per the parent design's beta criteria.

## Alternatives considered
### Reject prompts while busy (parent-approved)
Merit: the smallest state machine — no queue semantics, no attribution across multiple messages, no visibility question for accepted input; matches the parent's single-active-operation security posture with the least machinery.
Decision: rejected. All four surveyed systems queue or steer; none rejects. Rejection blocks mid-run corrections, the highest-value input during a run, and combined with no streaming and no progress output it makes the bridge silent in every direction during long runs.

### Queue follow-ups for after the run (`follow_up`)
Merit: `pi-telegram` ships exactly this and it preserves turn separation — the final response answers one message, not a blend; the queue lives in Pi, not Trygalle.
Decision: rejected in favor of steering. TUI parity is steering (the default interactive behavior), mid-run corrections reach the model, and the same `prompt{steer}` call covers both cases without a second routing rule. Follow-up delivery also has queue-visibility costs without TUI status lines.

### Route by coordinator state (`idle → prompt`, `active → steer`)
Merit: explicit control over which RPC command is sent and when; no reliance on the `streamingBehavior` passthrough.
Decision: rejected. Bare `steer` errors on extension commands, so mid-run extension commands would fail; and the routing decision would race Pi's own state. `prompt{steer}` is correct in every state because Pi — the authority — decides.

### `abort` alone for `/abort`
Merit: one RPC call; aborts the active operation and waits for idle.
Decision: rejected. `abort` continues queued steering messages when they remain in the session, so the user's undelivered corrections would run after the abort. Esc parity requires `clear_queue` first.

### Hold compaction-window messages in Trygalle
Merit: the TUI does this with its own compaction queue, so it is the behavior Pi's authors chose for the same case.
Decision: rejected. A Trygalle-held queue is a second source of truth competing with Pi's queue, plus flush and failure logic. The bare-`steer` fallback delegates the queue to Pi and keeps the coordinator stateless.

### Select sessions in Trygalle (`switch_session`)
Merit: explicit control over which session resumes; no dependence on `--continue` semantics.
Decision: rejected. `switch_session` needs a path, so Trygalle would have to list and rank session files — the session-file parsing the parent design explicitly forbids as a competing source of behavior.

### Automatic operation watchdog
Merit: a hung provider call would terminate on its own instead of silencing the bot until manual `/abort` or a pod restart.
Decision: rejected. A deadline cannot distinguish a hang from a legitimate long call, and killing legitimate work is the worse failure for a personal operations bridge. The heartbeat makes silence visible; the user keeps the judgment.

### Reconstruct final text from the event stream
Merit: no extra round-trip after settlement; the text is already in the consumed events.
Decision: rejected. `get_last_assistant_text` is the authoritative query with an explicit `null`, and reconstruction duplicates Pi's message assembly — a second source of behavior.

### Adopt `badlogic/pi-telegram` behavior wholesale
Merit: the closest prior art — a working Pi-Telegram bridge by a Pi-adjacent author, with a complete terminal-state table worth copying.
Decision: behavior copied, code not. It is an in-process TypeScript extension (no process boundary, so its completion detection does not transfer), and the repository ships no LICENSE file, so its code cannot be safely treated as MIT-licensed even if the README claims it. Its `agent_end`-based completion would also be wrong across the boundary: it ignores `willRetry`, and `agent_end` can be followed by retries and compaction recovery.

### Adopt Pi Durable
Merit: crash-continuing runs, exactly-once submissions, concurrent conversations, and durable application state would all come free, and upstream is building messaging bots on it.
Decision: deferred, not rejected. It is experimental ("the API might still change"), TypeScript in-process, and would replace the approved Go-subprocess architecture. Revisit trigger: Pi Durable leaves experimental status and exposes a stable remote-client protocol. Watch item: if upstream ships a maintained Telegram bridge, Trygalle's Telegram adapter may become its only unique part.

## Risks and mitigations
| Risk | Mitigation |
|---|---|
| `streamingBehavior` passthrough changes across Pi versions | Pin the Pi version; contract-test disposition values in each state (idle, streaming, compaction) |
| Steer-during-compaction delivery differs from the TUI's own queue | Contract test for delivery ordering; the TUI parked compaction-window messages in its own queue rather than calling `steer` |
| Mid-run `new_session` emits unexpected trailing events | Coordinator treats `new_session` success as "operation over"; trailing events logged, not attributed; contract test pins what fires |
| Steering queue survives `new_session` | `clear_queue` runs before `new_session`; contract test verifies the queue is gone after the switch |
| `--continue` on an empty or damaged session dir behaves unexpectedly | Contract tests for first boot and damaged-latest cases; degraded ladder bounds the failure to one restart |
| Extension command dialogs degrade interactive harness commands to no-ops | Immediate cancellation is visible in logs; future Telegram dialog bridging is the recorded restore path |
| Heartbeat notices feel noisy on long runs | One configurable interval constant; wording owned by the Telegram design |
| Protocol drift after a Pi version bump | Every fact in "Confirmed protocol facts" becomes a contract test; upgrade evidence gate owned by the operability design |
| Pi changes extension-run event ordering | Live contract test pins `agent_start` before the `handled` response for a synchronous `pi.sendMessage(..., { triggerTurn: true })` call |

## Testing strategy
Contract tests run against a controlled Pi process (or a protocol fixture implementing the same records) with a temporary session directory:

1. `prompt{steer}` dispositions in each state: idle (`"started"`), active run (`"queued"`), extension command mid-run (`"handled"` + immediate execution).
2. Steer queued during compaction is delivered after compaction ends.
3. `clear_queue` before `abort` leaves nothing queued; `abort` alone continues queued steering messages (pin the documented behavior).
4. Trailing events after mid-run `new_session`; steering queue state after `clear_queue` + `new_session`.
5. `new_session` canceled by a `session_before_switch` handler reports `cancelled` and keeps the session.
6. `--continue` on an empty session directory (first boot) and on a damaged latest session (partial trailing line; wholly unreadable file).
7. `agent_settled` after retries and compaction recovery; `agent_end` with `willRetry` is not treated as completion.
8. `"handled"` with and without an extension-initiated run; for a synchronous `pi.sendMessage(..., { triggerTurn: true })`, assert `agent_start` precedes the `handled` response. A delayed fire-and-forget event is not attributed to the completed input. Include null/empty final text and stale-text guard cases.
9. Failed-run classification from assistant `stopReason: "error"` and the error message in events.
10. Extension dialog requests are answered `cancelled` immediately; fire-and-forget requests are logged.
11. Malformed stdout line → fatal exit; Pi `parse` error response → logged, not fatal.
12. SIGTERM during an active run: `clear_queue` → `abort` → stdin close → bounded exit; SIGTERM while idle.
13. Startup validation deadline (60 s) and the degraded resume ladder (two strikes, fresh-session fallback notice).

## Operability
This boundary logs lifecycle metadata by request identifier: Pi start/exit, session identifier after resume, RPC command outcomes and dispositions, operation start/settle/fail/abort/supersede, heartbeat activity summary, extension UI cancellations, protocol integrity failures, and the shutdown sequence. No prompt bodies, tool output, or dialog content at this layer; the full payload and redaction policy belongs to the operability design.

## Rollout and rollback
The design is implemented locally against a controlled Pi process before receiving any Telegram or Kubernetes credentials, per the parent rollout. Rollback of the application reverts to the previous image; the session PVC is preserved. No schema or data migration is introduced by this component — it stores no durable state.

## Security and data handling
The single-active-run posture is preserved: steering joins the same run, so fan-out remains bounded by one conversation. `clear_queue` + `abort` bounds what one prompt can cause. Protocol facts come from the pinned Pi documentation or confirmed implementation behavior; implementation-only facts have contract tests. No private harness names, paths, or credentials appear in this design, its tests, or its logs.

## Open questions
- Exact wording and formatting of every system notice (steer acknowledgment, empty response, failure, heartbeat, abort and new-session reports) — Telegram design
- Heartbeat interval confirmation (5-minute default) and whether it needs a settings surface — Telegram design
- Shutdown deadline value and its relationship to `terminationGracePeriodSeconds` — Kubernetes design
- Final Pi CLI flag set for startup (`--continue`, `--session-dir`, model and provider flags) — Pi environment design
- Which fire-and-forget extension notifications might deserve Telegram delivery — future revision after beta usage

## Skills loaded and used
| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `learning-opportunities` | `prompt-required` | Brainstorm coaching rules | Prediction-question technique throughout the session |
| `learning-lookup` | `prompt-required` | Advisory guidance before design decisions | One matched section reviewed; not materially applicable; recorded for provenance |
| `feature-worktree` | `prompt-required` | Durable artifact requires a feature worktree | Created `maruina/pi-runtime-and-rpc-design` worktree from `origin/main` |
| `tavily-extract` | `agent-selected` | Prior-art and documentation research | Pi Durable article, OpenClaw and Hermes documentation, `pi-telegram` and `tandoor` repositories |
| `tavily-search` | `agent-selected` | Locate prior-art documentation | OpenClaw and Hermes doc pages |
| `codebase-research` | `agent-selected` | Verify protocol facts against the installed implementation | `agent-session.js`, `session-manager.js`, `interactive-mode.js`, `rpc-mode.js` inspection grounding the "Confirmed protocol facts" section |
| `write` | `agent-selected` | Produce a concise, reviewable public design | Applied to this document |

## Self-review notes
The design was reviewed skeptically against the approved scope.

Material findings incorporated:
- The parent's reject-busy and `/new`-busy decisions were changed with explicit rationale and prior-art evidence; both beta success criteria were rewritten to match.
- The routing rule was corrected twice during the session: bare `steer` (errors on extension commands) and `abort`-then-`clear_queue` (wrong order; `abort` continues queued messages). The final rules are `prompt{steer}` everywhere and `clear_queue` before `abort`.
- The damaged-session crash-loop risk was reduced by evidence: Pi skips malformed lines and excludes unreadable files from discovery. The degraded ladder covers the residual case.
- The stale-text trap (`get_last_assistant_text` after a handled-no-run prompt) is guarded by rule rather than by hope.
- Every fact relied on that the documentation does not state is listed as a contract test rather than assumed.

Material suggestions rejected or deferred:
- A request queue inside Trygalle (second source of truth; Pi owns queues).
- An automatic operation watchdog (kills legitimate calls; heartbeat makes silence visible instead).
- Adopting Pi Durable now (experimental; revisit trigger recorded).
- Copying `pi-telegram` code (no LICENSE file in the repository; TypeScript, not Go; in-process completion detection does not transfer across the boundary).

Known downside accepted: steering widens what one Telegram message sequence can cause within a single run, and the attribution of the final response across steered messages is coarser than turn-separated follow-ups would be. Both are accepted for TUI parity and the smaller routing rule.
