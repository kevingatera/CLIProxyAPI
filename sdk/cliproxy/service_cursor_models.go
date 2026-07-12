package cliproxy

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func cursorModelsFromAuthMetadata(metadata map[string]any) []*ModelInfo {
	modelIDs := sanitizeCursorModelIDs(extractModelIDsFromMetadata(metadata, "models"))
	if len(modelIDs) == 0 {
		modelIDs = defaultCursorModelIDs()
	}
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(modelIDs))
	for _, id := range modelIDs {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, &ModelInfo{
				ID: id, Object: "model", Created: now, OwnedBy: "cursor", Type: "cursor",
				DisplayName: id, Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
			})
		}
	}
	return out
}

func sanitizeCursorModelIDs(modelIDs []string) []string {
	out := make([]string, 0, len(modelIDs))
	seen := make(map[string]struct{}, len(modelIDs))
	for _, raw := range modelIDs {
		id := strings.TrimSpace(raw)
		lower := strings.ToLower(id)
		if id == "" || strings.HasPrefix(lower, "loading models") || strings.HasPrefix(lower, "available models") || strings.HasPrefix(lower, "tip:") {
			continue
		}
		if parts := strings.SplitN(id, " - ", 2); len(parts) == 2 {
			id = strings.TrimSpace(parts[0])
		}
		id = strings.TrimSpace(strings.TrimSuffix(id, "(default)"))
		key := strings.ToLower(id)
		if id == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	return out
}

func extractModelIDsFromMetadata(metadata map[string]any, key string) []string {
	entries, ok := metadata[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var value string
		switch typed := entry.(type) {
		case string:
			value = strings.TrimSpace(typed)
		case map[string]any:
			value, _ = typed["id"].(string)
			if strings.TrimSpace(value) == "" {
				value, _ = typed["name"].(string)
			}
			value = strings.TrimSpace(value)
		}
		key := strings.ToLower(value)
		if value == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func defaultCursorModelIDs() []string {
	return []string{
		"auto", "composer-1.5", "composer-1", "sonnet-4.6", "sonnet-4.6-thinking",
		"opus-4.6", "opus-4.6-thinking", "gpt-5.4-medium", "gpt-5.4-medium-fast",
		"gpt-5.3-codex", "gpt-5.3-codex-fast", "gemini-3-pro", "gemini-3-flash",
		"grok", "kimi-k2.5",
	}
}

func forceModelPrefixForProvider(provider string, globalForce bool) bool {
	return globalForce || strings.EqualFold(strings.TrimSpace(provider), "cursor")
}
