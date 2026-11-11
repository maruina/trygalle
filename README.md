# trygalle
A lightweight Go bridge that lets you control your Pi harness over Telegram

## Run against a throwaway bot

1. Create a bot with [@BotFather](https://t.me/botfather) and keep its token secret: never commit it, log it, or paste it into code.
2. Start a private chat with the bot and send it any message, then read your numeric user id (for example from [@userinfobot](https://t.me/userinfobot)).
3. Run the bridge with the token and your user id:

```bash
export TRYGALLE_TELEGRAM_TOKEN="123456:Aa..."    # the BotFather token
export TRYGALLE_TELEGRAM_ALLOWED_USER="42"       # your numeric user id
go run ./cmd/trygalle --session-dir /tmp/trygalle
```

The bridge long-polls Telegram, forwards your text messages to one pi subprocess, and sends pi's responses and system notices back to the private chat. `/status`, `/abort`, and `/new` work like the REPL's reserved commands. A message that is not text gets one canned reply.

Optional: `--pi-bin` (default `pi`), `--no-continue` for a fresh session, `--heartbeat-interval` (default `5m`), `--shutdown-timeout` (default `30s`), and `TRYGALLE_TELEGRAM_POLL_TIMEOUT` (default `30s`). `--help` shows the full list.

## Development

Build, test, and live contract-test instructions are in [AGENTS.md](AGENTS.md).
