#!/bin/sh
# Install claude-sub on an exe.dev VM: proxy service, systemd units, and the
# Shelley custom-model registration.
#
#   sudo sh install.sh            # from a checkout (builds with go)
#   sudo sh install.sh --no-register
set -eu

PORT=${CLAUDE_SUB_PORT:-8788}
SHELLEY_URL=${CLAUDE_SUB_SHELLEY:-http://127.0.0.1:9999}
REGISTER=1

for arg in "$@"; do
	case "$arg" in
	--no-register) REGISTER=0 ;;
	--port=*) PORT=${arg#*=} ;;
	*) echo "unknown argument: $arg" >&2; exit 2 ;;
	esac
done

[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo sh install.sh)" >&2; exit 1; }

LOGIN_USER=${SUDO_USER:-${CLAUDE_SUB_USER:-}}
if [ -z "$LOGIN_USER" ] || [ "$LOGIN_USER" = root ]; then
	LOGIN_USER=$(awk -F: '$3>=1000 && $3<65534 {print $1; exit}' /etc/passwd)
fi
[ -n "$LOGIN_USER" ] || { echo "could not determine the login user" >&2; exit 1; }
LOGIN_GROUP=$(id -gn "$LOGIN_USER")

SRC=$(cd "$(dirname "$0")" && pwd)

# The login page is reached through the exe.dev proxy on the VM's hostname.
PUBLIC_URL=${CLAUDE_SUB_PUBLIC_URL:-}
if [ -z "$PUBLIC_URL" ] && [ -r /exe.dev/shelley.json ]; then
	host=$(sed -n 's/.*"url":"https:\/\/\([a-z0-9.-]*\.exe\.xyz\)".*/\1/p' /exe.dev/shelley.json | head -n1)
	[ -n "$host" ] && PUBLIC_URL="https://$host:$PORT/"
fi
[ -n "$PUBLIC_URL" ] || PUBLIC_URL="http://127.0.0.1:$PORT/"

echo "==> installing binary"
if [ -x "$SRC/claude-sub" ] && [ "${CLAUDE_SUB_REBUILD:-0}" != 1 ]; then
	install -m 0755 "$SRC/claude-sub" /usr/local/bin/claude-sub
else
	command -v go >/dev/null 2>&1 || { echo "go is required to build claude-sub" >&2; exit 1; }
	(cd "$SRC" && go build -trimpath -ldflags=-s -o /tmp/claude-sub.build .)
	install -m 0755 /tmp/claude-sub.build /usr/local/bin/claude-sub
	rm -f /tmp/claude-sub.build
fi

echo "==> installing units for user $LOGIN_USER on port $PORT"
render() {
	sed -e "s|@LOGIN_USER@|$LOGIN_USER|g" \
		-e "s|@LOGIN_GROUP@|$LOGIN_GROUP|g" \
		-e "s|@PORT@|$PORT|g" \
		-e "s|@PUBLIC_URL@|$PUBLIC_URL|g" \
		-e "s|@SHELLEY_URL@|$SHELLEY_URL|g" \
		"$1" >"$2"
	chmod 0644 "$2"
}
render "$SRC/deploy/claude-sub.service" /etc/systemd/system/claude-sub.service
render "$SRC/deploy/claude-sub-register.service" /etc/systemd/system/claude-sub-register.service

systemctl daemon-reload
systemctl enable --now claude-sub.service >/dev/null

echo "==> service started"
if [ "$REGISTER" = 1 ]; then
	if systemctl enable --now claude-sub-register.service >/dev/null 2>&1; then
		echo "==> model registered with Shelley"
	else
		echo "==> could not register the model with Shelley yet (Shelley not reachable?)"
		echo "    retry with: sudo /usr/local/bin/claude-sub register -shelley $SHELLEY_URL"
	fi
fi

cat <<EOF

claude-sub is installed.

  Sign in (once per VM):   ${PUBLIC_URL}
  After signing in, Shelley's model picker shows: Claude (subscription)

  Status:                  claude-sub status
  Proxy endpoint:          http://127.0.0.1:${PORT}/v1/messages
EOF
