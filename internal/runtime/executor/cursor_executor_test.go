package executor

import (
	"strings"
	"testing"
)

func TestCursorProviderModelID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "cursor/auto", want: "auto"},
		{in: "auto", want: "auto"},
		{in: "team/cursor/gpt-5.3-codex", want: "gpt-5.3-codex"},
		{in: "  cursor/sonnet-4.6  ", want: "sonnet-4.6"},
	}
	for _, tt := range tests {
		if got := cursorProviderModelID(tt.in); got != tt.want {
			t.Fatalf("cursorProviderModelID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseCursorAgentUsage(t *testing.T) {
	payload := []byte(`{"usage":{"inputTokens":11,"outputTokens":7,"cacheReadTokens":99}}`)
	detail := parseCursorAgentUsage(payload)
	if detail.InputTokens != 11 || detail.OutputTokens != 7 || detail.CachedTokens != 99 || detail.TotalTokens != 18 {
		t.Fatalf("unexpected usage detail: %+v", detail)
	}
}

func TestCursorPromptFromOpenAIPayload(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role":"system","content":"You are concise."},
			{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"text","text":"World"}]},
			{"role":"assistant","content":"Hi there"}
		]
	}`)
	prompt := cursorPromptFromOpenAIPayload(payload)
	if !strings.Contains(prompt, "SYSTEM:\nYou are concise.") {
		t.Fatalf("prompt missing system section: %q", prompt)
	}
	if !strings.Contains(prompt, "USER:\nHello\nWorld") {
		t.Fatalf("prompt missing user section: %q", prompt)
	}
	if !strings.Contains(prompt, "ASSISTANT:\nHi there") {
		t.Fatalf("prompt missing assistant section: %q", prompt)
	}
}

func TestBuildCursorChatCompletionStreamChunks(t *testing.T) {
	chunks, err := buildCursorChatCompletionStreamChunks("chatcmpl-test", 123, "cursor/auto", "ok", parseCursorAgentUsage([]byte(`{"usage":{"inputTokens":1,"outputTokens":2}}`)))
	if err != nil {
		t.Fatalf("buildCursorChatCompletionStreamChunks() error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(chunks))
	}
	if !strings.Contains(string(chunks[0]), `"object":"chat.completion.chunk"`) {
		t.Fatalf("first chunk is not a completion chunk: %q", chunks[0])
	}
	if !strings.Contains(string(chunks[2]), "data: [DONE]") {
		t.Fatalf("missing done chunk: %q", chunks[2])
	}
}
