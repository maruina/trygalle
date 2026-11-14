# trygalle
A lightweight Go bridge that lets you control your Pi harness over Telegram

## Run
The published image contains Trygalle, the pinned Pi, kubectl, and the tools the agent needs, and runs as UID 1000. It takes three inputs: the Telegram bot token, your numeric Telegram user id, and the model-provider API key.

```bash
docker run -d --name trygalle \
  -e TRYGALLE_TELEGRAM_TOKEN="123456:Aa..." \
  -e TRYGALLE_TELEGRAM_ALLOWED_USER="42" \
  -e ANTHROPIC_API_KEY="sk-ant-..." \
  -v trygalle-data:/data/pi \
  ghcr.io/maruina/trygalle:<release>
```

Replace `<release>` with the newest version tag from the repository releases. Use the versioned tag instead of `latest`, so an image pull cannot change the Pi version under running sessions. The `trygalle-data` volume persists sessions and working files under `/data/pi`.

To run it on Kubernetes instead, clone this repository, put the same three inputs in a values file, and install the chart:

```bash
git clone https://github.com/maruina/trygalle
cd trygalle
cat > my-values.yaml <<'EOF'
telegram:
  token: "123456:Aa..."    # the BotFather token
  allowedUser: 42          # your numeric user id
providerEnv:
  ANTHROPIC_API_KEY: "sk-ant-..."  # each key becomes a Secret key and an env var
EOF
helm install trygalle ./deploy/trygalle -f my-values.yaml
```

The chart creates a Secret from these values, a 5Gi PVC mounted at `/data/pi/sessions` and `/data/pi/work`, and a one-replica StatefulSet. Keep real credentials out of git: write them to an ignored file or pass them with `--set`. Customization options for both run modes are in [docs/customize.md](docs/customize.md).

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
