package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

type webHandler struct {
	store    *Store
	logins   *loginStore
	clientID string
	scope    string
	proxyURL string
}

func (h *webHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		w.Write([]byte("ok\n"))
	case r.URL.Path == "/login":
		h.handleLogin(w, r)
	case r.URL.Path == "/oauth/exchange":
		h.handleExchange(w, r)
	case r.URL.Path == "/logout":
		h.handleLogout(w, r)
	case r.URL.Path == "/export":
		h.handleExport(w, r)
	case r.URL.Path == "/import":
		h.handleImport(w, r)
	case r.URL.Path == "/" || r.URL.Path == "":
		h.handleStatus(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *webHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	verifier, challenge, err := pkce()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state, err := randB64(24)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.logins.put(state, verifier)
	http.Redirect(w, r, authorizeURLFor(h.clientID, challenge, state, h.scope), http.StatusFound)
}

func (h *webHandler) handleExchange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(r.FormValue("code"))
	if raw == "" {
		http.Redirect(w, r, "/?err="+urlQuery("paste the code shown after signing in"), http.StatusFound)
		return
	}

	code, state := parseCallback(raw)
	if code == "" {
		http.Redirect(w, r, "/?err="+urlQuery("could not find a code in that paste"), http.StatusFound)
		return
	}

	verifier, ok := h.logins.take(state)
	if !ok {
		http.Redirect(w, r, "/?err="+urlQuery("that sign-in expired or was already used; click Log in with Claude again"), http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := h.store.Exchange(ctx, h.clientID, code, verifier, state); err != nil {
		http.Redirect(w, r, "/?err="+urlQuery(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/?ok=1", http.StatusFound)
}

func (h *webHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := h.store.Clear(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *webHandler) handleExport(w http.ResponseWriter, r *http.Request) {
	t := h.store.Snapshot()
	if t.RefreshToken == "" {
		http.Error(w, "not logged in", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="claude-sub-state.json"`)
	_ = json.NewEncoder(w).Encode(t)
}

func (h *webHandler) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pasted := strings.TrimSpace(r.FormValue("state"))
	ctx := r.Context()

	// A raw OAuth access token (sk-ant-oat01-...) is accepted too.
	if pasted != "" && !strings.HasPrefix(pasted, "{") {
		ids, err := verifyAccessToken(ctx, pasted)
		if err != nil {
			http.Redirect(w, r, "/?err="+urlQuery(err.Error()), http.StatusFound)
			return
		}
		if err := h.store.SetTokens(Tokens{
			AccessToken: pasted,
			ExpiresAt:   time.Now().Add(365 * 24 * time.Hour).UTC(),
			Scopes:      defaultScope,
			ClientID:    h.clientID,
			Source:      "pasted-token",
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.store.setSubscription(h.clientID, strings.Join(ids, " "))
		http.Redirect(w, r, "/?ok=1", http.StatusFound)
		return
	}

	var t Tokens
	if err := json.Unmarshal([]byte(pasted), &t); err != nil {
		http.Redirect(w, r, "/?err="+urlQuery("could not parse that JSON: "+err.Error()), http.StatusFound)
		return
	}
	if t.RefreshToken == "" && t.AccessToken == "" {
		http.Redirect(w, r, "/?err="+urlQuery("that JSON has no token"), http.StatusFound)
		return
	}
	t.Source = "imported"
	if err := h.store.SetTokens(t); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?ok=1", http.StatusFound)
}

func (h *webHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	t := h.store.Snapshot()
	data := map[string]any{
		"Ok":           r.URL.Query().Get("ok") != "",
		"Err":          r.URL.Query().Get("err"),
		"LoggedIn":     t.RefreshToken != "",
		"Source":       t.Source,
		"Subscription": t.Subscription,
		"Expires":      "",
		"ExpiresSoon":  false,
		"Email":        emailOf(t.Account),
		"ProxyURL":     h.proxyURL,
		"Token":        mask(t.AccessToken),
		"Version":      version,
	}
	if !t.ExpiresAt.IsZero() {
		data["Expires"] = t.ExpiresAt.Local().Format("2006-01-02 15:04 MST")
		data["ExpiresSoon"] = time.Until(t.ExpiresAt) < 10*time.Minute
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, data); err != nil {
		fmt.Fprintf(w, "template error: %v", err)
	}
}

func emailOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		EmailAddress string `json:"email_address"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.EmailAddress
}

func mask(s string) string {
	if len(s) < 12 {
		if s == "" {
			return ""
		}
		return "****"
	}
	return s[:10] + "..." + s[len(s)-4:]
}

func urlQuery(s string) string {
	out := ""
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~':
			out += string(r)
		default:
			out += fmt.Sprintf("%%%02X", r)
		}
	}
	return out
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>claude-sub</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 15px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
         max-width: 46rem; margin: 3rem auto; padding: 0 1.25rem; }
  h1 { font-size: 1.35rem; margin: 0 0 .25rem; }
  .sub { color: #6b7280; margin: 0 0 2rem; }
  .card { border: 1px solid #d1d5db; border-radius: 10px; padding: 1.1rem 1.25rem; margin-bottom: 1.1rem; }
  .row { display: flex; gap: .75rem; flex-wrap: wrap; align-items: baseline; }
  .k { color: #6b7280; min-width: 8.5rem; }
  .ok { color: #047857; font-weight: 600; }
  .warn { color: #b45309; font-weight: 600; }
  .bad { color: #b91c1c; font-weight: 600; }
  code, input, textarea { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
  code { background: rgba(127,127,127,.14); padding: .1rem .3rem; border-radius: 4px; }
  input[type=text], textarea { width: 100%; box-sizing: border-box; padding: .55rem .6rem;
         border: 1px solid #9ca3af; border-radius: 7px; background: transparent; color: inherit; }
  textarea { min-height: 5.5rem; }
  button, .btn { display: inline-block; font: inherit; font-weight: 600; padding: .55rem 1rem;
         border-radius: 7px; border: 1px solid #111827; background: #111827; color: #fff;
         cursor: pointer; text-decoration: none; }
  .btn.ghost, button.ghost { background: transparent; color: inherit; border-color: #9ca3af; font-weight: 500; }
  ol { padding-left: 1.2rem; }
  li { margin: .3rem 0; }
  .err { border-color: #b91c1c; color: #b91c1c; }
  .banner { border: 1px solid #047857; border-radius: 10px; padding: .8rem 1.1rem; margin-bottom: 1.1rem; }
  details summary { cursor: pointer; color: #6b7280; }
</style></head><body>
<h1>claude-sub</h1>
<p class="sub">Claude subscription &rarr; Anthropic Messages API proxy, v{{.Version}}</p>

{{if .Ok}}<div class="banner ok">Signed in. New conversations can use this model now.</div>{{end}}
{{if .Err}}<div class="banner err">{{.Err}}</div>{{end}}

<div class="card">
  <div class="row"><span class="k">Status</span>
  {{if .LoggedIn}}<span class="ok">signed in</span>{{else}}<span class="bad">not signed in</span>{{end}}</div>
  {{if .Email}}<div class="row"><span class="k">Account</span><span>{{.Email}}</span></div>{{end}}
  {{if .Subscription}}<div class="row"><span class="k">Plan</span><span>{{.Subscription}}</span></div>{{end}}
  {{if .Source}}<div class="row"><span class="k">Source</span><span><code>{{.Source}}</code></span></div>{{end}}
  {{if .Token}}<div class="row"><span class="k">Access token</span><span><code>{{.Token}}</code></span></div>{{end}}
  {{if .Expires}}<div class="row"><span class="k">Access expires</span>
    <span {{if .ExpiresSoon}}class="warn"{{end}}>{{.Expires}}{{if .ExpiresSoon}} (refreshing soon){{end}}</span></div>{{end}}
  <div class="row"><span class="k">Proxy endpoint</span><span><code>{{.ProxyURL}}</code></span></div>
  {{if .LoggedIn}}
  <div class="row" style="margin-top:1rem">
    <form method="post" action="/logout"><button class="ghost" type="submit">Sign out</button></form>
    <a class="btn ghost" href="/export">Export tokens</a>
    <a class="btn ghost" href="/login">Sign in again</a>
  </div>
  {{end}}
</div>

{{if not .LoggedIn}}
<div class="card">
  <h2 style="font-size:1.05rem;margin:.1rem 0 .7rem">Sign in with your Claude subscription</h2>
  <ol>
    <li><a class="btn" href="/login">Log in with Claude</a> &mdash; opens claude.com in this browser.</li>
    <li>Approve the request. The page that follows shows a <code>code</code>.</li>
    <li>Copy it (or the whole <code>code#state</code>) and paste it below.</li>
  </ol>
  <form method="post" action="/oauth/exchange">
    <input type="text" name="code" placeholder="paste the code here" autocomplete="off" autofocus>
    <div class="row" style="margin-top:.7rem"><button type="submit">Finish sign-in</button></div>
  </form>
  <p style="color:#6b7280;margin:.9rem 0 0">Already signed in with the <code>claude</code> CLI on this VM?
     <code>claude setup-token</code> writes credentials the proxy picks up automatically.</p>
  <details style="margin-top:.9rem">
    <summary>I already have a token</summary>
    <p style="color:#6b7280">Paste an OAuth access token (<code>sk-ant-oat01-&hellip;</code>) or a token bundle. It is
       checked against Anthropic before it is stored.</p>
    <form method="post" action="/import">
      <textarea name="state" placeholder="sk-ant-oat01-..."></textarea>
      <div class="row" style="margin-top:.7rem"><button class="ghost" type="submit">Use this token</button></div>
    </form>
  </details>
</div>
{{end}}

<div class="card">
  <details>
    <summary>Use this on another VM (import)</summary>
    <p style="color:#6b7280">Export the token bundle from the VM that is signed in, then paste it here to avoid signing in again.</p>
    <form method="post" action="/import">
      <textarea name="state" placeholder='{"refresh_token": "...", ...}'></textarea>
      <div class="row" style="margin-top:.7rem"><button class="ghost" type="submit">Import tokens</button></div>
    </form>
  </details>
</div>
</body></html>
`))
