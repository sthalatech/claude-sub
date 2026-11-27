package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeInfisical implements the few endpoints the vault uses.
func fakeInfisical(t *testing.T, wantAuth string) (*httptest.Server, map[string]string) {
	secrets := map[string]string{}
	folders := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != "Bearer "+wantAuth {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/folders":
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			p := strings.TrimRight(b["path"], "/") + "/" + b["name"]
			folders[p] = true
			w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/api/v3/secrets/raw/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v3/secrets/raw/")
			if r.Method == "GET" {
				if v, ok := secrets[name]; ok {
					json.NewEncoder(w).Encode(map[string]any{"secret": map[string]string{"secretValue": v}})
					return
				}
				http.Error(w, `{"message":"not found"}`, 404)
				return
			}
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			if !folders[b["secretPath"]] {
				http.Error(w, `{"message":"folder not found"}`, 404)
				return
			}
			_, exists := secrets[name]
			if (r.Method == "POST") == exists {
				http.Error(w, "bad method for state", 400)
				return
			}
			secrets[name] = b["secretValue"]
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, secrets
}

func TestInfisicalVaultRestoreAndSync(t *testing.T) {
	srv, secrets := fakeInfisical(t, "tok")
	t.Setenv("INFISICAL_URL", srv.URL)
	t.Setenv("INFISICAL_PROJECT_ID", "p")
	t.Setenv("INFISICAL_TOKEN", "tok")

	a := t.TempDir()
	os.WriteFile(filepath.Join(a, "state.json"), []byte(`{"refresh_token":"r1"}`), 0o600)
	v, err := openVault("", a)
	if err != nil || v == nil {
		t.Fatalf("openVault: %v %v", v, err)
	}
	if s, err := v.Sync(false); err != nil || s != "pushed" {
		t.Fatalf("first sync = %q, %v", s, err)
	}
	if secrets["state_json"] != `{"refresh_token":"r1"}` {
		t.Fatalf("stored %q", secrets["state_json"])
	}
	if s, _ := v.Sync(false); s != "unchanged" {
		t.Fatalf("second sync = %q", s)
	}
	os.WriteFile(filepath.Join(a, "state.json"), []byte(`{"refresh_token":"r2"}`), 0o600)
	if s, err := v.Sync(false); err != nil || s != "pushed" {
		t.Fatalf("update sync = %q, %v", s, err)
	}

	// A brand-new VM restores the login.
	b := t.TempDir()
	v2, _ := openVault("", b)
	if done, err := v2.Restore(); err != nil || !done {
		t.Fatalf("restore = %v, %v", done, err)
	}
	got, _ := os.ReadFile(filepath.Join(b, "state.json"))
	if string(got) != `{"refresh_token":"r2"}` {
		t.Fatalf("restored %q", got)
	}
	// ...and never clobbers an existing login.
	if done, _ := v2.Restore(); done {
		t.Fatal("restore overwrote an existing login")
	}
}

func TestInfisicalVaultNoAuthIsEdgeInjected(t *testing.T) {
	srv, _ := fakeInfisical(t, "")
	t.Setenv("INFISICAL_URL", srv.URL)
	t.Setenv("INFISICAL_PROJECT_ID", "p")
	v, _ := openVault("", t.TempDir())
	if done, err := v.Restore(); err != nil || done {
		t.Fatalf("empty vault restore = %v, %v", done, err)
	}
}

// Newer Infisical servers renamed the project parameter to projectId; the vault
// must be able to speak that dialect too.
func TestInfisicalProjectParamDialect(t *testing.T) {
	var gotQuery, gotBody bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gotQuery = r.URL.Query().Get("projectId") == "p" && r.URL.Query().Get("workspaceId") == ""
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotBody = b["projectId"] == "p" && b["workspaceId"] == nil
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	t.Setenv("INFISICAL_URL", srv.URL)
	t.Setenv("INFISICAL_PROJECT_ID", "p")
	t.Setenv("INFISICAL_PROJECT_PARAM", "projectId")
	t.Setenv("INFISICAL_SECRET_PATH", "/")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"refresh_token":"r"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := openVault("", dir)
	if err != nil || v == nil {
		t.Fatalf("openVault: %v %v", v, err)
	}
	if s, err := v.Sync(false); err != nil || s != "pushed" {
		t.Fatalf("sync = %q, %v", s, err)
	}
	if !gotQuery {
		t.Error("read did not send projectId")
	}
	if !gotBody {
		t.Error("write did not send projectId")
	}
}
