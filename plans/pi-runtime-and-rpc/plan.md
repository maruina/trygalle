# Pi Runtime and RPC Implementation Plan

> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the approved Pi runtime and RPC design (`plans/pi-runtime-and-rpc/design.md`) as the Go `internal/rpc`, `internal/harness`, and `internal/runtime` packages plus a `cmd/trygalle-repl` demo binary, validated by a live contract-test suite against the pinned Pi 1.0.1.
**Smallest user-feedback slice:** A live `get_state`/`get_commands` round-trip against the real installed Pi from Go (Slice 1) — the first observable proof that Trygalle speaks Pi's JSONL RPC protocol over the process boundary.
**Out of Scope:** Telegram presentation, formatting, delivery, and notice wording (follow-up #2); container image, dotfiles mapping, project trust (follow-up #3); Kubernetes objects and grace periods (follow-up #4); log schema and redaction policy (follow-up #5); bridging extension dialogs to Telegram; streaming response edits or typing indicators; any queue, session store, or watchdog inside Trygalle; CI pipeline definition; lint configuration; a `Makefile`.
**Architecture:** One long-lived Pi subprocess per Trygalle process, spoken to over strict JSONL on stdin/stdout. `internal/rpc` owns framing, records, and the client. `internal/runtime` owns the operation state machine, routing, lifecycle ladder, and shutdown. Pi is the authority on session and run state; the coordinator tracks operations for attribution and visibility only.
**Tech Stack:** Go 1.27 (stdlib only, no third-party dependencies), Pi 1.0.1 CLI (`--mode rpc`), `httptest` mock model server for live tests, `log/slog` for lifecycle logging.

---

## Skills loaded and used
| Skill | Source | Why loaded | How used |
|---|---|---|---|
| `resolve-worktree` | `prompt-required` | Design path lives in a feature worktree | Resolved the design path; all work happens in `maruina/pi-runtime-and-rpc-design` |
| `skill-loader` | `prompt-required` | Planning stage requires skill selection | Selected Go, CLI, and prose skills; `k8s-controller-dev` not triggered (no Kubernetes interaction); `codebase-research` not loaded — the repository contains no code yet |
| `learning-lookup` | `prompt-required` | Advisory guidance before planning decisions | One matched section (control-plane/data-plane decoupling); not materially applicable to a single local subprocess; recorded for provenance |
| `go-best-practices` | `skill-loader` | Go files will be created | Version baseline (Go 1.27), consumer-owned interfaces, exit only in `main`, bounded goroutines and channels, `httptest` over real network, external test packages, modernization check before commit |
| `cli-best-practices` | `user-requested` | The REPL is a CLI surface | stdout for data, stderr for diagnostics, `--help` with full long flags, no secrets in argv, consistent exit codes; `--json` deliberately omitted for an interactive debug tool |
| `write` | `skill-loader` | The plan is a prose artifact | Applied to this document |

---

## Implementation Contract
### Components Affected
| Component | Files | Responsibility | Verification |
|---|---|---|---|
| RPC framing | `internal/rpc/framing.go`, `internal/rpc/framing_test.go` | LF-only record scanner and writer; no length limit; backpressure-safe writes | Unit tests (R1) |
| Protocol records | `internal/rpc/records.go`, `internal/rpc/records_test.go` | Decode commands, responses, events, extension-UI records | Unit tests against golden JSON (R1, R2) |
| RPC client | `internal/rpc/client.go`, `internal/rpc/client_test.go` | Start Pi, correlate by ID, event subscription before send, deadlines, extension-UI answers, stdin close | Unit tests + live round-trip (R2, R16) |
| Live-test harness | `internal/harness/*.go`, `internal/harness/testdata/*` | Controlled real Pi: temp dirs, mock model server, test extension, `TRYGALLE_PI_*` gate | Harness self-test (R3) |
| Coordinator | `internal/runtime/coordinator.go`, `internal/runtime/coordinator_test.go` | Routing table, operation state machine, terminal states, heartbeat | Unit tests (scripted client) + live disposition tests (R5–R12) |
| Lifecycle | `internal/runtime/lifecycle.go`, `internal/runtime/lifecycle_test.go` | Startup validation ladder, degraded resume, fatal exits | Unit tests + live resume tests (R15, R16) |
| Shutdown | `internal/runtime/shutdown.go`, `internal/runtime/shutdown_test.go` | Bounded `clear_queue` → `abort` → stdin close sequence | Unit tests + live SIGTERM tests (R17) |
| REPL | `cmd/trygalle-repl/main.go` | Demo surface: stdin lines through the coordinator, stdout `Notifier`, signal wiring | Manual walkthrough (R18) |
| Docs | `AGENTS.md`, `README.md` | Build/test/live-test commands, Pi pin, REPL usage | Final verification |

### Key Decisions
- **Package names without a `pi` prefix** (user decision): `internal/rpc`, `internal/harness`, `internal/runtime`.
- **Live tests are the primary strategy** (user decision): the design's 13 contract tests run against the real installed Pi 1.0.1 with a mock model server. Unit tests cover only inputs real Pi cannot deterministically produce: garbage bytes on stdout, deadline expiry, `U+2028` framing.
- **Test configuration** (user decision): `TRYGALLE_PI_BIN` (path override; default `pi` from `PATH`) and `TRYGALLE_PI_TESTS` = `auto` (default: run when Pi is available, skip with instructions otherwise) | `on` (fail when unavailable — the CI gate) | `off` (always skip). CI installs Pi 1.0.1 and sets `TRYGALLE_PI_TESTS=on`.
- **Mock model server** registers through `models.json` with `api: "openai-completions"` and `baseUrl` pointing at an `httptest` server. Provider key `mock`; if Pi 1.0.1 requires a known provider id, fall back to key `ollama` with the same `baseUrl` override — verified in Task 6 with a pinned assertion either way.
- **Zero third-party dependencies**: the whole component uses the Go standard library only.
- **Logging**: `log/slog` with lifecycle metadata by request/operation identifier. No prompt bodies, tool output, or dialog content. The full schema is owned by follow-up #5.
- **Degraded start**: the startup args with the `--continue` or `-c` token (standalone argv element) filtered out. The full argv stays configurable for the image design.
- **REPL demo surface**: `cmd/trygalle-repl` is a dev-only binary; it is not the container main process. The production `cmd/trygalle` binary lands with the Telegram design.
- **Constants**: startup validation deadline 60 s; heartbeat interval 5 min (configurable); shutdown deadline configurable, default 30 s, finalized by the Kubernetes design.

### Implementation Constraints
- The approved design is the source of truth for WHAT. Its three deviations from the parent design (steer-not-busy, immediate `/new`, steer acknowledgment) are settled; do not revisit them.
- Pi is the authority on session and run state. Never add session-file parsing, command expansion, queueing, or completion detection as competing sources of behavior.
- Subscribe to events before the first command is sent (a fast completion can emit events before a late subscriber attaches).
- Read stdout continuously so Pi is never stalled; the client read goroutine always drains, and a full event channel is treated as a coordinator bug (protocol integrity failure), never as silent drop.
- Add a `deliberate:` comment when an intentional simplification has a known limit; name the limit and the upgrade path.
- Public-repository hygiene: no private harness names, paths, or credentials in code, tests, fixtures, or logs. The test extension and mock model are synthetic and public-safe.
- Do not use `bufio.Scanner` with its default 64 KiB limit anywhere in the framing path.

### Security Requirements
- Log no prompt bodies, tool output, or dialog content at this layer; log the extension-UI request method only.
- The single-active-run posture is preserved: steering joins the same run; `clear_queue` + `abort` bounds what one prompt can cause.
- The REPL accepts no secrets in argv or environment logging; it reads only text lines.

### Observability Requirements
- `slog` records: Pi start/exit, resumed session identifier, RPC command outcomes and dispositions, operation start/settle/fail/abort/supersede, heartbeat activity summary, extension-UI cancellations (method only), protocol integrity failures, shutdown sequence. Payload and redaction policy is follow-up #5; this component logs metadata only.

### Failure Modes to Handle
| Failure | Expected behavior | Verification |
|---|---|---|
| Unparseable or non-conforming stdout record | Log metadata (no content); fatal exit | Unit test (R1, R16) |
| Pi `parse` error response (no request ID) | Log; not fatal | Unit test (R16) |
| Pi process exits unexpectedly | Runtime returns fatal error; REPL exits non-zero | Live test (R16) |
| Startup validation fails once | Retry once with the same argv | Unit test (R15) |
| Startup validation fails twice | Restart without `--continue`; loud system notice; log entry | Unit test (R15) |
| Degraded start also fails | Fatal error; pod restart is the recovery | Unit test (R15) |
| `prompt` fails during compaction | Retry once with bare `steer` | Unit + live test (R6) |
| Retry also fails | Routing-failure system notice; never a silent drop | Unit test (R7) |
| Run settles with no text | Empty-response system notice | Unit + live test (R7) |
| Run fails (`stopReason: "error"`) | Failure notice with Pi's error message; no automatic retry | Live test (R9) |
| `new_session` canceled by an extension | Report cancellation; keep the current session | Live test (R13) |
| SIGTERM during an active run | `clear_queue` → `abort` → stdin close → bounded exit; exit 0 | Live test (R17) |

### Rollout and Rollback
- Rollout: implementation lands locally behind live contract tests; no Telegram or Kubernetes credentials are involved. Rollback: revert the commits; no durable state exists (the component stores nothing). No schema or data migration.
- Owner: Matteo (sole user, operator, maintainer).

### Test Strategy
- **Live contract tests** (primary): real Pi 1.0.1, temporary `PI_CODING_AGENT_DIR` (synthetic agent dir with generated `models.json` + test extension), temporary `--session-dir`, mock model server via `httptest`. Cover the design's 13 contract tests. Gated by `TRYGALLE_PI_BIN`/`TRYGALLE_PI_TESTS`. Use external test packages (`package rpc_test`, `package runtime_test`).
- **Unit tests**: framing invariants, garbage stdout bytes, deadline expiry, and the coordinator state machine through a scripted fake client (the design's "protocol fixture" allowance). The fake client implements the same record types; it does not reimplement Pi behavior.
- Bound resource use: one Pi process per live test, temp dirs under `t.TempDir()`, no parallel live tests unless isolated per test.
- The narrow command expected to fail before implementation: `go test ./...` (no Go module exists yet).

---

## Protocol Record Schemas
Pin these schemas in `internal/rpc/records.go` exactly. Source: Pi 1.0.1 docs `rpc.md`, `rpc-commands.md`, `json.md`, `rpc-extension-ui.md`, `message-types.md` (installed at `$PI_CODING_AGENT_DIR/../docs` under the package install; the design's "Confirmed protocol facts" section is the behavioral contract).

**Command envelope** (stdin): `{"id":"<string>","type":"<command>"}` plus command fields. The `id` is optional but always set by Trygalle; the response repeats it.

**Response envelope** (stdout):
```json
{"id":"req-1","type":"response","command":"prompt","success":true,"data":{...}}
{"id":"req-3","type":"response","command":"set_model","success":false,"error":"..."}
```
A `parse` response has no `id`: `{"type":"response","command":"parse","success":false,"error":"..."}`.

**Command data shapes consumed by Trygalle:**
- `prompt` (send): `{"id":"...","type":"prompt","message":"...","streamingBehavior":"steer"}`; response `data.disposition` ∈ `"started" | "queued" | "handled"`.
- `steer` (send, fallback during compaction): `{"id":"...","type":"steer","message":"..."}`; response `data.disposition` ∈ `"queued" | "handled"`.
- `abort` (send): response has `success` only.
- `clear_queue` (send): response `data` = `{"steering":["..."],"followUp":["..."]}`.
- `new_session` (send): response `data` = `{"cancelled":false}` or `{"cancelled":true}`.
- `get_state` (send): response `data` includes `sessionId`, `sessionFile`, `isStreaming`, `isCompacting`, `pendingMessageCount`, `messageCount`, `model` (optional full model object).
- `get_commands` (send): response `data.commands` is a non-empty list of `{name, description?, source: "extension"|"prompt"|"skill", sourceInfo}`.
- `get_last_assistant_text` (send): response `data` = `{"text":"..."}`; `text` is `null` when no assistant text exists.

**Events consumed by Trygalle** (no `id`): `agent_start`; `agent_end` with `messages` and `willRetry`; `agent_settled`; `turn_start`; `turn_end` with `message`; `message_start`/`message_update`/`message_end` with `message`; compaction events; retry events; queue events. The coordinator uses events for terminal-state classification and heartbeat activity only, never for final text.

**Assistant message failure fields** (inside `message` and `agent_end.messages`): `stopReason` ∈ `"pending" | "stop" | "length" | "toolUse" | "error" | "aborted" | "deferred"` and optional `errorMessage`. A failed run is classified from the last assistant message with `stopReason: "error"` and its `errorMessage`.

**Extension UI** (both directions):
```json
{"type":"extension_ui_request","id":"uuid-1","method":"select","title":"...","options":["..."],"timeout":10000}
{"type":"extension_ui_response","id":"uuid-1","cancelled":true}
```
Dialog methods: `select`, `confirm`, `input`, `editor` (expect a response; optional `timeout` auto-resolves on the Pi side). Fire-and-forget methods: `notify`, `setStatus`, `setWidget`, `setTitle`, `set_editor_text` (no response).

---

## Pinned Interfaces
Pin these Go types exactly; they are the seams between packages and the future Telegram transport.

**Runtime client seam** (`internal/runtime/client.go`; `*rpc.Client` satisfies it; unit tests use a scripted fake):
```go
type PiClient interface {
	Prompt(ctx context.Context, message string) (Disposition, error) // prompt with streamingBehavior "steer"
	Steer(ctx context.Context, message string) (Disposition, error)
	GetState(ctx context.Context) (State, error)
	GetCommands(ctx context.Context) ([]CommandInfo, error)
	Abort(ctx context.Context) error
	ClearQueue(ctx context.Context) (QueueContents, error)
	NewSession(ctx context.Context) (cancelled bool, err error)
	GetLastAssistantText(ctx context.Context) (*string, error) // nil pointer == JSON null
	Events() <-chan Event
	UIRequests() <-chan UIRequest
	AnswerUIDialog(ctx context.Context, id string) error // extension_ui_response with cancelled: true
	CloseStdin() error
	Wait() error  // returns on process exit or protocol integrity failure
}
```

**Notifier seam** (`internal/runtime/notices.go`; the REPL implements it now, the Telegram transport later):
```go
type Notifier interface {
	Response(ctx context.Context, text string)
	Notice(ctx context.Context, notice Notice)
}
```
`Notice` carries a `NoticeKind` and a small data payload; wording is owned by the Telegram design. Kinds: `Status`, `SteerAcknowledged`, `EmptyResponse`, `FailedRun`, `AbortReport`, `NewSessionReport`, `SessionResumeFailed`, `Heartbeat`, `RoutingFailure`, `RestartNotice`.

**Config** (`internal/runtime/config.go`):
```go
type Config struct {
	PiBin  string   // default "pi"
	Args   []string // e.g. ["--mode","rpc","--continue","--session-dir",dir]
	SessionDir string
	StartupValidationTimeout time.Duration // default 60s
	HeartbeatInterval        time.Duration // default 5m
	ShutdownTimeout          time.Duration // default 30s; finalized by the K8s design
}
```
Degraded args = `Args` with a standalone `"--continue"` or `"-c"` element removed.

**REPL entry seam**: `runtime.HandleUserInput(ctx context.Context, text string)` classifies reserved commands (`/status`, `/abort`, `/new`, exact match) and routes everything else per the design's routing table.

---

## Acceptance Criteria
### Requirement R1: JSONL framing integrity
The RPC layer SHALL write one complete JSON object per record, LF-terminated, and SHALL split stdout records only on LF, strip an optional preceding CR, accept arbitrary record length, and treat an unparseable record as a protocol integrity failure.
#### Scenario: Unicode separator inside a JSON string
- GIVEN a stdout record whose JSON string value contains `U+2028` (`\xe2\x80\xa8`) or `U+2029` (`\xe2\x80\xa9`)
- WHEN the scanner reads the record
- THEN the record decodes as one line and the separators do not split it
#### Scenario: Oversize record
- GIVEN a 1 MiB single-line JSON record
- WHEN the scanner reads it
- THEN it decodes without error (no 64 KiB-style cap)
#### Scenario: Garbage record
- GIVEN a stdout line that is not valid JSON
- WHEN the scanner reads it
- THEN it returns a typed protocol integrity error and the record is never silently skipped

### Requirement R2: Command correlation and subscription ordering
The client SHALL correlate responses by request ID, subscribe to events before the first command is sent, and enforce a caller-supplied deadline per command.
#### Scenario: Fast completion before send returns
- GIVEN a scripted event stream that emits `agent_settled` before the prompt response arrives
- WHEN the coordinator subscribes and then sends `prompt`
- THEN the event is observed (no lost fast completion)
#### Scenario: Deadline expiry
- GIVEN a command whose response never arrives
- WHEN the caller's context deadline passes
- THEN `Send` returns a deadline error

### Requirement R3: Full prompt round-trip
The system SHALL complete a prompt cycle: `prompt{steer}` accepted, `agent_settled` observed, final text from `get_last_assistant_text`.
#### Scenario: Live round-trip
- GIVEN a live Pi with the mock model server returning scripted text
- WHEN a prompt is sent
- THEN the disposition is `"started"`, `agent_settled` follows, and `get_last_assistant_text` returns the scripted text

### Requirement R4: Settlement semantics
The coordinator SHALL treat only `agent_settled` as operation completion; `agent_end` (including `willRetry: true`) SHALL NOT complete an operation.
#### Scenario: Retry then settle
- GIVEN a live Pi where the mock model fails once then succeeds
- WHEN the prompt runs
- THEN `agent_end` with `willRetry: true` is observed first, the retry runs, and completion is detected only at `agent_settled`

### Requirement R5: Single routing rule
Every non-reserved message SHALL be routed through `prompt` with `streamingBehavior: "steer"`, in every state.
#### Scenario: Idle
- GIVEN an idle session
- WHEN a prompt is sent
- THEN the disposition is `"started"`
#### Scenario: Active run
- GIVEN an active run
- WHEN a second prompt is sent
- THEN the disposition is `"queued"` and a steer-acknowledgment notice is emitted
#### Scenario: Extension command mid-run
- GIVEN an active run
- WHEN an extension command is sent
- THEN the disposition is `"handled"` and the command executes immediately

### Requirement R6: Compaction fallback
When `prompt` fails during compaction, the coordinator SHALL retry once with bare `steer`; if that also fails, it SHALL emit a routing-failure notice and never drop the input silently.
#### Scenario: Live compaction window
- GIVEN a live Pi compacting mid-run
- WHEN a prompt is sent during compaction
- THEN the prompt errors, the bare `steer` retry succeeds as queued, and the message is delivered after compaction ends

### Requirement R7: No silent terminal state
Every terminal state SHALL produce a visible outcome through the `Notifier`.
#### Scenario: Settled with no text
- GIVEN a run that settles without assistant text
- WHEN settlement is detected
- THEN an `EmptyResponse` notice is emitted, not silence

### Requirement R8: Handled-no-run guard
After a `"handled"` disposition with no following run events, the coordinator SHALL complete the operation immediately without calling `get_last_assistant_text`.
#### Scenario: Stale-text guard
- GIVEN a prior run produced assistant text and a new prompt is `"handled"` with no run events
- WHEN the operation completes
- THEN the empty/handled notice is emitted and the previous run's text is never returned

### Requirement R9: Failed-run classification
A run whose last assistant message has `stopReason: "error"` SHALL produce a `FailedRun` notice carrying Pi's `errorMessage`, with no automatic retry.
#### Scenario: Live failed run
- GIVEN a live Pi where the mock model returns a persistent error
- WHEN the run settles
- THEN the failure notice contains the error message and no retry loop starts

### Requirement R10: Heartbeat
While an operation is active, the coordinator SHALL emit a heartbeat notice every configured interval with elapsed time, the type and age of the last observed event (metadata only), and an abort hint.
#### Scenario: Active operation
- GIVEN an operation active longer than the heartbeat interval
- WHEN the interval elapses
- THEN one heartbeat notice is emitted with event metadata and no message content

### Requirement R11: `/status`
`/status` SHALL send `get_state` and return a status summary available while an operation is active.
#### Scenario: During a run
- GIVEN an active operation
- WHEN `/status` is sent
- THEN a status summary with session and streaming state is emitted

### Requirement R12: `/abort` Esc parity
`/abort` SHALL run `clear_queue` before `abort`, then report the abort result and the dropped message texts.
#### Scenario: Queued steering dropped
- GIVEN an active run with a queued steering message
- WHEN `/abort` is sent
- THEN the steering text is reported as dropped and does not run after the abort

### Requirement R13: `/new` immediate execution
`/new` SHALL run `clear_queue` then `new_session` immediately, supersede any active operation, and report the new-session result plus dropped texts.
#### Scenario: Mid-run supersede
- GIVEN an active run
- WHEN `/new` is sent
- THEN the operation is superseded, trailing events are logged but not attributed, and no steering survives the switch
#### Scenario: Extension cancels the switch
- GIVEN a `session_before_switch` handler that cancels
- WHEN `/new` is sent
- THEN the cancellation is reported and the current session is kept

### Requirement R14: Extension UI policy
The runtime SHALL answer every dialog request (`select`, `confirm`, `input`, `editor`) immediately with `cancelled: true`, log the request method only, and log fire-and-forget requests as metadata without delivery.
#### Scenario: Live dialog
- GIVEN a live Pi running a test extension that opens a dialog
- WHEN the dialog request arrives
- THEN the answer is sent immediately, the extension receives the cancellation, and the run settles without hanging

### Requirement R15: Startup validation and degraded resume ladder
The runtime SHALL validate startup (`get_state` with a session identifier, `get_commands` non-empty) under a 60 s deadline, retry once on failure, then restart without `--continue` with a loud `SessionResumeFailed` notice; a degraded-start failure SHALL be fatal.
#### Scenario: Resume latest session
- GIVEN a session directory with an existing session created by a previous run
- WHEN the runtime restarts with `--continue`
- THEN `get_state` reports the same session identifier
#### Scenario: Damaged latest session
- GIVEN a latest session with a partial trailing line, or a wholly unreadable session file
- WHEN the runtime restarts
- THEN startup succeeds (Pi skips malformed lines and excludes unreadable files) or the degraded ladder bounds the failure to one fresh-session restart

### Requirement R16: Fatal and non-fatal protocol failures
An unexpected Pi exit or protocol integrity failure SHALL be fatal to Trygalle; a Pi `parse` error response SHALL be logged and not fatal.
#### Scenario: Unexpected exit
- GIVEN a live Pi killed mid-session
- WHEN the process exits
- THEN the runtime surfaces a fatal error and the REPL exits non-zero
#### Scenario: Pi rejects a malformed command from Trygalle
- GIVEN a `parse` response with no request ID
- WHEN it arrives
- THEN it is logged and the client keeps running

### Requirement R17: Deterministic shutdown
On SIGTERM, the runtime SHALL run `clear_queue` → `abort` (when an operation is active), close stdin, and wait for exit under the shutdown deadline, exiting 0 either way; the restart notice is best-effort and never blocks shutdown.
#### Scenario: SIGTERM during an active run
- GIVEN an active run with a delayed mock model response
- WHEN SIGTERM arrives
- THEN the ordered sequence runs, Pi exits within the deadline, and the process exits 0

### Requirement R18: REPL demo surface
The REPL SHALL route stdin lines through the coordinator entry seam, print responses and notices to stdout, diagnostics to stderr, and exit 0 on clean EOF or SIGTERM.
#### Scenario: Manual walkthrough
- GIVEN a started REPL with a live Pi and mock model
- WHEN the user sends a prompt, a mid-run steering message, `/status`, `/abort`, and `/new`
- THEN every input produces a visible outcome per the terminal-state table

---

## Task Sequence
### Slice 1: Protocol framing and RPC client (smallest user-feedback slice)
Delivers the live `get_state`/`get_commands` round-trip against the real installed Pi.

### Task 1: Go module and LF-only framing
**Delivers:** `internal/rpc` framing primitives with unit tests proving the framing invariants.
**Blocked by:** None
**Traces to:** R1
**Files:** `go.mod`, `internal/rpc/framing.go`, `internal/rpc/framing_test.go`

- [ ] Create `go.mod` for module `github.com/maruina/trygalle` with `go 1.27`.
- [ ] Implement the record scanner: read a byte stream, split records only on LF (`0x0a`), strip one optional preceding CR (`0x0d`), return each raw record line. No record-length limit.
- [ ] Implement the record writer: marshal one JSON object, append LF, write with a full-write loop (short writes retried).
- [ ] Unit-test: LF split; CR strip; `U+2028`/`U+2029` bytes inside a JSON string do not split; a 1 MiB record decodes; a non-JSON line returns a typed `ProtocolError` (define it here).
- [ ] Run `go test ./internal/rpc/` and `go vet ./...`; expect all green.
- [ ] Commit with `feat: add JSONL record framing for the pi rpc protocol`.

### Task 2: Protocol record types
**Delivers:** Decoding of every record family Trygalle consumes, pinned to the schemas in this plan.
**Blocked by:** Task 1
**Traces to:** R1, R2
**Files:** `internal/rpc/records.go`, `internal/rpc/records_test.go`

- [ ] Implement `Command` builders (send-side), `Response` envelope (success/error, `command`, `data`, optional `id`), `Event` (type plus the fields the coordinator consumes: `agent_start`, `agent_end{messages,willRetry}`, `agent_settled`, `turn_*`, `message_*`, compaction, retry, queue events), `UIRequest`/`UIResponse`, and the command-data structs from "Protocol Record Schemas".
- [ ] Represent `get_last_assistant_text`'s `text` so JSON `null` is distinguishable from empty string (e.g. `*string`).
- [ ] Unit-test each struct against a golden JSON literal copied from the schemas section, including the `parse` response without `id` and a `text: null` payload.
- [ ] Run `go test ./internal/rpc/`; expect green.
- [ ] Commit with `feat: add pi rpc protocol record types`.

### Task 3: RPC client
**Delivers:** The client that starts Pi, correlates by ID, subscribes before send, enforces deadlines, answers extension UI, and closes stdin.
**Blocked by:** Task 2
**Traces to:** R2, R16
**Files:** `internal/rpc/client.go`, `internal/rpc/client_test.go`

- [ ] Implement `Client`: start the process (`exec.Command`); a read goroutine that always drains stdout, decodes records, routes responses by ID to waiting `Send` calls, delivers events and UI requests on buffered channels (event buffer 1024; a full channel is a `ProtocolError`, never a drop); surface process exit and `ProtocolError` via `Wait`.
- [ ] Establish the events/UI subscriptions at construction, before any `Send` can run.
- [ ] Implement `Send` with a per-call `context.Context` deadline, unique incrementing IDs, `AnswerUIDialog` (`extension_ui_response` with `cancelled: true`), and `CloseStdin` + bounded `Wait`.
- [ ] Route a `parse` response (no ID) to a log record, not an error.
- [ ] Unit-test with in-memory scripted streams (no process): correlation, fast-completion-before-send (R2 scenario), deadline expiry, garbage line → `ProtocolError`, `parse` response → logged, extension-UI answer written to stdin.
- [ ] Run `go test ./internal/rpc/ -race`; expect green.
- [ ] Commit with `feat: add pi rpc client with id correlation and event subscription`.

### Task 4: Live-test gate and first live round-trip
**Delivers:** The `internal/harness` gate and the live `get_state`/`get_commands` round-trip — the smallest user-feedback slice.
**Blocked by:** Task 3
**Traces to:** R2, R15 (validation command subset)
**Files:** `internal/harness/harness.go`, `internal/harness/harness_test.go`, `internal/rpc/live_test.go`

- [ ] Implement the gate: `harness.Pi(t)` resolves `TRYGALLE_PI_BIN` (default `pi` from `PATH`) and applies `TRYGALLE_PI_TESTS` semantics (`auto`: skip with instructions when unavailable; `on`: `t.Fatalf`; `off`: skip).
- [ ] Implement `harness.Start`: temp `PI_CODING_AGENT_DIR` (empty synthetic agent dir), temp `--session-dir` under `t.TempDir()`, `PI_SKIP_VERSION_CHECK=1`, and `PI_OFFLINE=1` (drop `PI_OFFLINE` if it blocks mock model calls in Task 5), start args `["--mode","rpc","--session-dir",dir]`, return a started `*rpc.Client` plus cleanup.
- [ ] Live-test (in `package rpc_test`): send `get_state`; expect `success: true` and a non-empty `sessionId`; send `get_commands`; expect a non-empty command list.
- [ ] Run `go test ./internal/rpc/ -run Live`; with `pi` on PATH expect green, and with `TRYGALLE_PI_TESTS=off` expect skips.
- [ ] Commit with `test: add live-test gate and get_state round-trip`.

### Slice 2: Hermetic live-Pi harness
Delivers the mock model server and test extension that make every later live contract test hermetic.

### Task 5: Mock model server and generated models.json
**Delivers:** An `httptest` `openai-completions` server that real Pi accepts, registered through generated `models.json`, with scripted behaviors.
**Blocked by:** Task 4
**Traces to:** R3, R4, R9
**Files:** `internal/harness/mockmodel.go`, `internal/harness/harness.go` (extend), `internal/harness/mockmodel_test.go`

- [ ] Implement the server on an `httptest.Listener`: `POST {base}/chat/completions`. Scripted behaviors: `RespondText(s)`, `RespondEmptyText`, `FailOnce` (HTTP 500 then text), `FailAlways`, `Delay(d)` (holds the run open).
- [ ] Generate `models.json` in the temp agent dir: provider `mock`, `api: "openai-completions"`, `apiKey: "mock"`, `baseUrl` = mock server URL, one model `mock-model`. If Pi 1.0.1 rejects an unknown provider id, switch the key to `ollama` with the same `baseUrl` and note it in a `deliberate:` comment.
- [ ] Harness start args gain `--provider mock --model mock-model` (or the working equivalent; pin the verified form in the harness).
- [ ] Live self-test: start Pi through the harness in request-logging mode, send one prompt, capture the exact request (path, headers, body, `stream` flag) in the test log, then implement the response side against the observed shape (SSE chunks when `stream: true`, plain JSON otherwise) and assert one scripted round-trip completes.
- [ ] Run `go test ./internal/harness/`; expect green.
- [ ] Commit with `test: add openai-completions mock model server and generated models.json`.

### Task 6: Live prompt round-trip, retries, and edge responses
**Delivers:** A live Pi completing a full prompt cycle through the mock model — hermetic, no credentials.
**Blocked by:** Task 5
**Traces to:** R3, R4, R5 (idle disposition), R7, R9
**Files:** `internal/harness/live_roundtrip_test.go`

- [ ] Live-test: send `prompt{steer}`; expect disposition `"started"`; consume events until `agent_settled`; call `get_last_assistant_text`; expect the scripted text.
- [ ] Live-test retry semantics: `FailOnce`; expect `agent_end` with `willRetry: true`, then the retry, then `agent_settled`; assert completion detection keys only on `agent_settled`.
- [ ] Live-test empty text: `RespondEmptyText`; record what `get_last_assistant_text` returns (`null` or empty string) and assert it — this pins the producibility of the no-text terminal state (the coordinator maps it per the Task 9 unit test).
- [ ] Live-test failed run: `FailAlways`; expect the run to settle with an assistant message carrying `stopReason: "error"` and an `errorMessage` in the event stream.
- [ ] Run `go test ./internal/harness/`; expect green.
- [ ] Commit with `test: pin live prompt round-trip and edge responses`.

### Task 7: Test extension fixture
**Delivers:** A synthetic public-safe Pi extension providing the interaction surfaces later tests need.
**Blocked by:** Task 6
**Traces to:** R5 (handled), R13, R14
**Files:** `internal/harness/testdata/agent/extension.js`, `internal/harness/extension_test.go`

- [ ] Write the extension (consult Pi's `extensions.md` for the exact factory API): register `/mock-dialog` (opens `ctx.ui.select` with a long timeout), `/mock-notify` (calls `ctx.ui.notify`, starts no run), `/mock-run` (starts its own run via `pi.sendMessage`), and a `session_before_switch` handler that cancels only when the process env `MOCK_CANCEL_NEW_SESSION=1`.
- [ ] Wire the harness to copy the extension into the temp agent dir.
- [ ] Live-test: `get_commands` lists the three commands; sending `/mock-notify` via `prompt` yields disposition `"handled"` with no run events.
- [ ] Run `go test ./internal/harness/`; expect green.
- [ ] Commit with `test: add synthetic pi extension fixture`.

### Slice 3: Runtime core — routing, operation state machine, heartbeat, minimal REPL
Delivers the coordinator that routes every message and produces a visible outcome for every terminal state, plus the prompt-only REPL.

### Task 8: Config, notices, and the client seam
**Delivers:** `internal/runtime` foundations: `Config` with defaults and degraded-args filtering, `Notifier`/`Notice`, and the `PiClient` seam.
**Blocked by:** Task 3 (types only; no live need)
**Traces to:** R7 (notice vocabulary), Implementation Contract constants
**Files:** `internal/runtime/config.go`, `internal/runtime/notices.go`, `internal/runtime/client.go`, `internal/runtime/config_test.go`

- [ ] Implement `Config` exactly as pinned; defaults: validation 60 s, heartbeat 5 m, shutdown 30 s. Implement degraded-args filtering (remove a standalone `"--continue"` or `"-c"` element) with a unit test.
- [ ] Implement `Notice`, `NoticeKind` (all ten kinds), and the `Notifier` interface as pinned.
- [ ] Define the `PiClient` interface as pinned; add a compile-time assertion that `*rpc.Client` satisfies it.
- [ ] Run `go test ./internal/runtime/`; expect green.
- [ ] Commit with `feat: add runtime config, notices, and client seam`.

### Task 9: Coordinator event loop and settled terminal states
**Delivers:** The operation state machine for the `prompt{steer}` happy path: idle → running → settled, with text, no-text, and failure outcomes.
**Blocked by:** Task 8
**Traces to:** R4, R5, R7, R9
**Files:** `internal/runtime/coordinator.go`, `internal/runtime/coordinator_test.go`

- [ ] Implement `HandleUserInput`: exact-match reserved names (`/status`, `/abort`, `/new`) are reserved (routed in Slice 4); everything else goes to `Prompt` (`prompt` with `streamingBehavior: "steer"`).
- [ ] Implement the event loop: subscribe before send; on `"started"` enter Running; on `agent_settled` call `GetLastAssistantText` (nil → `EmptyResponse` notice; text → `Response`); classify a failed run from the last assistant message with `stopReason: "error"` and its `errorMessage` (`FailedRun` notice, no retry).
- [ ] Unit-test with a scripted fake `PiClient`: started → settled-with-text; settled-with-null-text; failed-run classification; fast-completion ordering (settled event scripted before the prompt response).
- [ ] Run `go test ./internal/runtime/`; expect green.
- [ ] Commit with `feat: add coordinator operation loop and settled terminal states`.

### Task 10: Handled guard, steer acknowledgment, compaction fallback
**Delivers:** The remaining prompt-path behaviors: `"handled"` with and without a following run, the steer acknowledgment notice, and the compaction-window fallback.
**Blocked by:** Task 9
**Traces to:** R5, R6, R7, R8
**Files:** `internal/runtime/coordinator.go` (extend), `internal/runtime/coordinator_test.go` (extend)

- [ ] On `"handled"`: if run events follow, track them to `agent_settled`; if none follow, complete immediately with a notice and never call `GetLastAssistantText` (stale-text guard).
- [ ] On `"queued"`: emit `SteerAcknowledged`.
- [ ] On `Prompt` error: retry once with `Steer` (bare `steer`); on second failure emit `RoutingFailure`; never drop the input silently.
- [ ] Unit-test with the fake client: handled-no-run → immediate completion without `GetLastAssistantText` (assert it is never called); handled-then-run → tracked to settled; queued → acknowledgment notice; prompt error → steer retry → queued; both fail → `RoutingFailure`.
- [ ] Run `go test ./internal/runtime/`; expect green.
- [ ] Commit with `feat: add handled guard, steer acknowledgment, and compaction fallback`.

### Task 11: Heartbeat
**Delivers:** Periodic activity notices during an active operation.
**Blocked by:** Task 9
**Traces to:** R10
**Files:** `internal/runtime/heartbeat.go`, `internal/runtime/heartbeat_test.go`

- [ ] While Running, tick every `HeartbeatInterval`: record elapsed time, the type and age of the last observed event (metadata only, no content), and an abort hint; emit a `Heartbeat` notice. Stop ticking on every terminal state.
- [ ] Unit-test with a tiny configured interval and a delayed fake run: exactly one notice per interval elapse, correct last-event metadata, ticking stops after settlement.
- [ ] Run `go test ./internal/runtime/`; expect green.
- [ ] Commit with `feat: add operation heartbeat`.

### Task 12: Minimal REPL
**Delivers:** The prompt-only demo surface: stdin lines through the coordinator, stdout `Notifier`, clean EOF handling. Reserved commands route in Slice 4.
**Blocked by:** Task 11
**Traces to:** R18 (prompt subset)
**Files:** `cmd/trygalle-repl/main.go`

- [ ] Flags: `--pi-bin`, `--session-dir` (required), `--no-continue`, `--heartbeat-interval`, `--shutdown-timeout`, `--help` with an example. stdout carries responses and notices (notices prefixed to distinguish them); diagnostics on stderr. Exit codes: 0 clean EOF, 1 fatal runtime error.
- [ ] Read lines from stdin, call `runtime.HandleUserInput`; SIGINT/SIGTERM wiring lands in Task 20 (note it in `--help`).
- [ ] Manual check: `go run ./cmd/trygalle-repl --session-dir /tmp/trygalle-demo` with a live Pi and a real model; send one prompt; observe the answer. This is a developer convenience check, not the contract gate.
- [ ] Run `go vet ./...`; expect clean.
- [ ] Commit with `feat: add prompt-only repl demo binary`.

### Slice 4: Reserved commands
Delivers `/status`, `/abort`, and `/new` with TUI-parity semantics.

### Task 13: `/status`
**Delivers:** Status routing and summary.
**Blocked by:** Task 9
**Traces to:** R11
**Files:** `internal/runtime/coordinator.go` (extend), `internal/runtime/coordinator_test.go` (extend), `internal/runtime/status_live_test.go`

- [ ] Route `/status` to `GetState`; emit a `Status` notice with `sessionId`, `isStreaming`, `isCompacting`, `pendingMessageCount`, `messageCount`, and the model id when present. No wording at this layer (Telegram design owns it).
- [ ] Unit-test with the fake client, including the active-operation case (status must not disturb the running operation).
- [ ] Live-test: during a delayed mock run, send `/status`; expect the summary with `isStreaming: true`.
- [ ] Run `go test ./internal/...`; expect green.
- [ ] Commit with `feat: add /status routing`.

### Task 14: `/abort`
**Delivers:** Esc-parity abort: `clear_queue` before `abort`, with dropped-text reporting.
**Blocked by:** Task 9
**Traces to:** R12
**Files:** `internal/runtime/coordinator.go` (extend), `internal/runtime/coordinator_test.go` (extend), `internal/runtime/abort_live_test.go`

- [ ] Route `/abort`: `ClearQueue` → `Abort` → `AbortReport` notice carrying the dropped steering and follow-up texts. If no operation is active, still run the sequence (mirrors Esc on an idle session) and report accordingly.
- [ ] Unit-test the ordering (assert `ClearQueue` precedes `Abort`) and the dropped-texts payload with the fake client.
- [ ] Live-test (design contract tests 1 and 3): queue a steering message mid-run via `prompt` with `streamingBehavior: "steer"` and assert disposition `"queued"`; then pin that `abort` alone continues queued steering (assert the queued message runs); then assert `clear_queue` before `abort` leaves nothing queued.
- [ ] Run `go test ./internal/...`; expect green.
- [ ] Commit with `feat: add /abort with esc parity`.

### Task 15: `/new`
**Delivers:** Immediate new-session: supersede, queue clear, cancellation report.
**Blocked by:** Task 9
**Traces to:** R13
**Files:** `internal/runtime/coordinator.go` (extend), `internal/runtime/coordinator_test.go` (extend), `internal/runtime/new_session_live_test.go`

- [ ] Route `/new`: `ClearQueue` → `NewSession`; on success enter Superseded, emit `NewSessionReport` with dropped texts, log (not attribute) trailing events, return to Idle after reporting. On `cancelled: true`, report the cancellation and keep the current session.
- [ ] Unit-test with the fake client: mid-run supersede (trailing `agent_end` logged, not attributed); cancelled switch; empty-queue dropped texts.
- [ ] Live-tests (design contract tests 4 and 5): mid-run `/new` — assert trailing events are observed and logged, and the steering queue is empty after the switch (a follow-up `clear_queue` returns empty lists); `MOCK_CANCEL_NEW_SESSION=1` — assert `cancelled: true`, session kept.
- [ ] Run `go test ./internal/...`; expect green.
- [ ] Commit with `feat: add /new with supersede semantics`.

### Slice 5: Extension UI policy and compaction live test
Delivers dialog cancellation and the live compaction-window proof.

### Task 16: Extension UI policy
**Delivers:** Immediate dialog cancellation and metadata-only logging of fire-and-forget requests.
**Blocked by:** Tasks 7, 9
**Traces to:** R14
**Files:** `internal/runtime/coordinator.go` or `internal/runtime/ui.go`, `internal/runtime/ui_test.go`, `internal/runtime/ui_live_test.go`

- [ ] Subscribe to UI requests: dialog methods (`select`, `confirm`, `input`, `editor`) → `AnswerUIDialog` immediately; log the method only. Fire-and-forget methods (`notify`, `setStatus`, `setWidget`, `setTitle`, `set_editor_text`) → log method as metadata; no delivery.
- [ ] Unit-test with the fake client: each dialog method answered with `cancelled: true`, no content logged (assert the log record's fields), fire-and-forget not answered.
- [ ] Live-tests (design contract test 10): `/mock-dialog` mid-run — assert the dialog is answered, the extension receives the cancellation, and the run settles without hanging; `/mock-notify` — assert a log record with the method name and no Telegram-style delivery.
- [ ] Run `go test ./internal/...`; expect green.
- [ ] Commit with `feat: add extension ui cancellation policy`.

### Task 17: Live compaction-window test
**Delivers:** The live proof of the compaction fallback and steer-during-compaction delivery.
**Blocked by:** Tasks 6, 10
**Traces to:** R6, R5 (mid-run dispositions)
**Files:** `internal/runtime/compaction_live_test.go`

- [ ] Live-test (design contract test 2): hold a run open with a delayed mock response; trigger compaction (send the `compact` command mid-run; if Pi 1.0.1 rejects mid-run `compact`, trigger it via context overflow — the mock reports usage exceeding the model's context window — and record the working trigger in the test).
- [ ] During compaction: send a prompt; expect the documented error; send a bare `steer`; expect `"queued"`; after compaction ends, assert the steered message is delivered and the run settles.
- [ ] Also assert the mid-run extension-command disposition: send `/mock-run` mid-run; expect `"handled"` and immediate execution (design contract test 1, third case).
- [ ] Run `go test ./internal/harness/ -run Compaction`; expect green.
- [ ] Commit with `test: pin compaction-window and mid-run disposition behavior`.

### Slice 6: Startup lifecycle and session resume
Delivers the validation ladder, restart-resume behavior, and fatal exit wiring.

### Task 18: Startup validation ladder
**Delivers:** Validate → retry once → degraded fresh session → fatal, with notices and logs.
**Blocked by:** Task 8
**Traces to:** R15
**Files:** `internal/runtime/lifecycle.go`, `internal/runtime/lifecycle_test.go`, `cmd/trygalle-repl/main.go` (wire startup)

- [ ] Implement startup: start Pi; `get_state` must succeed with a session identifier under `StartupValidationTimeout`; `get_commands` must succeed with a non-empty list; log the resumed session identifier and command category counts (no names).
- [ ] On validation failure or process exit during validation: retry once with the same argv. Second consecutive failure: restart with degraded args (no `--continue`), emit `SessionResumeFailed` (the loud notice), log the failure. Degraded start failure: return a fatal error (the pod restart is the recovery).
- [ ] Unit-test with an injectable start function and scripted clients: one-strike recovery; two strikes → degraded start (assert the argv dropped `--continue`) and the notice; degraded failure → fatal error; validation deadline expiry → treated as a failure strike.
- [ ] Wire the REPL to run startup before reading input; fatal startup → exit 1 with a diagnostic.
- [ ] Run `go test ./internal/...`; expect green.
- [ ] Commit with `feat: add startup validation ladder with degraded resume`.

### Task 19: Session resume and fatal exits (live)
**Delivers:** Live proof of resume-latest, damaged-session tolerance, and unexpected-exit fatality.
**Blocked by:** Task 18
**Traces to:** R15, R16
**Files:** `internal/runtime/resume_live_test.go`, `internal/runtime/lifecycle_test.go` (extend)

- [ ] Live-test (design contract test 6): (a) empty session dir — first boot succeeds, a fresh session id appears; (b) restart after a completed prompt — the same session id resumes; (c) latest session with a partial trailing line — startup succeeds; (d) a wholly unreadable session file in the dir — startup succeeds (excluded from discovery) or the degraded ladder bounds it to one fresh-session restart.
- [ ] Live-test: kill the Pi process mid-session; assert the runtime surfaces a fatal error (R16 scenario).
- [ ] Unit-test the runtime fatal wiring: `parse` response → logged, not fatal; `ProtocolError` → fatal error returned to the caller.
- [ ] Run `go test ./internal/...` with `TRYGALLE_PI_TESTS=auto`; expect green.
- [ ] Commit with `test: pin session resume and fatal exit behavior`.

### Slice 7: Shutdown, docs, and final verification
Delivers deterministic SIGTERM behavior, the docs, and the feature-level gate.

### Task 20: Shutdown sequence and REPL signal wiring
**Delivers:** Bounded `clear_queue` → `abort` → stdin close → exit under the deadline.
**Blocked by:** Task 14 (abort path), Task 18
**Traces to:** R17
**Files:** `internal/runtime/shutdown.go`, `internal/runtime/shutdown_test.go`, `internal/runtime/shutdown_live_test.go`, `cmd/trygalle-repl/main.go` (signal wiring)

- [ ] Implement `Shutdown(ctx)`: if Running → `ClearQueue`, `Abort`, wait for idle within the deadline; then `CloseStdin`; wait for exit within the deadline; best-effort `RestartNotice` when an operation was aborted (delivery never blocks shutdown); exit 0 either way; log each step.
- [ ] Unit-test with the fake client: command ordering (`clear_queue` before `abort` before stdin close); deadline enforcement with a hanging fake (short deadline → returns without blocking); idle path (no abort commands sent).
- [ ] Live-test (design contract test 12): SIGTERM during a delayed mock run — assert the ordered sequence, Pi exit within the deadline, process exit 0; SIGTERM while idle — stdin close, bounded exit.
- [ ] Wire the REPL: `signal.NotifyContext` (SIGTERM, SIGINT) → `Shutdown` → exit 0.
- [ ] Run `go test ./internal/...`; expect green.
- [ ] Commit with `feat: add deterministic shutdown sequence`.

### Task 21: Documentation
**Delivers:** Durable commands and constraints for future agents.
**Blocked by:** Task 20
**Traces to:** Durable-plan documentation requirement
**Files:** `AGENTS.md`, `README.md`

- [ ] Create `AGENTS.md`: build and test commands; live-test gate (`TRYGALLE_PI_BIN`, `TRYGALLE_PI_TESTS` semantics, CI invocation `TRYGALLE_PI_TESTS=on`); the pinned Pi version (1.0.1) and the contract tests' role as the upgrade evidence gate; REPL usage; the constraint that Pi is the authority on session/run state (no competing implementations); the stdlib-only constraint.
- [ ] Extend `README.md` with a short Development section pointing to `AGENTS.md` and the test commands.
- [ ] Commit with `docs: add agent and development instructions`.

### Task 22: Final verification
**Delivers:** The feature-level acceptance gate that only the completed component satisfies.
**Blocked by:** Tasks 4, 6, 7, 13, 14, 15, 16, 17, 19, 20, 21
**Traces to:** Design success criteria (all)
**Files:** none (verification only)

- [ ] Run `TRYGALLE_PI_TESTS=on go test ./... -race`; expect all live and unit tests green in one run.
- [ ] Run `go vet ./...` and `go fix -diff ./...`; expect clean.
- [ ] Manual REPL walkthrough with a real model (developer machine): send a prompt; send a mid-run steering message (expect acknowledgment then a blended final answer); `/status` mid-run; `/abort` with a queued message; `/new` mid-run; restart the REPL and confirm the same session resumes; send SIGTERM mid-run and confirm a bounded, ordered shutdown. Record outcomes in the PR description.
- [ ] Confirm no private harness names, paths, or credentials appear: `git grep -iE 'ruina|ddoghq|talos|matteo'` beyond unavoidable public identity (module path), and review test fixtures.
- [ ] Commit any resulting fix with `fix: address final verification findings` (or record a clean result with no commit).

---

## Validation Summary
| Requirement | Interface | Seam |
|---|---|---|
| R1 framing | `internal/rpc` unit tests | In-memory byte streams incl. `U+2028`, 1 MiB, garbage |
| R2 correlation/deadline | `internal/rpc` unit tests | Scripted in-memory streams |
| R3–R6 routing, settlement, compaction | Live tests + coordinator unit tests | `internal/harness` (mock model, temp dirs) + fake `PiClient` |
| R7–R10 terminal states, heartbeat | Coordinator unit tests + live subset | Fake `PiClient`; live no-text and failed-run pins in Task 6 |
| R11–R13 reserved commands | Live tests + unit ordering tests | `internal/harness` + fake `PiClient` |
| R14 extension UI | Live test + unit logging assertions | Test extension `/mock-dialog`, `/mock-notify` |
| R15–R16 lifecycle, fatal exits | Unit ladder tests + live resume/kill tests | Injectable start function; `internal/harness` |
| R17 shutdown | Live SIGTERM tests + unit ordering | Mock model `Delay`; fake `PiClient` |
| R18 REPL | Manual walkthrough | `cmd/trygalle-repl` |

**Coverage honesty note:** any terminal state the live tier cannot produce deterministically (candidates: "settled, no text" if the mock model's empty response is normalized away by Pi) gets a scripted-fake unit test instead, named in the relevant task's notes during execution. No coverage gap is left silent.

## Assumptions and Risks
- Pi 1.0.1 is the pinned version; the installed CLI is at `pi` on `PATH` locally. Contract tests double as the upgrade gate.
- The mock model's wire shape is discovered in Task 5 by logging one real request; the OpenAI-compatible surface is small and Pi routes local servers through this path by design (Ollama/vLLM support).
- `mock` as a `models.json` provider key may need the `ollama` fallback (decided in Task 6, pinned in the harness).
- Live tests are slower than unit tests; the suite stays bounded (one Pi process per test, temp dirs).
- The REPL is a dev tool; its behavior is superseded by the Telegram transport in follow-up #2, which implements the same `Notifier` seam.
