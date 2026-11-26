package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestVaultRestoreAndSync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitCmd(t, root, "init", "--bare", "-q", "-b", "main", remote)

	seed := filepath.Join(root, "seed")
	gitCmd(t, root, "clone", "-q", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "state.json"), []byte(`{"refresh_token":"vault-refresh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "state.json")
	gitCmd(t, seed, "commit", "-q", "-m", "seed")
	gitCmd(t, seed, "push", "-q", "origin", "HEAD:main")

	// A new VM: clone the vault, restore, then publish a refreshed token.
	stateDirPath := t.TempDir()
	vaultDir := filepath.Join(root, "vault")
	gitCmd(t, root, "clone", "-q", remote, vaultDir)

	v, err := openVault(vaultDir, stateDirPath)
	if err != nil || v == nil {
		t.Fatalf("openVault: %v %v", v, err)
	}
	if done, err := v.Restore(); err != nil || !done {
		t.Fatalf("restore: done=%v err=%v", done, err)
	}
	got, _ := os.ReadFile(filepath.Join(stateDirPath, "state.json"))
	if string(got) != `{"refresh_token":"vault-refresh"}` {
		t.Fatalf("restored %q", got)
	}

	// A second restore must not clobber an existing login.
	if done, _ := v.Restore(); done {
		t.Fatal("restore overwrote existing state")
	}

	if err := os.WriteFile(filepath.Join(stateDirPath, "state.json"), []byte(`{"refresh_token":"newer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, err := v.Sync(false); err != nil || status != "pushed" {
		t.Fatalf("sync: %q %v", status, err)
	}
	if status, err := v.Sync(false); err != nil || status != "unchanged" {
		t.Fatalf("second sync: %q %v", status, err)
	}

	check := filepath.Join(root, "check")
	gitCmd(t, root, "clone", "-q", remote, check)
	b, err := os.ReadFile(filepath.Join(check, "state.json"))
	if err != nil || string(b) != `{"refresh_token":"newer"}` {
		t.Fatalf("remote has %q (%v)", b, err)
	}
}
