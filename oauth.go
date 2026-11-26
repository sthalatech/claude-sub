package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Constants mirror the Claude Code CLI (extracted from the installed binary).
const (
	defaultClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	authorizeURL    = "https://claude.com/cai/oauth/authorize"
	manualRedirect  = "https://platform.claude.com/oauth/code/callback"

	oauthBeta    = "oauth-2025-04-20"
	codeBeta     = "claude-code-20250219"
	defaultUA    = "claude-cli/2.1.216 (external, cli)"
	defaultScope = "user:inference user:profile"

	// claudeCodePrefix is prepended to every request's system prompt; the
	// subscription OAuth credential is only accepted for Claude Code traffic.
	claudeCodePrefix = "You are Claude Code, Anthropic's official CLI for Claude."
)

// Endpoints are variables so tests can point them at a local server.
var (
	tokenURL   = "https://platform.claude.com/v1/oauth/token"
	profileURL = "https://api.anthropic.com/api/oauth/profile"
	apiBaseURL = "https://api.anthropic.com"
)

var httpClient = &http.Client{Timeout: 120 * time.Second}

// loginSessions holds in-flight PKCE verifiers, keyed by state.
type loginSession struct {
	verifier string
	created  time.Time
}

type loginStore struct {
	mu   sync.Mutex
	m    map[string]loginSession
	path string // persisted so a restart does not strand a code the user holds
}

func newLoginStore(path string) *loginStore {
	l := &loginStore{m: map[string]loginSession{}, path: path}
	l.load()
	return l
}

func (l *loginStore) load() {
	if l.path == "" {
		return
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var m map[string]loginSession
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for k, v := range m {
		if time.Since(v.created) < 30*time.Minute {
			l.m[k] = v
		}
	}
}

func (l *loginStore) saveLocked() {
	if l.path == "" {
		return
	}
	b, err := json.Marshal(l.m)
	if err != nil {
		return
	}
	_ = os.WriteFile(l.path, b, 0o600)
}

func (l *loginStore) put(state, verifier string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range l.m {
		if time.Since(v.created) > 30*time.Minute {
			delete(l.m, k)
		}
	}
	l.m[state] = loginSession{verifier: verifier, created: time.Now()}
	l.saveLocked()
}

// take returns the verifier for a state, falling back to the only pending
// session when the caller pasted a bare code or a state we no longer match.
func (l *loginStore) take(state string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if v, ok := l.m[state]; ok && time.Since(v.created) < 30*time.Minute {
		delete(l.m, state)
		l.saveLocked()
		return v.verifier, true
	}
	var (
		onlyState string
		only      loginSession
		n         int
	)
	for k, v := range l.m {
		if time.Since(v.created) > 30*time.Minute {
			delete(l.m, k)
			continue
		}
		onlyState, only, n = k, v, n+1
	}
	if n == 1 {
		delete(l.m, onlyState)
		l.saveLocked()
		return only.verifier, true
	}
	return "", false
}

func randB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkce() (verifier, challenge string, err error) {
	verifier, err = randB64(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func authorizeURLFor(clientID, challenge, state, scope string) string {
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", manualRedirect)
	q.Set("scope", scope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return authorizeURL + "?" + q.Encode()
}

type tokenResponse struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresIn    int64           `json:"expires_in"`
	Scope        string          `json:"scope"`
	Account      json.RawMessage `json:"account"`
	Organization json.RawMessage `json:"organization"`
	Error        string          `json:"error"`
	ErrorDesc    string          `json:"error_description"`
}

func postToken(ctx context.Context, body any) (*tokenResponse, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("User-Agent", defaultUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr tokenResponse
	_ = json.Unmarshal(raw, &tr)
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw))
		if tr.Error != "" {
			msg = tr.Error
			if tr.ErrorDesc != "" {
				msg += ": " + tr.ErrorDesc
			}
		}
		return nil, fmt.Errorf("token endpoint HTTP %d: %s", resp.StatusCode, msg)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned no access_token: %s", strings.TrimSpace(string(raw)))
	}
	return &tr, nil
}

func expiryFrom(expiresIn int64) time.Time {
	if expiresIn <= 0 {
		expiresIn = 31536000 // Claude Code assumes a year when the server omits it
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second).UTC()
}

// Exchange turns an authorization code ("code#state" or bare code) into tokens.
func (s *Store) Exchange(ctx context.Context, clientID, code, verifier, state string) error {
	tr, err := postToken(ctx, map[string]any{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  manualRedirect,
		"client_id":     clientID,
		"code_verifier": verifier,
		"state":         state,
	})
	if err != nil {
		return err
	}
	t := Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    expiryFrom(tr.ExpiresIn),
		Scopes:       tr.Scope,
		ClientID:     clientID,
		Account:      tr.Account,
		Source:       "oauth-login",
	}
	if t.Scopes == "" {
		t.Scopes = defaultScope
	}
	if err := s.SetTokens(t); err != nil {
		return err
	}
	if sub, err := s.fetchProfile(ctx); err == nil && sub != "" {
		_ = s.updateLocked(func(t *Tokens) { t.Subscription = sub })
	}
	return nil
}

// fetchProfile asks Anthropic who we are and which plan the subscription is.
func (s *Store) fetchProfile(ctx context.Context) (string, error) {
	tok, err := s.Access(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, profileURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("User-Agent", defaultUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("profile HTTP %d", resp.StatusCode)
	}
	var p struct {
		Account struct {
			EmailAddress string `json:"email_address"`
		} `json:"account"`
		Organization struct {
			SubscriptionType string `json:"subscription_type"`
			RateLimitTier    string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return "", err
	}
	sub := p.Organization.SubscriptionType
	if p.Organization.RateLimitTier != "" {
		sub += " (" + p.Organization.RateLimitTier + ")"
	}
	if p.Account.EmailAddress != "" {
		_ = s.updateLocked(func(t *Tokens) {
			t.Account = json.RawMessage(fmt.Sprintf(`{"email_address":%q}`, p.Account.EmailAddress))
		})
	}
	return strings.TrimSpace(sub), nil
}

var errNotLoggedIn = errors.New("not logged in: open the web UI and sign in with your Claude subscription")

// Access returns a usable access token, refreshing or importing when needed.
func (s *Store) Access(ctx context.Context) (string, error) {
	if _, err := s.ImportFromClaudeCode(false); err != nil {
		return "", err
	}

	s.mu.Lock()
	t := s.toks
	s.mu.Unlock()

	if t.AccessToken != "" && time.Until(t.ExpiresAt) > 2*time.Minute {
		return t.AccessToken, nil
	}
	if t.RefreshToken == "" {
		return "", errNotLoggedIn
	}
	return s.refresh(ctx)
}

func (s *Store) refresh(ctx context.Context) (string, error) {
	s.mu.Lock()
	t := s.toks
	s.mu.Unlock()

	clientID := t.ClientID
	if clientID == "" {
		clientID = defaultClientID
	}
	tr, err := postToken(ctx, map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": t.RefreshToken,
		"client_id":     clientID,
	})
	if err != nil {
		return "", fmt.Errorf("refresh: %w", err)
	}

	err = s.updateLocked(func(cur *Tokens) {
		cur.AccessToken = tr.AccessToken
		cur.ExpiresAt = expiryFrom(tr.ExpiresIn)
		cur.ClientID = clientID
		if tr.RefreshToken != "" {
			cur.RefreshToken = tr.RefreshToken
		}
		if tr.Scope != "" {
			cur.Scopes = tr.Scope
		}
		if tr.Account != nil {
			cur.Account = tr.Account
		}
		cur.Source = "refresh"
	})
	if err != nil {
		return tr.AccessToken, nil
	}
	return tr.AccessToken, nil
}

// parseCallback accepts a bare code, `code#state`, or the full callback URL
// Anthropic redirects to, and returns the authorization code and state.
func parseCallback(raw string) (code, state string) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", ""
		}
		q := u.Query()
		return q.Get("code"), q.Get("state")
	}
	if i := strings.LastIndex(raw, "#"); i > 0 {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

// verifyAccessToken checks a token against the upstream models endpoint.
func verifyAccessToken(ctx context.Context, tok string) ([]string, error) {
	tok = strings.TrimPrefix(strings.TrimSpace(tok), "Bearer ")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("User-Agent", defaultUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("that token was rejected (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// setSubscription records what the token can see (used for the status page).
func (s *Store) setSubscription(clientID, note string) {
	_ = s.updateLocked(func(t *Tokens) {
		if t.ClientID == "" {
			t.ClientID = clientID
		}
		if note != "" {
			t.Subscription = "models: " + note
		}
	})
}
