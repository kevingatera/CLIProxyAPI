package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeResponseToOpenAINonStream_PlainMessageJSON(t *testing.T) {
	raw := []byte(`{
		"id":"msg_test",
		"type":"message",
		"role":"assistant",
		"content":[{"type":"text","text":"ok"}],
		"model":"deepseek-v4-flash",
		"stop_reason":"end_turn",
		"usage":{"input_tokens":88,"output_tokens":20}
	}`)

	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "deepseek-v4-flash", nil, nil, raw, nil)
	result := gjson.Parse(out)

	if got := result.Get("id").String(); got != "msg_test" {
		t.Fatalf("id = %q, want %q", got, "msg_test")
	}
	if got := result.Get("model").String(); got != "deepseek-v4-flash" {
		t.Fatalf("model = %q, want %q", got, "deepseek-v4-flash")
	}
	if got := result.Get("choices.0.message.content").String(); got != "ok" {
		t.Fatalf("content = %q, want %q", got, "ok")
	}
	if got := result.Get("choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q, want %q", got, "stop")
	}
	if got := result.Get("usage.prompt_tokens").Int(); got != 88 {
		t.Fatalf("prompt_tokens = %d, want 88", got)
	}
	if got := result.Get("usage.completion_tokens").Int(); got != 20 {
		t.Fatalf("completion_tokens = %d, want 20", got)
	}
	if got := result.Get("usage.total_tokens").Int(); got != 108 {
		t.Fatalf("total_tokens = %d, want 108", got)
	}
}
