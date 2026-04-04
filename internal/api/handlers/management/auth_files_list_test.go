package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestListAuthFilesFromDisk_HidesUsageStatsFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "codex-user.json"), []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "usage_stats.json"), []byte(`{"success_count":12}`), 0o600); err != nil {
		t.Fatalf("failed to write usage stats file: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode list payload: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("expected 1 auth file, got %d", len(payload.Files))
	}
	if got := payload.Files[0]["name"]; got != "codex-user.json" {
		t.Fatalf("expected codex auth file, got %v", got)
	}
}

func TestBuildAuthFileEntry_HidesUsageStatsFile(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "usage-stats",
		FileName: "usage_stats.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": filepath.Join(t.TempDir(), "usage_stats.json"),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("failed to register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	if entry := h.buildAuthFileEntry(auth); entry != nil {
		t.Fatalf("expected usage stats file to be hidden, got %#v", entry)
	}
}
