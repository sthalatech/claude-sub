#!/bin/sh
# Tiny stable first-boot stub: register this once, fixes land via setup-infisical.sh.
LOG=/var/log/vm-setup.log
{
	echo "=== vm setup $(date -u +%FT%TZ) ==="
	curl -fsSL "https://raw.githubusercontent.com/sthalatech/claude-sub/main/setup-infisical.sh?$(date +%s)" | sh -x
	echo "=== exit $? ==="
} 2>&1 | { if [ -w /var/log ]; then tee -a "$LOG"; else sudo tee -a "$LOG"; fi; }
