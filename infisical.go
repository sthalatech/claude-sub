package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// tokenVault keeps a copy of state.json outside the VM so the subscription
// login survives the loss of any single VM.
type tokenVault interface {
	// Restore seeds the state directory when it has no login yet.
	Restore() (bool, error)
	// Sync publishes state.json when it changed ("pushed", "unchanged", ...).
	Sync(dryRun bool) (string, error)
}

// infisicalVault stores state.json as one secret in a self-hosted Infisical.
//
// Configuration comes from the environment (systemd loads
// /etc/claude-sub/infisical.env):
//
//	INFISICAL_URL           base URL, e.g. https://infisical.int.exe.xyz
//	INFISICAL_PROJECT_ID    project (workspace) id
//	INFISICAL_ENVIRONMENT   default "prod"
//	INFISICAL_SECRET_PATH   default "/claude-sub"
//	INFISICAL_SECRET_NAME   default "state_json"
//
// Authentication is optional so an exe.dev integration can inject it at the
// network edge (no credential on the VM). Otherwise set either
// INFISICAL_TOKEN (machine-identity access token) or
// INFISICAL_CLIENT_ID + INFISICAL_CLIENT_SECRET (Universal Auth).
type infisicalVault struct {
	base, project, env, path, name string
	token, clientID, clientSecret  string
	statePath                      string
	hc                             *http.Client
	tok                            string
	tokExp                         time.Time
}

func newInfisicalVault(stateDir string) *infisicalVault {
	base := strings.TrimRight(os.Getenv("INFISICAL_URL"), "/")
	if base == "" {
		return nil
	}
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return &infisicalVault{
		base:         base,
		project:      os.Getenv("INFISICAL_PROJECT_ID"),
		env:          get("INFISICAL_ENVIRONMENT", "prod"),
		path:         get("INFISICAL_SECRET_PATH", "/claude-sub"),
		name:         get("INFISICAL_SECRET_NAME", "state_json"),
		token:        os.Getenv("INFISICAL_TOKEN"),
		clientID:     os.Getenv("INFISICAL_CLIENT_ID"),
		clientSecret: os.Getenv("INFISICAL_CLIENT_SECRET"),
		statePath:    stateDir + "/state.json",
		hc:           &http.Client{Timeout: 20 * time.Second},
	}
}

func (v *infisicalVault) bearer() (string, error) {
	if v.token != "" {
		return v.token, nil
	}
	if v.clientID == "" || v.clientSecret == "" {
		return "", nil // auth injected at the network edge
	}
	if v.tok != "" && time.Now().Before(v.tokExp.Add(-time.Minute)) {
		return v.tok, nil
	}
	body, _ := json.Marshal(map[string]string{"clientId": v.clientID, "clientSecret": v.clientSecret})
	resp, err := v.hc.Post(v.base+"/api/v1/auth/universal-auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(b, &out) != nil || out.AccessToken == "" {
		return "", fmt.Errorf("infisical login: HTTP %d: %.200s", resp.StatusCode, b)
	}
	v.tok, v.tokExp = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return v.tok, nil
}

func (v *infisicalVault) do(method, p string, q url.Values, payload any) (int, []byte, error) {
	var rd io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		rd = bytes.NewReader(b)
	}
	u := v.base + p
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := v.bearer()
	if err != nil {
		return 0, nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func (v *infisicalVault) scope() url.Values {
	return url.Values{"workspaceId": {v.project}, "environment": {v.env}, "secretPath": {v.path}}
}

// read returns the stored state.json ("" when the secret does not exist yet).
func (v *infisicalVault) read() (string, error) {
	code, b, err := v.do("GET", "/api/v3/secrets/raw/"+url.PathEscape(v.name), v.scope(), nil)
	if err != nil {
		return "", err
	}
	if code == 404 {
		return "", nil
	}
	if code != 200 {
		return "", fmt.Errorf("infisical read: HTTP %d: %.200s", code, b)
	}
	var out struct {
		Secret struct {
			SecretValue string `json:"secretValue"`
		} `json:"secret"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	return out.Secret.SecretValue, nil
}

func (v *infisicalVault) write(value string, exists bool) error {
	body := map[string]any{
		"workspaceId": v.project, "environment": v.env, "secretPath": v.path,
		"secretValue": value, "type": "shared",
	}
	method := "POST"
	if exists {
		method = "PATCH"
	}
	code, b, err := v.do(method, "/api/v3/secrets/raw/"+url.PathEscape(v.name), nil, body)
	if err != nil {
		return err
	}
	if code == 404 && !exists { // folder missing: create it, then retry once
		if err := v.ensureFolder(); err != nil {
			return err
		}
		code, b, err = v.do(method, "/api/v3/secrets/raw/"+url.PathEscape(v.name), nil, body)
		if err != nil {
			return err
		}
	}
	if code != 200 {
		return fmt.Errorf("infisical write: HTTP %d: %.200s", code, b)
	}
	return nil
}

func (v *infisicalVault) ensureFolder() error {
	cur := "/"
	for _, seg := range strings.Split(strings.Trim(v.path, "/"), "/") {
		if seg == "" {
			continue
		}
		code, b, err := v.do("POST", "/api/v1/folders", nil, map[string]string{
			"workspaceId": v.project, "environment": v.env, "name": seg, "path": cur,
		})
		if err != nil {
			return err
		}
		if code != 200 && code != 400 && code != 409 { // 400/409: already exists
			return fmt.Errorf("infisical folder %q: HTTP %d: %.200s", seg, code, b)
		}
		if cur == "/" {
			cur = "/" + seg
		} else {
			cur += "/" + seg
		}
	}
	return nil
}

func (v *infisicalVault) Restore() (bool, error) {
	if b, err := os.ReadFile(v.statePath); err == nil && len(bytes.TrimSpace(b)) > 2 {
		return false, nil
	}
	val, err := v.read()
	if err != nil || len(strings.TrimSpace(val)) < 2 {
		return false, err
	}
	tmp := v.statePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(val), 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, v.statePath)
}

func (v *infisicalVault) Sync(dryRun bool) (string, error) {
	b, err := os.ReadFile(v.statePath)
	if err != nil || len(bytes.TrimSpace(b)) < 2 {
		return "", nil // nothing to publish yet
	}
	cur, err := v.read()
	if err != nil {
		return "", err
	}
	if cur == string(b) {
		return "unchanged", nil
	}
	if dryRun {
		return "would push", nil
	}
	if err := v.write(string(b), cur != ""); err != nil {
		return "", err
	}
	return "pushed", nil
}
