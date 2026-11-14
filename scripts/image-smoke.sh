#!/bin/sh
# Image smoke checks for the trygalle container image.
#
# Usage: scripts/image-smoke.sh [image]   (default image: trygalle:smoke)
#
# The script builds nothing; run `docker build -t trygalle:smoke .` first.
# Local runs and the CI image/release jobs share these checks.
set -eu

IMAGE=${1:-trygalle:smoke}
REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

fail() {
    printf 'smoke: FAIL: %s\n' "$1" >&2
    exit 1
}

cleanup() {
    [ -z "${FAKEBIN:-}" ] || rm -rf "$FAKEBIN"
}
trap cleanup EXIT

# Pin cross-check: the Go constant, the Dockerfile npm literal, and the
# in-image `pi --version` must all report the same version.
PIN_GO=$(grep -o 'PinnedPiVersion = "[^"]*"' "$REPO_ROOT/internal/harness/harness.go" | grep -o '[0-9][0-9.]*')
PIN_DOCKERFILE=$(grep -o '@earendil-works/pi-coding-agent@[0-9][0-9.]*' "$REPO_ROOT/Dockerfile" | grep -o '[0-9][0-9.]*')
KUBECTL_VERSION=$(grep -o 'ARG KUBECTL_VERSION=v[0-9][0-9.]*' "$REPO_ROOT/Dockerfile" | sed 's/^ARG KUBECTL_VERSION=//')

[ -n "$PIN_GO" ] || fail 'could not extract PinnedPiVersion from internal/harness/harness.go'
[ -n "$PIN_DOCKERFILE" ] || fail 'could not extract the pi pin from Dockerfile'
[ -n "$KUBECTL_VERSION" ] || fail 'could not extract ARG KUBECTL_VERSION from Dockerfile'
[ "$PIN_GO" = "$PIN_DOCKERFILE" ] || fail "harness.PinnedPiVersion ($PIN_GO) and the Dockerfile pin ($PIN_DOCKERFILE) differ"

# Missing-token diagnostic: with no inputs the image must exit non-zero
# quickly with the missing-token message, not start the poll loop.
set +e
OUT=$(docker run --rm "$IMAGE" 2>&1)
RC=$?
set -e
[ "$RC" -ne 0 ] || fail 'image with no inputs exited 0; expected a non-zero missing-token exit'
case $OUT in
*'missing bot token (set TRYGALLE_TELEGRAM_TOKEN)'*) ;;
*) fail "missing-token diagnostic not found in output: $OUT" ;;
esac

# Entrypoint arguments: a fake `trygalle` on PATH echoes the arguments the
# entrypoint supplies, for the default and an overridden session directory,
# plus any extra args passed through as CMD.
FAKEBIN=$(mktemp -d)
# The container reads the fake binary as UID 1000, so the mounted directory
# must be traversable by all users; mktemp creates it 0700 under the CI user.
chmod 0755 "$FAKEBIN"
cat >"$FAKEBIN/trygalle" <<'EOF'
#!/bin/sh
printf '%s\n' "$*"
EOF
chmod 0755 "$FAKEBIN/trygalle"

OUT=$(docker run --rm -e PATH=/fakebin -v "$FAKEBIN:/fakebin:ro" "$IMAGE" 2>&1) \
    || fail "entrypoint default args run failed ($?): $OUT"
case $OUT in
*'--pi-bin pi --session-dir /data/pi/sessions'*) ;;
*) fail "entrypoint default args wrong: $OUT" ;;
esac

OUT=$(docker run --rm -e PATH=/fakebin -e TRYGALLE_SESSION_DIR=/tmp/other -v "$FAKEBIN:/fakebin:ro" "$IMAGE" 2>&1) \
    || fail "entrypoint override args run failed ($?): $OUT"
case $OUT in
*'--pi-bin pi --session-dir /tmp/other'*) ;;
*) fail "entrypoint override args wrong: $OUT" ;;
esac

OUT=$(docker run --rm -e PATH=/fakebin -v "$FAKEBIN:/fakebin:ro" "$IMAGE" --extra-flag 2>&1) \
    || fail "entrypoint extra-args run failed ($?): $OUT"
case $OUT in
'--pi-bin pi --session-dir /data/pi/sessions --extra-flag') ;;
*) fail "entrypoint did not pass extra args through in order: $OUT" ;;
esac

# Pinned toolchain: pi reports the pinned version, trygalle runs, kubectl
# matches the ARG literal, and every required tool resolves.
OUT=$(docker run --rm --entrypoint /usr/local/bin/pi "$IMAGE" --version 2>&1) \
    || fail "in-image pi --version run failed ($?): $OUT"
[ "$OUT" = "$PIN_GO" ] || fail "in-image pi --version ($OUT) does not match the pinned version ($PIN_GO)"

docker run --rm --entrypoint /usr/local/bin/trygalle "$IMAGE" --help >/dev/null 2>&1 \
    || fail 'trygalle --help did not exit 0'

OUT=$(docker run --rm --entrypoint /usr/local/bin/kubectl "$IMAGE" version --client 2>&1) \
    || fail "kubectl version run failed ($?): $OUT"
case $OUT in
*"$KUBECTL_VERSION"*) ;;
*) fail "kubectl version --client does not report $KUBECTL_VERSION: $OUT" ;;
esac

docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'command -v bash git curl jq rg trygalle pi kubectl' >/dev/null 2>&1 \
    || fail 'a required tool is not on PATH'
docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'test -r /etc/ssl/certs/ca-certificates.crt' >/dev/null 2>&1 \
    || fail 'CA certificates bundle is missing'

# Non-root runtime: UID 1000 and writable /data/pi paths.
OUT=$(docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'id -u' 2>&1) \
    || fail "id -u run failed ($?): $OUT"
[ "$OUT" = '1000' ] || fail "container runs as UID $OUT, expected 1000"

docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'touch /data/pi/sessions/.smoke-probe /data/pi/work/.smoke-probe' >/dev/null 2>&1 \
    || fail '/data/pi paths are not writable'

printf 'smoke: PASS: %s (pi %s, kubectl %s)\n' "$IMAGE" "$PIN_GO" "$KUBECTL_VERSION"
