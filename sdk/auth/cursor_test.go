package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestParseCursorModelsOutput_JSONArray(t *testing.T) {
	raw := []byte(`["auto","gpt-5.4-medium",{"id":"sonnet-4.5"},"auto"]`)
	got := parseCursorModelsOutput(raw)
	if len(got) != 3 {
		t.Fatalf("expected 3 models, got %d: %#v", len(got), got)
	}
	if got[0] != "auto" || got[1] != "gpt-5.4-medium" || got[2] != "sonnet-4.5" {
		t.Fatalf("unexpected models: %#v", got)
	}
}

func TestParseCursorModelsOutput_JSONObject(t *testing.T) {
	raw := []byte(`{"models":[{"name":"composer-1.5"},{"model":"grok"}]}`)
	got := parseCursorModelsOutput(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %#v", len(got), got)
	}
	if got[0] != "composer-1.5" || got[1] != "grok" {
		t.Fatalf("unexpected models: %#v", got)
	}
}

func TestParseCursorModelsOutput_LineFallback(t *testing.T) {
	raw := []byte("1. auto\n2. sonnet-4.6\n\n")
	got := parseCursorModelsOutput(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %#v", len(got), got)
	}
	if got[0] != "auto" || got[1] != "sonnet-4.6" {
		t.Fatalf("unexpected models: %#v", got)
	}
}

// installFakeCursorAgentLogin puts a cursor-agent stub alone on PATH. It accepts
// `login` and prints a model list for `models`.
func installFakeCursorAgentLogin(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub requires a POSIX shell")
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  login) exit 0 ;;
  models) printf '%s\n' '{"models":["auto",{"id":"sonnet-4.6"}]}' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "cursor-agent"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir)
}

func TestCursorLoginNeverPersistsAPIKeyValue(t *testing.T) {
	installFakeCursorAgentLogin(t)
	const secret = "sk-cursor-secret-value"
	t.Setenv(CursorAPIKeyEnv, secret)

	record, err := NewCursorAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{})
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	persisted, err := json.Marshal(map[string]any{"metadata": record.Metadata, "attributes": record.Attributes})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), secret) {
		t.Fatalf("API key value was written to the auth record: %s", persisted)
	}
	if _, ok := record.Metadata["api_key"]; ok {
		t.Fatal("plaintext api_key field present in metadata")
	}
	if got := record.Metadata[CursorAPIKeyEnvMetadataKey]; got != CursorAPIKeyEnv {
		t.Fatalf("%s = %v, want the env var name", CursorAPIKeyEnvMetadataKey, got)
	}
	if record.Provider != "cursor" || record.FileName == "" {
		t.Fatalf("record provider/file = %q/%q", record.Provider, record.FileName)
	}
	if models, _ := record.Metadata["models"].([]string); len(models) != 2 {
		t.Fatalf("models metadata = %#v, want two models", record.Metadata["models"])
	}
}

func TestCursorLoginWithoutEnvKeyRecordsNoReference(t *testing.T) {
	installFakeCursorAgentLogin(t)
	t.Setenv(CursorAPIKeyEnv, "")

	record, err := NewCursorAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{})
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if _, ok := record.Metadata[CursorAPIKeyEnvMetadataKey]; ok {
		t.Fatal("api_key_env should be absent when the login process has no key")
	}
}

func TestCursorLoginReportsMissingCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := NewCursorAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{Metadata: map[string]string{"skip_login": "true"}})
	if err == nil || !strings.Contains(err.Error(), "cursor-agent") {
		t.Fatalf("Login() error = %v, want a missing cursor-agent error", err)
	}
}

// Auth files written before this rewrite carry base_url and no secret. They must still load.
func TestLegacyCursorAuthFileStillLoads(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"type":"cursor","provider_key":"cursor","compat_name":"cursor","prefix":"cursor",` +
		`"base_url":"http://127.0.0.1:32124/v1","models":["auto","composer-1.5"],"label":"kevin","timestamp":1765000000000}`
	if err := os.WriteFile(filepath.Join(dir, "cursor-1765000000000.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("loaded %d auths, want 1", len(auths))
	}
	if auths[0].Provider != "cursor" {
		t.Fatalf("provider = %q, want cursor", auths[0].Provider)
	}
	if _, ok := auths[0].Metadata["api_key"]; ok {
		t.Fatal("legacy file unexpectedly gained an api_key")
	}
}

func TestCursorLoginRecordsRelayOnlyWhenConfigured(t *testing.T) {
	installFakeCursorAgentLogin(t)
	t.Setenv(CursorAPIKeyEnv, "")
	t.Setenv("CURSOR_PROXY_BASE_URL", "")
	t.Setenv("CURSOR_BASE_URL", "")

	record, err := NewCursorAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{})
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if _, ok := record.Metadata["base_url"]; ok {
		t.Fatal("base_url recorded without an explicit relay setting")
	}

	t.Setenv("CURSOR_PROXY_BASE_URL", "http://192.168.1.20:32125/v1")
	record, err = NewCursorAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{})
	if err != nil {
		t.Fatalf("Login() with relay error: %v", err)
	}
	if got := record.Metadata["base_url"]; got != "http://192.168.1.20:32125/v1" {
		t.Fatalf("metadata base_url = %v, want the relay URL", got)
	}
	if got := record.Attributes["base_url"]; got != "http://192.168.1.20:32125/v1" {
		t.Fatalf("attribute base_url = %q, want the relay URL", got)
	}
}
