#!/bin/sh
# exe.dev first-boot entry point for VM fleets: install claude-sub *and* wire it
# to a shared token vault, so a new VM comes up already signed in.
#
#   curl -fsSL https://raw.githubusercontent.com/sthalatech/claude-sub/main/setup-vault.sh \
#     | ssh exe.dev defaults write dev.exe new.setup-script
#
# Requires the vault repo to be reachable from the VM, e.g. an exe.dev GitHub
# integration attached to auto:all:
#   ssh exe.dev integrations add github --name claude-vault \
#     --repository sthalatech/claude-sub-vault --attach auto:all
set -eu

# Point this at your own vault repository if you forked claude-sub.
VAULT=${CLAUDE_SUB_VAULT:-https://github.int.exe.xyz/sthalatech/claude-sub-vault.git}
REPO=${CLAUDE_SUB_REPO:-sthalatech/claude-sub}
TAG=${CLAUDE_SUB_TAG:-main}
SRC=${CLAUDE_SUB_SRC:-https://codeload.github.com/$REPO/tar.gz/$TAG}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

echo "claude-sub: fetching $REPO@$TAG (vault: $VAULT)"
curl -fsSL "$SRC" | tar -xz -C "$WORK"
DIR=$(find "$WORK" -maxdepth 1 -mindepth 1 -type d | head -n1)

SUDO=""
[ "$(id -u)" -eq 0 ] || SUDO="sudo"
$SUDO sh "$DIR/install.sh" --vault "$VAULT"
