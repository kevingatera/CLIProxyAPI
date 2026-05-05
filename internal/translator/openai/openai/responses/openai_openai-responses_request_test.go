package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesRequestSkipsToolsWithEmptyNames(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"tools": [
			{"type": "function", "name": "", "description": "invalid"},
			{"type": "function", "description": "missing"},
			{"type": "function", "name": "search", "description": "valid"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("minimax-test", input, false)
	tools := gjson.GetBytes(out, "tools")
	if got := len(tools.Array()); got != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "search" {
		t.Fatalf("tool name = %q, want search; body=%s", got, string(out))
	}
}

func TestResponsesRequestDefaultsMissingToolParametersToObjectSchema(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"tools": [
			{"type": "function", "name": "search", "description": "valid"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("minimax-test", input, false)
	if got := gjson.GetBytes(out, "tools.0.function.parameters.type").String(); got != "object" {
		t.Fatalf("parameters.type = %q, want object; body=%s", got, string(out))
	}
	if !gjson.GetBytes(out, "tools.0.function.parameters.properties").Exists() {
		t.Fatalf("parameters.properties missing; body=%s", string(out))
	}
}
