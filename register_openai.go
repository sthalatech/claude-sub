package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// runRegisterOpenAI registers OpenAI-compatible models (Ollama Cloud, ...) as
// Shelley custom models. The API key comes from an environment variable so it
// never lands on a command line or on disk.
func runRegisterOpenAI(args []string) error {
	fs := flag.NewFlagSet("register-openai", flag.ExitOnError)
	shelley := fs.String("shelley", "http://127.0.0.1:9999", "Shelley base URL")
	user := fs.String("user", "claude-sub", "value for the X-Exedev-Userid header")
	endpoint := fs.String("endpoint", "https://ollama.com/v1", "OpenAI-compatible base URL")
	keyEnv := fs.String("key-env", "OLLAMA_API_KEY", "environment variable holding the API key")
	modelsFile := fs.String("models", "", "JSON file: [{display_name, model_name, max_tokens}]")
	_ = fs.Parse(args)

	key := os.Getenv(*keyEnv)
	if key == "" {
		return fmt.Errorf("%s is empty", *keyEnv)
	}
	if *modelsFile == "" {
		return fmt.Errorf("-models is required")
	}
	raw, err := os.ReadFile(*modelsFile)
	if err != nil {
		return err
	}
	var models []struct {
		DisplayName string `json:"display_name"`
		ModelName   string `json:"model_name"`
		MaxTokens   int64  `json:"max_tokens"`
	}
	if err := json.Unmarshal(raw, &models); err != nil {
		return fmt.Errorf("parse %s: %w", *modelsFile, err)
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
		req.Header.Set("X-Exedev-Userid", *user)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, nil
	}

	code, body, err := do(http.MethodGet, "/api/custom-models", nil)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("list models: HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	var existing []struct {
		ModelID     string `json:"model_id"`
		DisplayName string `json:"display_name"`
		Endpoint    string `json:"endpoint"`
		ModelName   string `json:"model_name"`
	}
	_ = json.Unmarshal(body, &existing)

	for _, m := range models {
		if m.MaxTokens == 0 {
			m.MaxTokens = 200000
		}
		skip := false
		for _, e := range existing {
			if e.Endpoint == *endpoint && e.ModelName == m.ModelName && e.DisplayName == m.DisplayName {
				skip = true
				break
			}
		}
		if skip {
			fmt.Printf("already registered: %s\n", m.DisplayName)
			continue
		}
		payload, _ := json.Marshal(map[string]any{
			"display_name": m.DisplayName, "provider_type": "openai", "endpoint": *endpoint,
			"api_key": key, "model_name": m.ModelName, "max_tokens": m.MaxTokens,
			"image_support": "auto", "reasoning_support": "auto",
		})
		code, b, err := do(http.MethodPost, "/api/custom-models", payload)
		if err != nil {
			return err
		}
		if code/100 != 2 {
			return fmt.Errorf("create %q: HTTP %d: %s", m.DisplayName, code, strings.TrimSpace(string(b)))
		}
		fmt.Printf("registered %s\n", m.DisplayName)
	}
	return nil
}
