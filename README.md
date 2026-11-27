# claude-sub

Use a **Claude subscription** (Claude Pro / Max) as a model provider for
[Shelley](https://exe.dev/docs/shelley/intro) and other Anthropic-compatible
agents, instead of paying per token through an API key or the exe.dev LLM
gateway.

`claude-sub` is a small Go service that

1. signs in to your Claude subscription with the Claude Code OAuth client
   (PKCE, browser + one code paste), and
2. exposes an **Anthropic Messages API** at `http://127.0.0.1:8788/v1/messages`
   that injects the OAuth bearer token, the `anthropic-beta: oauth-2025-04-20`
   header, and the Claude Code system-prompt preamble those credentials
   require.

Shelley reaches it as a *custom model*, so `Claude Opus (subscription)` (and
any siblings you register) appears in the model picker next to the managed
models.

Two quirks of Shelley's custom-model support, handled here:

- Shelley stores a custom model's `endpoint` verbatim and POSTs straight to it,
  so register the full URL: `http://127.0.0.1:8788/v1/messages`.
- The proxy also accepts `POST /` for registrations that point at the root.

> **Caveat.** Subscription OAuth credentials are meant for Claude Code. Using
them from another client is not sanctioned by Anthropic's terms and can be
revoked, and you inherit Claude Code's subscription rate limits rather than API
capacity. Use it for your own VM, at your own risk.

## Install on one VM

```sh
git clone https://github.com/sthalatech/claude-sub
cd claude-sub
sudo sh install.sh
```

It installs `/usr/local/bin/claude-sub`, enables `claude-sub.service`, and
registers the model with the local Shelley (`claude-sub-register.service`).

Then open the login page it prints (`https://<vm>.exe.xyz:8788/` on exe.dev),
click **Log in with Claude**, approve, and paste the code the next page shows.

If you already ran `claude` / `claude setup-token` on the VM, you do not even
need the browser: the proxy imports `~/.claude/.credentials.json` on its own.

## Every new VM

exe.dev runs `/exe.dev/setup` once at first boot, and that path can have an
account-wide default. Register this repo as that default:

```sh
# from your laptop, once per exe.dev account
curl -fsSL https://raw.githubusercontent.com/sthalatech/claude-sub/main/setup.sh \
  | ssh exe.dev defaults write dev.exe new.setup-script
```

Now every `ssh exe.dev new ...` VM installs claude-sub and registers the model
before you ever log in. Each VM still needs its own sign-in — unless you use
the **export / import** flow below, or the account-wide integration variant.

### Signing in once for many VMs

On the VM that is already signed in, click **Export tokens** (or
`GET /export`), then paste the JSON into **Use this on another VM (import)** on
the new VM. The refresh token does the rest.

To retrofit VMs that already exist:

```sh
for vm in $(ssh exe.dev list --json | jq -r '.[].name'); do
  ssh "$vm.exe.xyz" 'sudo sh -c "curl -fsSL https://codeload.github.com/sthalatech/claude-sub/tar.gz/main | tar -xz -O --wildcards \*/install.sh > /tmp/i.sh && sh /tmp/i.sh"'
done
```

### The token vault (sign in once, ever)

The login lives in `/var/lib/claude-sub/state.json` on whatever VM signed in, so
losing that VM would mean signing in again. Point claude-sub at a private git
repository instead and the login survives:

```sh
sudo sh install.sh --vault https://github.int.exe.xyz/<you>/claude-sub-vault.git
```

`--vault` clones the repo into `/var/lib/claude-sub/vault`; if the VM has no
login yet the tokens are restored from it. A systemd timer
(`claude-sub-vault.timer`, every 10 minutes) pushes the tokens back after any
refresh, and a fresh login pushes immediately.

Give VMs access to the private repo with an exe.dev GitHub integration, so no
credential ever sits on a VM:

```sh
ssh exe.dev integrations add github --name claude-vault \
  --repository <you>/claude-sub-vault --attach auto:all
```

Then make the vault-backed installer the account-wide first-boot script:

```sh
curl -fsSL https://raw.githubusercontent.com/sthalatech/claude-sub/main/setup-vault.sh \
  | ssh exe.dev defaults write dev.exe new.setup-script
```

A new VM now boots with claude-sub installed, the model registered, and the
existing login restored: no browser, no per-VM sign-in, and the credential
outlives any single VM.

Treat the vault repo like a password: read access to it is the ability to use
your Claude subscription. Sign out and back in (`claude-sub logout`, then the
login page) to rotate it, which rewrites `state.json`.

### Truly zero-touch (no per-VM sign-in)

Hold the subscription credential at the exe.dev edge instead of on the VM, as a
custom HTTPS provider on an LLM integration attached to `auto:all`:

```sh
ssh exe.dev integrations add llm --name claude-sub \
  --custom-provider=anthropic=https://api.anthropic.com \
  --custom-provider-api=anthropic_messages \
  --header='anthropic-beta: oauth-2025-04-20' \
  --bearer=- --attach auto:all
```

Verify that `anthropic_messages` is accepted (the docs only demonstrate
`openai_responses`) and that the proxy forwards the extra header. When it
works, point Shelley's custom model endpoint at
`https://claude-sub.int.exe.xyz` instead of the local proxy, and the local
service is only needed for the system-prompt shim.

Newer Infisical servers renamed the project parameter from `workspaceId` to
`projectId`; set `INFISICAL_PROJECT_PARAM=projectId` (default `workspaceId`) for
those.

## CLI

```
claude-sub serve     run proxy + login page (systemd does this for you)
claude-sub register  add/replace the custom model in Shelley (defaults: newest Opus, "Claude Opus (subscription)")
claude-sub models    list model ids the subscription can call
claude-sub status    show login state
claude-sub vault-sync publish tokens to the vault (or --restore from it)
claude-sub logout    forget stored tokens
```

Useful flags: `serve -addr :8788 -state DIR -upstream URL`,
`register -shelley http://127.0.0.1:9999 -model-name claude-opus-4-5`.

## Files

- `/usr/local/bin/claude-sub` — the binary
- `/var/lib/claude-sub/state.json` — OAuth tokens (0600, user-owned)
- `/var/lib/claude-sub/proxy.token` — bearer token Shelley uses against the proxy
- `/etc/systemd/system/claude-sub.service`, `claude-sub-register.service`

## How the OAuth flow works

The constants come from the installed Claude Code CLI, so this stays in step
with it:

- authorize: `https://claude.com/cai/oauth/authorize` with
  `client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e`, PKCE S256, and the manual
  redirect `https://platform.claude.com/oauth/code/callback` (hence the code
  paste)
- token/refresh: `https://platform.claude.com/v1/oauth/token`
- inference: `https://api.anthropic.com/v1/messages` with
  `Authorization: Bearer <token>` and `anthropic-beta: oauth-2025-04-20`

## Tests

```sh
go test ./...
```

## Infisical backend (replaces the git vault)

Set `INFISICAL_URL` (+ `INFISICAL_PROJECT_ID`) and claude-sub stores `state.json`
as the secret `state_json` at `INFISICAL_SECRET_PATH` (default `/claude-sub`,
environment `INFISICAL_ENVIRONMENT`, default `prod`). `vault-sync` and the
10-minute timer push refreshed tokens; a new VM restores the login on first start.
Auth is optional: with an exe.dev peer integration injecting a bearer token no
credential lives on the VM; otherwise set `INFISICAL_TOKEN` or
`INFISICAL_CLIENT_ID`/`INFISICAL_CLIENT_SECRET`.

`setup-infisical.sh` is the new-VM entry point. It writes
`/etc/claude-sub/infisical.env`, installs claude-sub (`install.sh --infisical`),
and points git at the GitHub PAT in Infisical (`/vm-defaults/GH_TOKEN`) via
`vm-git-credential` (fetched on demand, never stored on disk; `gh` is logged in too).

    curl -fsSL https://raw.githubusercontent.com/sthalatech/claude-sub/main/bootstrap.sh \
      | ssh exe.dev defaults write dev.exe new.setup-script

To seed from an already signed-in VM: put the env file in place, run `claude-sub vault-sync`.
