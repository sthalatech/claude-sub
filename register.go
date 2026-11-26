package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runRegister adds (or refreshes) the claude-sub custom model in a local Shelley.
//
// Shelley exposes custom models at POST /api/custom-models; for provider_type
// "anthropic" it calls <endpoint>/v1/messages with the configured api_key.
func runRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	shelley := fs.String("shelley", "http://127.0.0.1:9999", "Shelley base URL")
	user := fs.String("user", "claude-sub", "value for the X-Exedev-Userid header")
	state := fs.String("state", "", "state directory")
	display := fs.String("display", "Claude Opus (subscription)", "display name in Shelley's model list")
	modelName := fs.String("model-name", "", "model name forwarded to Anthropic (empty = pick the newest Opus)")
	endpoint := fs.String("endpoint", "http://127.0.0.1:8788/v1/messages", "claude-sub proxy URL (Shelley uses this verbatim)")
	maxTokens := fs.Int64("max-tokens", 200000, "context window recorded in Shelley")
	_ = fs.Parse(args)

	store, err := NewStore(stateDir(nil, *state))
	if err != nil {
		return err
	}

	upstreamModel := *modelName
	if upstreamModel == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ids, err := store.UpstreamModels(ctx)
		if err != nil {
			return fmt.Errorf("list upstream models (pass -model-name to skip): %w", err)
		}
		upstreamModel = pickModel(ids)
		if upstreamModel == "" {
			return fmt.Errorf("no upstream models available; pass -model-name explicitly")
		}
	}

	base := strings.TrimRight(*shelley, "/")
	client := &http.Client{Timeout: 30 * time.Second}

	do := func(method, path string, body []byte) (int, []byte, error) {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, base+path, rdr)
		if err != nil {
			return 0, nil, err
		}
		if *user != "" {
			req.Header.Set("X-Exedev-Userid", *user)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, raw, nil
	}

	// Remove an earlier registration so re-running stays idempotent.
	code, raw, err := do(http.MethodGet, "/api/custom-models", nil)
	if err != nil {
		return fmt.Errorf("list custom models: %w", err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("list custom models: HTTP %d: %s", code, strings.TrimSpace(string(raw)))
	}
	var existing []struct {
		ModelID     string `json:"model_id"`
		DisplayName string `json:"display_name"`
		Endpoint    string `json:"endpoint"`
		ModelName   string `json:"model_name"`
	}
	_ = json.Unmarshal(raw, &existing)
	for _, m := range existing {
		// Replace our own earlier registration of the same display name, or of
		// the same upstream model behind this endpoint; leave siblings alone.
		sameModel := m.Endpoint == *endpoint && m.ModelName == upstreamModel
		if m.DisplayName != *display && !sameModel {
			continue
		}
		if c, r2, err := do(http.MethodDelete, "/api/custom-models/"+m.ModelID, nil); err != nil {
			return fmt.Errorf("delete existing model %q: %w", m.ModelID, err)
		} else if c/100 != 2 {
			fmt.Printf("warning: could not delete existing model %q (HTTP %d: %s)\n", m.ModelID, c, strings.TrimSpace(string(r2)))
		} else {
			fmt.Printf("replaced existing model %q\n", m.ModelID)
		}
	}

	payload := map[string]any{
		"display_name":      *display,
		"provider_type":     "anthropic",
		"endpoint":          *endpoint,
		"api_key":           store.ProxyToken(),
		"model_name":        upstreamModel,
		"max_tokens":        *maxTokens,
		"image_support":     "auto",
		"reasoning_support": "auto",
	}
	body, _ := json.Marshal(payload)
	code, raw, err = do(http.MethodPost, "/api/custom-models", body)
	if err != nil {
		return fmt.Errorf("create model: %w", err)
	}
	if code/100 != 2 {
		return fmt.Errorf("create model: HTTP %d: %s", code, strings.TrimSpace(string(raw)))
	}
	fmt.Printf("registered %q (%s) in Shelley at %s\n", *display, upstreamModel, base)
	return nil
}

// UpstreamModels asks Anthropic (through the subscription) which models exist.
func (s *Store) UpstreamModels(ctx context.Context) ([]string, error) {
	tok, err := s.Access(ctx)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("upstream models: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
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

// pickModel prefers the newest Opus, then any model.
func pickModel(ids []string) string {
	for _, id := range ids {
		if strings.Contains(id, "opus") {
			return id
		}
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}
