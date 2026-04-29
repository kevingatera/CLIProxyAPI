// Package registry provides model definitions and lookup helpers for various AI providers.
// Static model metadata is loaded from the embedded models.json file and can be refreshed from network.
package registry

import (
	"strings"
)

const (
	codexPrimaryModelID = "gpt-5.4"
	codexMiniModelID    = "gpt-5.4-mini"
)

// staticModelsJSON mirrors the top-level structure of models.json.
type staticModelsJSON struct {
	Claude      []*ModelInfo `json:"claude"`
	Gemini      []*ModelInfo `json:"gemini"`
	Vertex      []*ModelInfo `json:"vertex"`
	GeminiCLI   []*ModelInfo `json:"gemini-cli"`
	AIStudio    []*ModelInfo `json:"aistudio"`
	CodexFree   []*ModelInfo `json:"codex-free"`
	CodexTeam   []*ModelInfo `json:"codex-team"`
	CodexPlus   []*ModelInfo `json:"codex-plus"`
	CodexPro    []*ModelInfo `json:"codex-pro"`
	Qwen        []*ModelInfo `json:"qwen"`
	IFlow       []*ModelInfo `json:"iflow"`
	Kimi        []*ModelInfo `json:"kimi"`
	Antigravity []*ModelInfo `json:"antigravity"`
}

// GetClaudeModels returns the standard Claude model definitions.
func GetClaudeModels() []*ModelInfo {
	return cloneModelInfos(getModels().Claude)
}

// GetGeminiModels returns the standard Gemini model definitions.
func GetGeminiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Gemini)
}

// GetGeminiVertexModels returns Gemini model definitions for Vertex AI.
func GetGeminiVertexModels() []*ModelInfo {
	return cloneModelInfos(getModels().Vertex)
}

// GetGeminiCLIModels returns Gemini model definitions for the Gemini CLI.
func GetGeminiCLIModels() []*ModelInfo {
	return cloneModelInfos(getModels().GeminiCLI)
}

// GetAIStudioModels returns model definitions for AI Studio.
func GetAIStudioModels() []*ModelInfo {
	return cloneModelInfos(getModels().AIStudio)
}

// GetCodexFreeModels returns model definitions for the Codex free plan tier.
func GetCodexFreeModels() []*ModelInfo {
	return ensureCodexMiniModel(cloneModelInfos(getModels().CodexFree))
}

// GetCodexTeamModels returns model definitions for the Codex team plan tier.
func GetCodexTeamModels() []*ModelInfo {
	return ensureCodexMiniModel(cloneModelInfos(getModels().CodexTeam))
}

// GetCodexPlusModels returns model definitions for the Codex plus plan tier.
func GetCodexPlusModels() []*ModelInfo {
	return ensureCodexMiniModel(cloneModelInfos(getModels().CodexPlus))
}

// GetCodexProModels returns model definitions for the Codex pro plan tier.
func GetCodexProModels() []*ModelInfo {
	return ensureCodexMiniModel(cloneModelInfos(getModels().CodexPro))
}

// GetQwenModels returns the standard Qwen model definitions.
func GetQwenModels() []*ModelInfo {
	return cloneModelInfos(getModels().Qwen)
}

// GetIFlowModels returns the standard iFlow model definitions.
func GetIFlowModels() []*ModelInfo {
	return cloneModelInfos(getModels().IFlow)
}

// GetKimiModels returns the standard Kimi (Moonshot AI) model definitions.
func GetKimiModels() []*ModelInfo {
	return cloneModelInfos(getModels().Kimi)
}

// GetAntigravityModels returns the standard Antigravity model definitions.
func GetAntigravityModels() []*ModelInfo {
	return cloneModelInfos(getModels().Antigravity)
}

// cloneModelInfos returns a shallow copy of the slice with each element deep-cloned.
func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, len(models))
	for i, m := range models {
		out[i] = cloneModelInfo(m)
	}
	return out
}

func ensureCodexMiniModel(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return []*ModelInfo{
			{
				ID:          codexMiniModelID,
				Object:      "model",
				OwnedBy:     "openai",
				Type:        "codex",
				DisplayName: codexMiniModelID,
				Thinking:    &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
			},
		}
	}
	if hasModelID(models, codexMiniModelID) {
		return models
	}
	for _, model := range models {
		if model == nil || model.ID != codexPrimaryModelID {
			continue
		}
		mini := cloneModelInfo(model)
		if mini == nil {
			break
		}
		mini.ID = codexMiniModelID
		mini.DisplayName = codexMiniModelID
		return append(models, mini)
	}
	return append(models, &ModelInfo{
		ID:          codexMiniModelID,
		Object:      "model",
		OwnedBy:     "openai",
		Type:        "codex",
		DisplayName: codexMiniModelID,
		Thinking:    &ThinkingSupport{Levels: []string{"low", "medium", "high"}},
	})
}

func hasModelID(models []*ModelInfo, target string) bool {
	for _, model := range models {
		if model != nil && model.ID == target {
			return true
		}
	}
	return false
}

// GetStaticModelDefinitionsByChannel returns static model definitions for a given channel/provider.
// It returns nil when the channel is unknown.
//
// Supported channels:
//   - claude
//   - gemini
//   - vertex
//   - gemini-cli
//   - aistudio
//   - codex
//   - qwen
//   - iflow
//   - kimi
//   - antigravity
func GetStaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	key := strings.ToLower(strings.TrimSpace(channel))
	switch key {
	case "claude":
		return GetClaudeModels()
	case "gemini":
		return GetGeminiModels()
	case "vertex":
		return GetGeminiVertexModels()
	case "gemini-cli":
		return GetGeminiCLIModels()
	case "aistudio":
		return GetAIStudioModels()
	case "codex":
		return GetCodexProModels()
	case "qwen":
		return GetQwenModels()
	case "iflow":
		return GetIFlowModels()
	case "kimi":
		return GetKimiModels()
	case "antigravity":
		return GetAntigravityModels()
	default:
		return nil
	}
}

// LookupStaticModelInfo searches all static model definitions for a model by ID.
// Returns nil if no matching model is found.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	if modelID == "" {
		return nil
	}

	data := getModels()
	allModels := [][]*ModelInfo{
		data.Claude,
		data.Gemini,
		data.Vertex,
		data.GeminiCLI,
		data.AIStudio,
		data.CodexPro,
		data.Qwen,
		data.IFlow,
		data.Kimi,
		data.Antigravity,
	}
	for _, models := range allModels {
		for _, m := range models {
			if m != nil && m.ID == modelID {
				return cloneModelInfo(m)
			}
		}
	}

	return nil
}
