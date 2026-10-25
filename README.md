# trygalle
A lightweight Go bridge that lets you control your Pi harness over Telegram

## Development

Build, test, and live contract-test instructions are in [AGENTS.md](AGENTS.md).

```bash
go test -race ./...
go run ./cmd/trygalle-repl --session-dir /tmp/trygalle-demo
```
