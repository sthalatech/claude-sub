---
name: vm-fleet-setup
description: Use when the user wants to add a model provider, tool, package, secret, or any other setup step to all their exe.dev VMs (new and existing), or asks how the VM startup/boot script works (claude-sub, Infisical, setup-infisical.sh).
---

# VM fleet setup (sthalatech/claude-sub)

Every exe.dev VM is configured by one script, so a change made there reaches
all VMs. Edit the repo; never hand-configure a single VM and call it done.

## How it works

- Account-wide setup script (registered once) = `bootstrap.sh`: a stub that
  downloads `setup-infisical.sh` from `main` of https://github.com/sthalatech/claude-sub
  and runs it. So **pushing to `main` is the whole deploy** for new VMs; nothing
  to re-register.
- `setup-infisical.sh` installs, in order: `/etc/claude-sub/infisical.env`,
  `vm-secret` + `vm-git-credential`, claude-sub org instance (port 8788,
  secret `/claude-sub/state_json`), personal instance (8789,
  `state_json_personal`), Ollama models (`ollama-models.json` via
  `claude-sub register-openai`), GitHub PAT, Shelley skills (`skills/*`).
- Secrets live in self-hosted Infisical (project `27479f9b-14ec-4fe5-bbe7-98416cf4554a`,
  env `prod`), reached at `https://infisical.int.exe.xyz` through an exe.dev
  peer integration that injects auth. No credential on the VM.
  Read one: `vm-secret NAME [/path]` (default path `/vm-defaults`).
- The repo is checked out on coder-darkstore at `~/claude-sub`. Git pushes work
  on any VM via the PAT helper.

## Add an OpenAI-compatible provider (OpenRouter, Groq, DeepSeek, ...)

1. Ask the user to put the key in Infisical at `/vm-defaults/<NAME>_API_KEY`
   (prod), or write it via the Infisical API if you have write access. Never
   commit keys.
2. Add `<name>-models.json`:
   `[{"display_name": "Model X (Provider)", "model_name": "upstream-id", "max_tokens": 200000}]`
3. In `setup-infisical.sh`, before `echo "setup: GitHub PAT"`, copy the Ollama block:
   ```sh
   if KEY=$(/usr/local/bin/vm-secret OPENROUTER_API_KEY) && [ -n "$KEY" ]; then
       OPENROUTER_API_KEY=$KEY /usr/local/bin/claude-sub register-openai \
           -endpoint https://openrouter.ai/api/v1 -key-env OPENROUTER_API_KEY \
           -models "$DIR/openrouter-models.json" || echo "    openrouter failed" >&2
   fi
   ```
   `register-openai` skips models already registered (same endpoint, model, name).
4. Test on this VM (below), then push.

Another Claude subscription = another claude-sub instance:
`$SUDO env CLAUDE_SUB_INSTANCE=<x> CLAUDE_SUB_PORT=<port> CLAUDE_SUB_DISPLAY="Claude Opus (<x>)" sh "$DIR/install.sh" --infisical`,
then the user signs in once at `https://<vm>.exe.xyz:<port>/`; the login syncs to
`/claude-sub/state_json_<x>` and other VMs restore it.

## Add anything else

Add a section to `setup-infisical.sh` before `echo "setup: done"`.
Add a skill: create `skills/<name>/SKILL.md` in the repo; setup copies every
`skills/*` dir to `~/.config/agents/skills/`.

## Rules (non-negotiable)

- **Idempotent**: existing VMs re-run the whole script. Check before
  installing; tolerate "already exists".
- **Never `set -x`** or echo secrets: output goes to `/var/log/vm-setup.log`
  and to chat. This leaked a PAT once.
- **Don't break the rest**: end optional steps with `|| echo "... failed" >&2`.
- Secrets only via Infisical + `vm-secret`; never in the repo.

## Test, deploy, roll out

```sh
cd ~/claude-sub && sh -n setup-infisical.sh && go test ./...
git add <files> && git commit -m "..." && git push origin main
# test on this VM:
curl -fsSL https://raw.githubusercontent.com/sthalatech/claude-sub/main/bootstrap.sh | sh 2>&1 | tail -5
```
raw.githubusercontent.com caches for ~5 min; bootstrap adds a cache-buster for
setup-infisical.sh, but files fetched via the tarball can lag briefly.

Existing VMs: this VM cannot run `ssh exe.dev`. Give the user this to run from
their laptop:
```sh
for vm in $(ssh exe.dev ls | grep -oE '[a-z0-9-]+\.exe\.xyz' | sort -u); do echo "=== $vm"; ssh -n "$vm" 'curl -fsSL https://raw.githubusercontent.com/sthalatech/claude-sub/main/bootstrap.sh | sh 2>&1 | tail -3; sudo rm -f /var/log/vm-setup.log'; done
```
Success per VM: `setup: done` and `=== exit 0 ===`.
