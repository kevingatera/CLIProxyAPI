package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/thinking/provider/claude"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/thinking/provider/openai"
	"github.com/tidwall/gjson"
)

func TestApplyThinking_UserDefinedClaudePreservesAdaptiveLevel(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-user-defined-claude-" + t.Name()
	modelID := "custom-claude-4-6"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{ID: modelID, UserDefined: true}})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})

	tests := []struct {
		name  string
		model string
		body  []byte
	}{
		{
			name:  "claude adaptive effort body",
			model: modelID,
			body:  []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`),
		},
		{
			name:  "suffix level",
			model: modelID + "(high)",
			body:  []byte(`{}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := thinking.ApplyThinking(tt.body, tt.model, "openai", "claude", "claude")
			if err != nil {
				t.Fatalf("ApplyThinking() error = %v", err)
			}
			if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
				t.Fatalf("thinking.type = %q, want %q, body=%s", got, "adaptive", string(out))
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != "high" {
				t.Fatalf("output_config.effort = %q, want %q, body=%s", got, "high", string(out))
			}
			if gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
				t.Fatalf("thinking.budget_tokens should be removed, body=%s", string(out))
			}
		})
	}
}

func TestApplyThinking_UserDefinedOpenAIModeNoneStripsReasoningEffort(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-user-defined-openai-none-" + t.Name()
	modelID := "custom-openai-compatible"
	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: modelID, UserDefined: true}})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})

	body := []byte(`{"model":"custom-openai-compatible","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`)
	out, err := thinking.ApplyThinking(body, modelID, "openai", "openai", "openai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be stripped for user-defined OpenAI ModeNone, body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("messages.0.content = %q, want %q, body=%s", got, "hi", string(out))
	}
}

func TestApplyThinking_UserDefinedOpenAILevelSetsReasoningEffort(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-user-defined-openai-level-" + t.Name()
	modelID := "custom-openai-compatible-level"
	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: modelID, UserDefined: true}})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})

	body := []byte(`{"model":"custom-openai-compatible-level","messages":[{"role":"user","content":"hi"}]}`)
	out, err := thinking.ApplyThinking(body, modelID+"(medium)", "openai", "openai", "openai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "medium" {
		t.Fatalf("reasoning_effort = %q, want %q, body=%s", got, "medium", string(out))
	}
}

func TestApplyThinking_UserDefinedReasoningSideChannelModelStripsReasoningEffort(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-user-defined-openai-sidechannel-" + t.Name()
	modelID := "opencode-go/deepseek-v4-pro"
	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{{ID: modelID, UserDefined: true}})
	t.Cleanup(func() {
		reg.UnregisterClient(clientID)
	})

	body := []byte(`{"model":"opencode-go/deepseek-v4-pro","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`)
	out, err := thinking.ApplyThinking(body, modelID, "openai", "openai", "openai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be stripped for reasoning side-channel models, body=%s", string(out))
	}
}

func TestApplyThinking_UnknownReasoningSideChannelModelStripsReasoningEffort(t *testing.T) {
	modelID := "unknown-client/deepseek-v4-pro"
	body := []byte(`{"model":"deepseek-reasoner","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`)
	out, err := thinking.ApplyThinking(body, modelID, "openai", "openai", "unknown-client")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should be stripped for unknown reasoning side-channel models, body=%s", string(out))
	}
}

func TestApplyThinking_OpenAIResponseReasoningSideChannelModelStripsReasoning(t *testing.T) {
	modelID := "opencode-go/deepseek-v4-pro"
	body := []byte(`{"model":"deepseek-reasoner","reasoning":{"effort":"medium"},"input":"hi"}`)
	out, err := thinking.ApplyThinking(body, modelID, "openai-response", "openai-response", "opencode-go")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if gjson.GetBytes(out, "reasoning").Exists() {
		t.Fatalf("reasoning should be stripped for responses-compatible reasoning side-channel models, body=%s", string(out))
	}
}
