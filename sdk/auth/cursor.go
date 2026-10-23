package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// CursorAPIKeyEnv is the environment variable cursor-agent reads for API-key auth.
	CursorAPIKeyEnv = "CURSOR_API_KEY"
	// CursorAPIKeyEnvMetadataKey records which variable supplies the key. The key
	// value itself is never written to auth metadata or attributes.
	CursorAPIKeyEnvMetadataKey = "api_key_env"
)

// CursorAuthenticator runs `cursor-agent login` and records a cursor auth file.
// The stored file holds no secret; requests use cursor-agent's own login state
// or the CURSOR_API_KEY variable when the record references it.
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

	now := time.Now().UTC()
	fileName := fmt.Sprintf("cursor-%d.json", now.UnixMilli())

	metadata := map[string]any{
		"type":         "cursor",
		"provider_key": "cursor",
		"compat_name":  "cursor",
		"prefix":       "cursor",
		"timestamp":    now.UnixMilli(),
	}
	if len(models) > 0 {
		metadata["models"] = models
	}
	if strings.TrimSpace(os.Getenv(CursorAPIKeyEnv)) != "" {
		metadata[CursorAPIKeyEnvMetadataKey] = CursorAPIKeyEnv
	}
	attrs := map[string]string{
		"provider_key": "cursor",
		"compat_name":  "cursor",
	}
	// Only an explicit relay setting records base_url. Without one the auth runs through the CLI.
	if relay := cursorRelayURLFromEnv(); relay != "" {
		metadata["base_url"] = relay
		attrs["base_url"] = relay
	}

	label := "cursor-agent"
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		label = user
		metadata["label"] = user
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

// cursorRelayURLFromEnv reads the HTTP relay endpoint for new logins. The
// relay is opt-in: it is used only when one of these variables is set.
func cursorRelayURLFromEnv() string {
	for _, key := range []string{"CURSOR_PROXY_BASE_URL", "CURSOR_BASE_URL"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// ResolveCursorAgentPath locates the cursor-agent CLI binary. It is the single
// source of truth for cursor-agent discovery across the codebase (auth login,
// executor, and management handlers). Exported so other packages avoid
// duplicating the candidate list.
func ResolveCursorAgentPath() (string, error) {
	if path, err := exec.LookPath("cursor-agent"); err == nil && strings.TrimSpace(path) != "" {
		return path, nil
	}
	for _, candidate := range []string{
		"/usr/local/bin/cursor-agent",
		"/root/.local/bin/cursor-agent",
		"/root/.local/bin/agent",
	} {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cursor-agent binary not found")
}

func resolveCursorAgentPath() (string, error) { return ResolveCursorAgentPath() }

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
		val := sanitizeCursorModelLine(line)
		if val == "" {
			continue
		}
		// Strip ordinal prefixes like "1. model".
		if parts := strings.SplitN(val, ". ", 2); len(parts) == 2 {
			if _, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
				val = strings.TrimSpace(parts[1])
			}
		}
		val = sanitizeCursorModelLine(val)
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
			val = sanitizeCursorModelLine(v)
		case map[string]any:
			if id, ok := v["id"].(string); ok {
				val = sanitizeCursorModelLine(id)
			}
			if val == "" {
				if name, ok := v["name"].(string); ok {
					val = sanitizeCursorModelLine(name)
				}
			}
			if val == "" {
				if model, ok := v["model"].(string); ok {
					val = sanitizeCursorModelLine(model)
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

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func sanitizeCursorModelLine(line string) string {
	val := strings.TrimSpace(ansiEscapeRE.ReplaceAllString(line, ""))
	if val == "" {
		return ""
	}
	lower := strings.ToLower(val)
	if strings.HasPrefix(lower, "loading models") ||
		strings.HasPrefix(lower, "available models") ||
		strings.HasPrefix(lower, "tip:") {
		return ""
	}
	// Cursor CLI output is often "<id> - <display label>".
	if parts := strings.SplitN(val, " - ", 2); len(parts) == 2 {
		val = strings.TrimSpace(parts[0])
	}
	// Remove common suffix decorations while keeping model id stable.
	val = strings.TrimSuffix(val, "(default)")
	val = strings.TrimSpace(val)
	return val
}
