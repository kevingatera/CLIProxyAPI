package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	defaultCursorProxyBaseURL = "http://127.0.0.1:32124/v1"
)

// CursorAuthenticator integrates Cursor CLI login and creates a cliproxy auth record
// that routes through an OpenAI-compatible local Cursor proxy.
type CursorAuthenticator struct{}

// NewCursorAuthenticator constructs a new Cursor authenticator.
func NewCursorAuthenticator() Authenticator {
	return &CursorAuthenticator{}
}

func (CursorAuthenticator) Provider() string {
	return "cursor"
}

// Cursor auth is delegated to cursor-agent local session state.
// No token refresh lead is managed by cliproxy.
func (CursorAuthenticator) RefreshLead() *time.Duration {
	return nil
}

func (a CursorAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	_ = cfg
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	cursorAgentPath, err := resolveCursorAgentPath()
	if err != nil {
		return nil, fmt.Errorf("cursor login requires `cursor-agent` in PATH: %w", err)
	}

	skipLogin := false
	if opts.Metadata != nil {
		if v, ok := opts.Metadata["skip_login"]; ok && strings.EqualFold(strings.TrimSpace(v), "true") {
			skipLogin = true
		}
	}

	if opts.NoBrowser && !skipLogin {
		fmt.Println("`--no-browser` requested: please complete Cursor login manually if browser auto-open is unavailable.")
	}

	if !skipLogin {
		loginCmd := exec.CommandContext(ctx, cursorAgentPath, "login")
		loginCmd.Stdout = os.Stdout
		loginCmd.Stderr = os.Stderr
		loginCmd.Stdin = os.Stdin
		if opts.NoBrowser {
			loginCmd.Env = append(os.Environ(), "NO_OPEN_BROWSER=1")
		}
		if err = loginCmd.Run(); err != nil {
			return nil, fmt.Errorf("cursor-agent login failed: %w", err)
		}
	}

	models := fetchCursorModels(ctx, cursorAgentPath)
	baseURL := resolveCursorBaseURL()
	apiKey := strings.TrimSpace(os.Getenv("CURSOR_API_KEY"))

	now := time.Now().UTC()
	fileName := fmt.Sprintf("cursor-%d.json", now.UnixMilli())

	metadata := map[string]any{
		"type":         "cursor",
		"provider_key": "cursor",
		"compat_name":  "cursor",
		"base_url":     baseURL,
		"timestamp":    now.UnixMilli(),
	}
	if len(models) > 0 {
		metadata["models"] = models
	}
	if apiKey != "" {
		metadata["api_key"] = apiKey
	}
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		metadata["label"] = user
	}

	attrs := map[string]string{
		"provider_key": "cursor",
		"compat_name":  "cursor",
		"base_url":     baseURL,
	}
	if apiKey != "" {
		attrs["api_key"] = apiKey
	}

	label := "cursor-agent"
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		label = user
	}

	return &coreauth.Auth{
		ID:         fileName,
		Provider:   "cursor",
		FileName:   fileName,
		Label:      label,
		Attributes: attrs,
		Metadata:   metadata,
	}, nil
}

func resolveCursorAgentPath() (string, error) {
	if path, err := exec.LookPath("cursor-agent"); err == nil {
		return path, nil
	}
	candidates := []string{
		"/usr/local/bin/cursor-agent",
		"/root/.local/bin/cursor-agent",
		"/root/.local/bin/agent",
	}
	for _, p := range candidates {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("cursor-agent binary not found")
}

func resolveCursorBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("CURSOR_PROXY_BASE_URL")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("CURSOR_BASE_URL")); v != "" {
		return v
	}
	return defaultCursorProxyBaseURL
}

func fetchCursorModels(ctx context.Context, cursorAgentPath string) []string {
	cmd := exec.CommandContext(ctx, cursorAgentPath, "models")
	out, err := cmd.Output()
	if err != nil {
		log.Debugf("cursor-agent models failed: %v", err)
		return nil
	}
	return parseCursorModelsOutput(out)
}

func parseCursorModelsOutput(raw []byte) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}

	// JSON shape 1: ["model-a", ...]
	var arr []any
	if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
		out := normalizeCursorModelEntries(arr)
		if len(out) > 0 {
			return out
		}
	}

	// JSON shape 2: {"models":[...]}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
		if rawModels, ok := obj["models"]; ok {
			if entries, okCast := rawModels.([]any); okCast {
				out := normalizeCursorModelEntries(entries)
				if len(out) > 0 {
					return out
				}
			}
		}
	}

	// Plain-text fallback: one model per line.
	lines := strings.Split(trimmed, "\n")
	out := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		val := strings.TrimSpace(line)
		if val == "" {
			continue
		}
		// Strip ordinal prefixes like "1. model".
		if parts := strings.SplitN(val, ". ", 2); len(parts) == 2 {
			if _, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
				val = strings.TrimSpace(parts[1])
			}
		}
		if val == "" {
			continue
		}
		key := strings.ToLower(val)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, val)
	}
	return out
}

func normalizeCursorModelEntries(entries []any) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		val := ""
		switch v := entry.(type) {
		case string:
			val = strings.TrimSpace(v)
		case map[string]any:
			if id, ok := v["id"].(string); ok {
				val = strings.TrimSpace(id)
			}
			if val == "" {
				if name, ok := v["name"].(string); ok {
					val = strings.TrimSpace(name)
				}
			}
			if val == "" {
				if model, ok := v["model"].(string); ok {
					val = strings.TrimSpace(model)
				}
			}
		}
		if val == "" {
			continue
		}
		key := strings.ToLower(val)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, val)
	}
	return out
}
