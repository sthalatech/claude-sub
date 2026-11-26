package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Tokens is the on-disk representation of a Claude subscription login.
type Tokens struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresAt    time.Time       `json:"expires_at"`
	Scopes       string          `json:"scopes,omitempty"`
	ClientID     string          `json:"client_id,omitempty"`
	Subscription string          `json:"subscription,omitempty"`
	Account      json.RawMessage `json:"account,omitempty"`
	Source       string          `json:"source,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Store owns the token state and the proxy bearer token.
type Store struct {
	dir string

	mu       sync.Mutex
	toks     Tokens
	proxyTok string

	lastImportCheck time.Time
	importStamp     time.Time
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := s.loadOrCreateProxyToken(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Dir() string            { return s.dir }
func (s *Store) statePath() string      { return filepath.Join(s.dir, "state.json") }
func (s *Store) proxyTokenPath() string { return filepath.Join(s.dir, "proxy.token") }

func (s *Store) load() error {
	b, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil {
		return fmt.Errorf("parse %s: %w", s.statePath(), err)
	}
	s.toks = t
	return nil
}

func (s *Store) saveLocked() error {
	s.toks.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(s.toks, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

func (s *Store) loadOrCreateProxyToken() error {
	b, err := os.ReadFile(s.proxyTokenPath())
	if err == nil && len(b) >= 16 {
		s.proxyTok = string(b)
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	tok := "csub_" + hex.EncodeToString(buf)
	if err := os.WriteFile(s.proxyTokenPath(), []byte(tok), 0o600); err != nil {
		return err
	}
	s.proxyTok = tok
	return nil
}

// ProxyToken authenticates local callers (Shelley) against the proxy.
func (s *Store) ProxyToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyTok
}

func (s *Store) CheckProxyToken(got string) bool {
	want := s.ProxyToken()
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// Snapshot returns a copy of the current tokens.
func (s *Store) Snapshot() Tokens {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toks
}

func (s *Store) LoggedIn() bool {
	t := s.Snapshot()
	return t.AccessToken != "" || t.RefreshToken != ""
}

func (s *Store) SetTokens(t Tokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toks = t
	return s.saveLocked()
}

func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toks = Tokens{}
	return s.saveLocked()
}

// updateLocked rewrites the token fields and persists.
func (s *Store) updateLocked(fn func(*Tokens)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.toks)
	return s.saveLocked()
}

// claudeCredsFile is the file the Claude Code CLI writes after `claude /login`.
func claudeCredsFile() string {
	if v := os.Getenv("CLAUDE_SUB_CLAUDE_CREDS"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

type cliCredentials struct {
	ClaudeAiOauth struct {
		AccessToken      string   `json:"accessToken"`
		RefreshToken     string   `json:"refreshToken"`
		ExpiresAt        int64    `json:"expiresAt"`
		Scopes           []string `json:"scopes"`
		SubscriptionType string   `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// ImportFromClaudeCode picks up tokens written by the Claude Code CLI in this VM
// (i.e. after `claude` or `claude setup-token` login), so a login performed
// anywhere on the box is immediately usable by the proxy.
func (s *Store) ImportFromClaudeCode(force bool) (bool, error) {
	path := claudeCredsFile()
	if path == "" {
		return false, nil
	}

	s.mu.Lock()
	if !force && time.Since(s.lastImportCheck) < 20*time.Second {
		s.mu.Unlock()
		return false, nil
	}
	s.lastImportCheck = time.Now()
	s.mu.Unlock()

	st, err := os.Stat(path)
	if err != nil {
		return false, nil
	}

	s.mu.Lock()
	if !force && st.ModTime().Equal(s.importStamp) {
		s.mu.Unlock()
		return false, nil
	}
	s.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	var c cliCredentials
	if err := json.Unmarshal(b, &c); err != nil {
		return false, nil
	}
	a := c.ClaudeAiOauth
	if a.AccessToken == "" || a.RefreshToken == "" {
		return false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.importStamp = st.ModTime()

	theirs := time.UnixMilli(a.ExpiresAt)
	// Adopt the CLI's tokens when ours are missing, expired, or materially older.
	adopt := s.toks.RefreshToken == "" ||
		s.toks.ExpiresAt.Before(time.Now().Add(1*time.Minute)) ||
		theirs.After(s.toks.ExpiresAt.Add(30*time.Minute))
	if !adopt {
		return false, nil
	}

	s.toks = Tokens{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    theirs,
		Scopes:       joinScopes(a.Scopes),
		Subscription: a.SubscriptionType,
		ClientID:     defaultClientID,
		Source:       "claude-code-cli:" + path,
	}
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func joinScopes(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
