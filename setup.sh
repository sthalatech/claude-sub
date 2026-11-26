#!/bin/sh
# exe.dev first-boot entry point: install claude-sub on a brand-new VM.
#
# Register it as the account-wide default setup script:
#   cat setup.sh | ssh exe.dev defaults write dev.exe new.setup-script
set -eu

REPO=${CLAUDE_SUB_REPO:-sthalatech/claude-sub}
TAG=${CLAUDE_SUB_TAG:-main}
SRC=${CLAUDE_SUB_SRC:-https://codeload.github.com/$REPO/tar.gz/$TAG}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

echo "claude-sub: fetching $REPO@$TAG"
curl -fsSL "$SRC" | tar -xz -C "$WORK"
DIR=$(find "$WORK" -maxdepth 1 -mindepth 1 -type d | head -n1)

SUDO=""
[ "$(id -u)" -eq 0 ] || SUDO="sudo"
$SUDO sh "$DIR/install.sh"
