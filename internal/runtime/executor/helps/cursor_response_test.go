package helps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCursorChatChunkSetsRoleOnlyOnFirstDelta(t *testing.T) {
	first, err := CursorChatChunk("chatcmpl-1", 1, "auto", "Hel", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CursorChatChunk("chatcmpl-1", 1, "auto", "lo", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), `"role":"assistant"`) {
		t.Fatalf("first chunk missing role: %s", first)
	}
	if strings.Contains(string(second), `"role"`) {
		t.Fatalf("later chunk repeats role: %s", second)
	}
}

func TestCursorChatCompletionReportsUsage(t *testing.T) {
	body, err := CursorChatCompletion("chatcmpl-1", 1, "auto", "ok", usage.Detail{InputTokens: 1, OutputTokens: 2})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	usageMap, _ := parsed["usage"].(map[string]any)
	if usageMap["total_tokens"].(float64) != 3 {
		t.Fatalf("usage = %v, want total_tokens 3", usageMap)
	}
}

func TestCursorChatStreamTranslatesToResponses(t *testing.T) {
	from := sdktranslator.FromString("openai-response")
	to := sdktranslator.FromString("openai")
	frames := []string{}
	text, _ := CursorChatChunk("chatcmpl-test", 123, "auto", "ok", true)
	final, _ := CursorChatFinalChunk("chatcmpl-test", 123, "auto", usage.Detail{InputTokens: 1, OutputTokens: 2})
	frames = append(frames, "data: "+string(text), "data: "+string(final), "data: [DONE]")

	var param any
	var translated []string
	for _, frame := range frames {
		for _, chunk := range sdktranslator.TranslateStream(context.Background(), to, from, "auto", nil, []byte(`{"model":"auto"}`), []byte(frame), &param) {
			translated = append(translated, string(chunk))
		}
	}
	stream := strings.Join(translated, "\n")
	if strings.Contains(stream, `"object":"chat.completion.chunk"`) {
		t.Fatalf("chat completion chunk leaked into responses stream: %s", stream)
	}
	if !strings.Contains(stream, "event: response.completed") {
		t.Fatalf("translated stream missing response.completed: %s", stream)
	}
}
