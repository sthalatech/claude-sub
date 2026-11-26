package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, toks Tokens) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SUB_CLAUDE_CREDS", "/nonexistent/creds.json")
	if err := s.SetTokens(toks); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWithClaudeCodeSystem(t *testing.T) {
	cases := []struct{ name, in string }{
		{"absent", `{"model":"m","messages":[]}`},
		{"string", `{"model":"m","system":"be terse","messages":[]}`},
		{"array", `{"model":"m","system":[{"type":"text","text":"be terse"}],"messages":[]}`},
	}
	for _, tc := range cases {
		out, err := withClaudeCodeSystem([]byte(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var payload struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(payload.System) == 0 || !strings.HasPrefix(payload.System[0].Text, claudeCodePrefix) {
			t.Fatalf("%s: prefix missing: %s", tc.name, out)
		}
	}
}

func TestProxyInjectsOAuthHeaders(t *testing.T) {
	var gotAuth, gotBeta, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_1"}`))
	}))
	defer upstream.Close()

	store := newTestStore(t, Tokens{
		AccessToken:  "sk-ant-oat01-token",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
		ClientID:     defaultClientID,
	})
	p := &proxyHandler{store: store, upstream: upstream.URL, ua: defaultUA}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+store.ProxyToken())
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer sk-ant-oat01-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if !strings.Contains(gotBeta, oauthBeta) {
		t.Fatalf("anthropic-beta = %q", gotBeta)
	}
	if !strings.Contains(gotBody, claudeCodePrefix) {
		t.Fatalf("system prefix missing from body: %s", gotBody)
	}
}

func TestProxyRejectsBadToken(t *testing.T) {
	store := newTestStore(t, Tokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)})
	p := &proxyHandler{store: store, upstream: "http://127.0.0.1:1", ua: defaultUA}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestProxyRefreshesAfter401(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-token", "refresh_token": "new-refresh", "expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()
	old := tokenURL
	tokenURL = tokenSrv.URL
	defer func() { tokenURL = old }()

	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fresh-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	store := newTestStore(t, Tokens{
		AccessToken:  "stale-token",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
		ClientID:     defaultClientID,
	})
	p := &proxyHandler{store: store, upstream: upstream.URL, ua: defaultUA}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+store.ProxyToken())
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
	if got := store.Snapshot().AccessToken; got != "fresh-token" {
		t.Fatalf("stored token = %q", got)
	}
}

func TestImportFromClaudeCodeCredentials(t *testing.T) {
	dir := t.TempDir()
	creds := dir + "/creds.json"
	exp := time.Now().Add(2 * time.Hour).UnixMilli()
	body := `{"claudeAiOauth":{"accessToken":"cli-access","refreshToken":"cli-refresh","expiresAt":` +
		itoa(int(exp)) + `,"scopes":["user:inference"],"subscriptionType":"max"}}`
	if err := writeFile(creds, body); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_SUB_CLAUDE_CREDS", creds)

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	changed, err := s.ImportFromClaudeCode(true)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got := s.Snapshot()
	if got.AccessToken != "cli-access" || got.RefreshToken != "cli-refresh" || got.Subscription != "max" {
		t.Fatalf("unexpected tokens: %+v", got)
	}
}
