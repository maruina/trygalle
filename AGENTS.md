# Agent Instructions

Reference for building, testing, and changing this repository.

## Layout

| Path | Contents |
|---|---|
| `internal/rpc` | JSONL framing, protocol records, and the Pi RPC client |
| `internal/runtime` | Coordinator event loop, operation state machine, reserved commands, startup ladder, shutdown |
| `internal/harness` | Live-test gate, mock model server, and synthetic test extension |
| `cmd/trygalle-repl` | Dev-only REPL demo binary |
| `plans/` | Committed design and plan records |

## Build and test

```bash
go build ./...
go vet ./...
go test ./...            # unit tier; live tests skip without the pinned pi
go test -race ./...
go test -race -short ./...   # what CI's short job runs
go fix -diff ./...
gofmt -l ./cmd ./internal
```

`go fix -diff ./...` must print nothing before a commit; the module targets Go 1.27.

## Live contract tests

Most protocol behavior is proven by contract tests that spawn the real `pi --mode rpc` binary with a mock model server and temporary session directories.

| Setting | Behavior |
|---|---|
| `TRYGALLE_PI_TESTS=auto` (default) | Run live tests when the pinned version is available; skip with instructions when the binary is missing or reports another version |
| `TRYGALLE_PI_TESTS=on` | Fail when the binary is missing or the version differs from the pin |
| `TRYGALLE_PI_TESTS=off` | Skip before any binary lookup |
| `TRYGALLE_PI_BIN` | Binary path; default `pi` from `PATH` |
| `go test -short` | Skips every live test |

A binary that exists but cannot report `--version` fails in every mode.

Run the full suite against the pin:

```bash
npm install --global @earendil-works/pi-coding-agent@1.3.0
TRYGALLE_PI_TESTS=on go test -race ./...
```

## Pinned Pi version

The supported version is `1.3.0`, declared as `harness.PinnedPiVersion`. The live suite is the upgrade evidence gate: changing the pin requires the complete live suite green against the new version, and the pin, the plan records, and `.github/workflows/ci.yml` must move in one change. Do not weaken the version check to make a test pass.

## Architecture constraints

- Pi is the authority on session and run state. Do not add session-file parsing, command expansion, queueing, or completion detection as competing sources of behavior.
- One coordinator event loop is the only consumer of the client event and extension-UI channels, and the only writer of operation state.
- Every reserved command and the shutdown sequence runs as a loop command: the caller performs the bounded RPC sequence and posts the outcome, and the loop alone writes state and emits the notice.
- `agent_settled` is the only completion signal; `agent_end` with `willRetry: true` is not.
- The component uses the Go standard library only. Do not add third-party dependencies.
- Do not use `bufio.Scanner` in the protocol framing path; its default 64 KiB limit breaks long records.

## REPL demo

```bash
go run ./cmd/trygalle-repl --session-dir /tmp/trygalle-demo
```

The REPL is a developer surface, not the container entrypoint. It reads stdin lines, prints Pi responses on stdout, prints `[trygalle]`-prefixed notices on stdout, and writes diagnostics to stderr. It exits 0 on end of input, SIGINT, and SIGTERM; 1 on a fatal runtime error; and 2 on a usage error. Run `--help` for the full flag list.

## CI

`.github/workflows/ci.yml` runs two jobs on every pull request and push to `main`:

- Short: `go test -race -short ./...`, where live tests skip by design.
- Full: installs pi 1.3.0, sets `TRYGALLE_PI_TESTS=on`, and runs `go test -race ./...`.

A daily scheduled run repeats the full suite.
