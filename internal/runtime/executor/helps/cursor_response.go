package helps

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// CursorChatCompletion builds a non-streaming OpenAI chat completion body.
func CursorChatCompletion(id string, created int64, model, content string, detail usage.Detail) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			},
		},
		"usage": cursorUsageMap(detail),
	})
}

// CursorChatChunk builds one streaming content delta. The role is set only on the first delta.
func CursorChatChunk(id string, created int64, model, content string, withRole bool) ([]byte, error) {
	delta := map[string]any{"content": content}
	if withRole {
		delta["role"] = "assistant"
	}
	return json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{"index": 0, "delta": delta, "finish_reason": nil},
		},
	})
}

// CursorChatFinalChunk builds the terminal streaming chunk that carries finish_reason and usage.
func CursorChatFinalChunk(id string, created int64, model string, detail usage.Detail) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
		},
		"usage": cursorUsageMap(detail),
	})
}

func cursorUsageMap(detail usage.Detail) map[string]any {
	return map[string]any{
		"prompt_tokens":     detail.InputTokens,
		"completion_tokens": detail.OutputTokens,
		"total_tokens":      CursorTotalTokens(detail),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": detail.CachedTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": detail.ReasoningTokens,
		},
	}
}

// CursorTotalTokens returns the reported total, or the sum of its parts when none is reported.
func CursorTotalTokens(detail usage.Detail) int64 {
	if detail.TotalTokens > 0 {
		return detail.TotalTokens
	}
	return detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
}
