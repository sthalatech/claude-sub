package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

type proxyHandler struct {
	store    *Store
	upstream string
	ua       string
}

func (p *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "unauthorized: send the proxy token as Authorization: Bearer or x-api-key", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/messages") && !strings.HasSuffix(r.URL.Path, "count_tokens") {
		if nb, err := withClaudeCodeSystem(body); err != nil {
			log.Printf("warning: could not inject system prompt: %v", err)
		} else {
			body = nb
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		tok, err := p.store.Access(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, p.upstream+r.URL.Path+queryOf(r), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		copyProxyHeaders(req.Header, r.Header)
		req.Header.Del("X-Api-Key")
		req.Header.Del("Authorization")
		req.Header.Set("Authorization", "Bearer "+tok)
		betas := []string{oauthBeta, codeBeta}
		for _, extra := range strings.Split(r.Header.Get("anthropic-beta"), ",") {
			extra = strings.TrimSpace(extra)
			if extra == "" || extra == oauthBeta || extra == codeBeta {
				continue
			}
			betas = append(betas, extra)
		}
		req.Header.Set("anthropic-beta", strings.Join(betas, ","))
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		if req.Header.Get("User-Agent") == "" || strings.HasPrefix(req.Header.Get("User-Agent"), "Go-http") {
			req.Header.Set("User-Agent", p.ua)
		}
		req.Header.Set("Content-Length", itoa(len(body)))

		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			return
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			log.Printf("upstream 401; forcing token refresh and retrying")
			if _, err := p.store.refresh(r.Context()); err != nil {
				log.Printf("refresh after 401 failed: %v", err)
				break
			}
			continue
		}

		writeUpstream(w, resp)
		return
	}

	http.Error(w, "upstream rejected the request", http.StatusBadGateway)
}

func (p *proxyHandler) authorized(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.Header.Get("X-Api-Key")
	}
	if r.Header.Get("Authorization") == r.Header.Get("X-Api-Key") {
		// both empty
		return false
	}
	return p.store.CheckProxyToken(strings.TrimSpace(tok))
}

func copyProxyHeaders(dst, src http.Header) {
	hop := map[string]bool{
		"connection": true, "keep-alive": true, "proxy-authenticate": true,
		"proxy-authorization": true, "te": true, "trailer": true,
		"transfer-encoding": true, "upgrade": true, "host": true,
		"content-length": true, "authorization": true, "x-api-key": true,
		"anthropic-beta": true, "accept-encoding": true,
	}
	for k, vs := range src {
		if hop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func writeUpstream(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "content-length", "transfer-encoding", "connection", "keep-alive":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// withClaudeCodeSystem ensures the request's system prompt starts with the
// Claude Code preamble, which subscription OAuth credentials require.
func withClaudeCodeSystem(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return body, err
	}

	prefix := map[string]any{"type": "text", "text": claudeCodePrefix}
	switch sys := payload["system"].(type) {
	case nil:
		payload["system"] = []any{prefix}
	case string:
		if strings.HasPrefix(strings.TrimSpace(sys), claudeCodePrefix) {
			return body, nil
		}
		payload["system"] = []any{prefix, map[string]any{"type": "text", "text": sys}}
	case []any:
		if len(sys) > 0 {
			if m, ok := sys[0].(map[string]any); ok {
				if s, _ := m["text"].(string); strings.HasPrefix(strings.TrimSpace(s), claudeCodePrefix) {
					return body, nil
				}
			}
		}
		payload["system"] = append([]any{prefix}, sys...)
	default:
		return body, nil
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return body, err
	}
	return out, nil
}
