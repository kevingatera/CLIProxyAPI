package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToGeminiSkipsEmptyReasoningThought(t *testing.T) {
	input := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hello"}]},
			{"type": "reasoning", "encrypted_content": "signed", "summary": []},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hi"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToGemini("gemini-3-pro-preview", input, false)
	if hasThoughtPart(out) {
		t.Fatalf("empty reasoning summary should not be replayed as a thought part: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToGeminiSkipsUnsignedClaudeReasoningThought(t *testing.T) {
	input := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hello"}]},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "thinking text"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hi"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToGemini("claude-sonnet-4-6", input, false)
	if hasThoughtPart(out) {
		t.Fatalf("unsigned Claude reasoning should not be replayed as a thought part: %s", string(out))
	}
}

func TestConvertOpenAIResponsesRequestToGeminiReplaysSignedClaudeReasoningThought(t *testing.T) {
	input := []byte(`{
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hello"}]},
			{
				"type": "reasoning",
				"encrypted_content": "signed-thinking-payload",
				"summary": [
					{"type": "summary_text", "text": "first thought"},
					{"type": "summary_text", "text": "second thought"}
				]
			},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hi"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToGemini("claude-sonnet-4-6", input, false)
	thoughts := thoughtParts(out)
	if len(thoughts) != 1 {
		t.Fatalf("expected one signed thought part, got %d: %s", len(thoughts), string(out))
	}
	if got := thoughts[0].Get("text").String(); got != "first thought\n\nsecond thought" {
		t.Fatalf("thought text = %q, want joined summary text; body=%s", got, string(out))
	}
	if got := thoughts[0].Get("thoughtSignature").String(); got != "signed-thinking-payload" {
		t.Fatalf("thoughtSignature = %q, want signed-thinking-payload; body=%s", got, string(out))
	}
}

func hasThoughtPart(raw []byte) bool {
	return len(thoughtParts(raw)) > 0
}

func thoughtParts(raw []byte) []gjson.Result {
	var out []gjson.Result
	gjson.GetBytes(raw, "contents").ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if part.Get("thought").Bool() {
				out = append(out, part)
			}
			return true
		})
		return true
	})
	return out
}
