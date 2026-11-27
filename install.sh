#!/bin/sh
# Install claude-sub on an exe.dev VM: proxy service, systemd units, and the
# Shelley custom-model registration.
#
#   sudo sh install.sh            # from a checkout (builds with go)
#   sudo sh install.sh --no-register
set -eu

PORT=${CLAUDE_SUB_PORT:-8788}
SHELLEY_URL=${CLAUDE_SUB_SHELLEY:-http://127.0.0.1:9999}
VAULT=${CLAUDE_SUB_VAULT:-}
REGISTER=1
INFISICAL=0
# A second login (e.g. a personal account) runs as its own instance:
#   CLAUDE_SUB_INSTANCE=personal CLAUDE_SUB_PORT=8789 CLAUDE_SUB_DISPLAY="Claude Opus (personal)" sh install.sh --infisical
INSTANCE=${CLAUDE_SUB_INSTANCE:-}
SFX=""; SECRET_NAME=state_json
if [ -n "$INSTANCE" ]; then SFX="-$INSTANCE"; SECRET_NAME="state_json_$INSTANCE"; fi
DISPLAY_NAME=${CLAUDE_SUB_DISPLAY:-Claude Opus (subscription)}

for arg in "$@"; do
	case "$arg" in
	--no-register) REGISTER=0 ;;
	--port=*) PORT=${arg#*=} ;;
	--vault=*) VAULT=${arg#*=} ;;
	--no-vault) VAULT= ;;
	--infisical) INFISICAL=1 ;;
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
	if ! command -v go >/dev/null 2>&1; then
		echo "    go not found; fetching a temporary toolchain"
		case "$(uname -m)" in x86_64) GOARCH=amd64 ;; aarch64|arm64) GOARCH=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
		GOTMP=$(mktemp -d)
		curl -fsSL "https://go.dev/dl/go1.24.6.linux-$GOARCH.tar.gz" | tar -xz -C "$GOTMP"
		export PATH="$GOTMP/go/bin:$PATH" GOCACHE="$GOTMP/cache" GOTOOLCHAIN=local
	fi
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
		-e "s|@SFX@|$SFX|g" \
		-e "s|@DISPLAY@|$DISPLAY_NAME|g" \
		-e "s|@SECRET_NAME@|$SECRET_NAME|g" \
		"$1" >"$2"
	chmod 0644 "$2"
}
render "$SRC/deploy/claude-sub.service" /etc/systemd/system/claude-sub$SFX.service
render "$SRC/deploy/claude-sub-register.service" /etc/systemd/system/claude-sub-register$SFX.service

render "$SRC/deploy/claude-sub-vault.service" /etc/systemd/system/claude-sub-vault$SFX.service
render "$SRC/deploy/claude-sub-vault.timer" /etc/systemd/system/claude-sub-vault$SFX.timer

STATE_DIR=/var/lib/claude-sub$SFX
if [ -n "$VAULT" ]; then
	echo "==> token vault: $VAULT"
	if [ ! -d "$STATE_DIR/vault/.git" ]; then
		install -d -o "$LOGIN_USER" -g "$LOGIN_GROUP" -m 0700 "$STATE_DIR"
		rm -rf "$STATE_DIR/vault"
		if ! runuser -u "$LOGIN_USER" -- git clone -q "$VAULT" "$STATE_DIR/vault"; then
			echo "    could not clone the vault yet (is the GitHub integration attached?)" >&2
			rm -rf "$STATE_DIR/vault"
		fi
	fi
fi

if [ "$INFISICAL" = 1 ] && [ ! -r /etc/claude-sub/infisical.env ]; then
	echo "--infisical needs /etc/claude-sub/infisical.env (see setup-infisical.sh)" >&2
	exit 1
fi

systemctl daemon-reload
systemctl enable claude-sub$SFX.service >/dev/null 2>&1
systemctl restart claude-sub$SFX.service >/dev/null

if [ "$INFISICAL" = 1 ]; then
	runuser -u "$LOGIN_USER" -- env CLAUDE_SUB_STATE="$STATE_DIR" INFISICAL_SECRET_NAME="$SECRET_NAME" sh -c '. /etc/claude-sub/infisical.env; export INFISICAL_SECRET_NAME INFISICAL_URL INFISICAL_PROJECT_ID INFISICAL_ENVIRONMENT INFISICAL_TOKEN INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET; /usr/local/bin/claude-sub vault-sync --restore' || true
	systemctl restart claude-sub$SFX.service >/dev/null
	systemctl enable --now claude-sub-vault$SFX.timer >/dev/null 2>&1 && echo "==> Infisical sync timer enabled"
elif [ -d "$STATE_DIR/vault/.git" ]; then
	runuser -u "$LOGIN_USER" -- /usr/local/bin/claude-sub vault-sync --restore >/dev/null 2>&1 || true
	systemctl enable --now claude-sub-vault$SFX.timer >/dev/null 2>&1 && echo "==> vault sync timer enabled"
else
	systemctl disable --now claude-sub-vault$SFX.timer >/dev/null 2>&1 || true
fi

echo "==> service started"
if [ "$REGISTER" = 1 ]; then
	if systemctl enable --now --no-block claude-sub-register$SFX.service >/dev/null 2>&1; then
		echo "==> model registered with Shelley"
	else
		echo "==> could not register the model with Shelley yet (Shelley not reachable?)"
		echo "    retry with: sudo /usr/local/bin/claude-sub register -shelley $SHELLEY_URL"
	fi
fi

cat <<EOF

claude-sub is installed.

  Sign in (once, then it syncs): ${PUBLIC_URL}
  After signing in, Shelley's model picker shows: ${DISPLAY_NAME}

  Status:                  CLAUDE_SUB_STATE=$STATE_DIR claude-sub status
  Proxy endpoint:          http://127.0.0.1:${PORT}/v1/messages
EOF
