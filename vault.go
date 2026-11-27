package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// (Legacy backend; see infisical.go.) The vault is a git checkout that holds a copy of state.json, so the
// subscription login survives the loss of any single VM. VMs reach a private
// vault repo through an exe.dev GitHub integration, with no credentials on the
// VM itself.

type vault struct {
	dir       string
	statePath string
}

// openVault picks the backend: Infisical when INFISICAL_URL is set, otherwise
// the legacy git checkout, otherwise none (nil).
func openVault(vaultDir, stateDir string) (tokenVault, error) {
	if iv := newInfisicalVault(stateDir); iv != nil {
		return iv, nil
	}
	if vaultDir == "" {
		vaultDir = filepath.Join(stateDir, "vault")
	}
	if _, err := os.Stat(filepath.Join(vaultDir, ".git")); err != nil {
		return nil, nil // no vault configured on this VM
	}
	return &vault{dir: vaultDir, statePath: filepath.Join(stateDir, "state.json")}, nil
}

func (v *vault) vaultStatePath() string { return filepath.Join(v.dir, "state.json") }

// Restore seeds the state directory from the vault when it has no login yet.
func (v *vault) Restore() (bool, error) {
	if b, err := os.ReadFile(v.statePath); err == nil && len(bytes.TrimSpace(b)) > 2 {
		return false, nil
	}
	b, err := os.ReadFile(v.vaultStatePath())
	if err != nil || len(bytes.TrimSpace(b)) < 2 {
		return false, nil
	}
	tmp := v.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, v.statePath); err != nil {
		return false, err
	}
	return true, nil
}

// Sync copies state.json into the vault and pushes it when it changed.
func (v *vault) Sync(dryRun bool) (string, error) {
	b, err := os.ReadFile(v.statePath)
	if err != nil {
		return "", nil // nothing to publish yet
	}
	if cur, err := os.ReadFile(v.vaultStatePath()); err == nil && bytes.Equal(cur, b) {
		return "unchanged", nil
	}
	if dryRun {
		return "would push", nil
	}
	if err := os.WriteFile(v.vaultStatePath(), b, 0o600); err != nil {
		return "", err
	}

	ident := []string{"-c", "user.name=claude-sub", "-c", "user.email=claude-sub@localhost"}
	if out, err := v.git(append(ident, "add", "state.json")...); err != nil {
		return "", fmt.Errorf("git add: %v: %s", err, out)
	}
	if _, err := v.git(append(ident, "diff", "--cached", "--quiet")...); err == nil {
		return "unchanged", nil // staged nothing new
	}
	msg := "claude-sub: tokens refreshed " + time.Now().UTC().Format(time.RFC3339)
	if out, err := v.git(append(ident, "commit", "-q", "-m", msg)...); err != nil {
		return "", fmt.Errorf("git commit: %v: %s", err, out)
	}
	// Another VM may have pushed a fresher copy; rebase before publishing.
	_, _ = v.git(append(ident, "pull", "-q", "--rebase", "--autostash")...)
	if out, err := v.git(append(ident, "push", "-q")...); err != nil {
		return "", fmt.Errorf("git push: %v: %s", err, out)
	}
	return "pushed", nil
}

func (v *vault) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = v.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func runVaultSync(args []string) error {
	fs := flag.NewFlagSet("vault-sync", flag.ExitOnError)
	vaultDir := fs.String("vault", "", "vault git checkout (default <state>/vault)")
	state := fs.String("state", "", "state directory")
	restore := fs.Bool("restore", false, "copy the vault's tokens into an empty state dir and exit")
	dryRun := fs.Bool("dry-run", false, "report what would happen")
	_ = fs.Parse(args)

	dir := stateDir(nil, *state)
	v, err := openVault(*vaultDir, dir)
	if err != nil {
		return err
	}
	if v == nil {
		fmt.Println("no vault configured")
		return nil
	}

	if *restore {
		done, err := v.Restore()
		if err != nil {
			return err
		}
		if done {
			fmt.Println("restored tokens from the vault")
		} else {
			fmt.Println("nothing to restore")
		}
		return nil
	}

	status, err := v.Sync(*dryRun)
	if err != nil {
		return err
	}
	fmt.Println(status)
	return nil
}
