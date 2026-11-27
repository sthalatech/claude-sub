#!/bin/sh
# exe.dev first-boot entry point: install claude-sub backed by Infisical, and
# give the VM git access through the GitHub PAT stored in Infisical.
#
#   curl -fsSL https://raw.githubusercontent.com/sthalatech/claude-sub/main/bootstrap.sh \
#     | ssh exe.dev defaults write dev.exe new.setup-script
# (bootstrap.sh just fetches this script, so later fixes apply without re-registering.
#  Output is logged to /var/log/vm-setup.log. Note: a VAR=x prefix on the ssh command
#  does not reach the script; the project id default below is baked in.)
#
# Prerequisite (once): an exe.dev integration that fronts Infisical privately,
# attached to your VMs (auto:all or a tag), e.g.
#   printf '%s' "$INFISICAL_TOKEN" | ssh exe.dev integrations add http-proxy \
#     --name infisical --target https://<infisical-vm>.exe.xyz:8090/ \
#     --peer --bearer=- --attach auto:all
# Secrets (Infisical, env $INFISICAL_ENVIRONMENT):
#   /claude-sub/state_json   Claude login (written/refreshed by claude-sub)
#   /claude-sub/state_json_personal  personal Claude login (sign in once on any VM at :8789)
#   /vm-defaults/GH_TOKEN    GitHub PAT
#   /vm-defaults/OLLAMA_API_KEY  Ollama Cloud key (models: ollama-models.json)
set -eu

INFISICAL_URL=${INFISICAL_URL:-https://infisical.int.exe.xyz}
INFISICAL_PROJECT_ID=${INFISICAL_PROJECT_ID:-27479f9b-14ec-4fe5-bbe7-98416cf4554a}
INFISICAL_ENVIRONMENT=${INFISICAL_ENVIRONMENT:-prod}
REPO=${CLAUDE_SUB_REPO:-sthalatech/claude-sub}
TAG=${CLAUDE_SUB_TAG:-main}
SRC=${CLAUDE_SUB_SRC:-https://codeload.github.com/$REPO/tar.gz/$TAG}

SUDO=""
[ "$(id -u)" -eq 0 ] || SUDO="sudo"
LOGIN_USER=${SUDO_USER:-$(id -un)}
[ "$LOGIN_USER" = root ] && LOGIN_USER=$(awk -F: '$3>=1000 && $3<65534 {print $1; exit}' /etc/passwd)
LOGIN_GROUP=$(id -gn "$LOGIN_USER")

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
echo "setup: fetching $REPO@$TAG"
curl -fsSL "$SRC" | tar -xz -C "$WORK"
DIR=$(find "$WORK" -maxdepth 1 -mindepth 1 -type d | head -n1)

echo "setup: writing /etc/claude-sub/infisical.env"
$SUDO install -d -m 0755 /etc/claude-sub
{
	echo "INFISICAL_URL=$INFISICAL_URL"
	echo "INFISICAL_PROJECT_ID=$INFISICAL_PROJECT_ID"
	echo "INFISICAL_ENVIRONMENT=$INFISICAL_ENVIRONMENT"
	# Optional credentials; omit when an integration injects auth at the edge.
	[ -n "${INFISICAL_TOKEN:-}" ] && echo "INFISICAL_TOKEN=$INFISICAL_TOKEN"
	[ -n "${INFISICAL_CLIENT_ID:-}" ] && echo "INFISICAL_CLIENT_ID=$INFISICAL_CLIENT_ID"
	[ -n "${INFISICAL_CLIENT_SECRET:-}" ] && echo "INFISICAL_CLIENT_SECRET=$INFISICAL_CLIENT_SECRET"
	true
} | $SUDO tee /etc/claude-sub/infisical.env >/dev/null
$SUDO chown root:"$LOGIN_GROUP" /etc/claude-sub/infisical.env
$SUDO chmod 0640 /etc/claude-sub/infisical.env

$SUDO install -m 0755 "$DIR/vm-secret" /usr/local/bin/vm-secret
$SUDO install -m 0755 "$DIR/vm-git-credential" /usr/local/bin/vm-git-credential

echo "setup: claude-sub"
$SUDO sh "$DIR/install.sh" --infisical

echo "setup: personal Claude account (second instance, port 8789)"
# Separate login, port, state and Infisical secret (state_json_personal). Sign in
# once at the printed URL; every other VM then restores it automatically.
$SUDO env CLAUDE_SUB_INSTANCE=personal CLAUDE_SUB_PORT=8789 CLAUDE_SUB_DISPLAY="Claude Opus (personal)" \
	sh "$DIR/install.sh" --infisical || echo "    personal instance failed (continuing)" >&2

echo "setup: Ollama models"
if OLLAMA_API_KEY=$(/usr/local/bin/vm-secret OLLAMA_API_KEY) && [ -n "$OLLAMA_API_KEY" ]; then
	export OLLAMA_API_KEY
	for i in $(seq 1 60); do
		curl -fsS -o /dev/null -H "X-Exedev-Userid: claude-sub" "${CLAUDE_SUB_SHELLEY:-http://127.0.0.1:9999}/api/models" && break
		sleep 2
	done
	/usr/local/bin/claude-sub register-openai -shelley "${CLAUDE_SUB_SHELLEY:-http://127.0.0.1:9999}" \
		-endpoint https://ollama.com/v1 -models "$DIR/ollama-models.json" || echo "    ollama registration failed (continuing)" >&2
else
	echo "    could not read /vm-defaults/OLLAMA_API_KEY" >&2
fi

echo "setup: GitHub PAT"
if GH_TOKEN_VALUE=$(/usr/local/bin/vm-secret GH_TOKEN) && [ -n "$GH_TOKEN_VALUE" ]; then
	# Fetched on demand by git; never stored on disk.
	$SUDO git config --system credential.https://github.com.helper '!/usr/local/bin/vm-git-credential'
	if command -v gh >/dev/null 2>&1; then
		printf '%s' "$GH_TOKEN_VALUE" | runuser -u "$LOGIN_USER" -- gh auth login --with-token >/dev/null 2>&1 || true
	fi
	echo "    git (and gh) now use the PAT from Infisical"
else
	echo "    could not read /vm-defaults/GH_TOKEN (is the Infisical integration attached?)" >&2
fi
echo "setup: done"
