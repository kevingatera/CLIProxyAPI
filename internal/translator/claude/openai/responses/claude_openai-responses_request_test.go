package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeResponsesRequestSkipsNonFunctionAndEmptyNameTools(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"tools": [
			{"type": "custom", "name": "apply_patch"},
			{"type": "web_search"},
			{"type": "function", "name": ""},
			{"type": "function", "name": "exec_command", "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("minimax-test", input, false)
	tools := gjson.GetBytes(out, "tools")
	if got := len(tools.Array()); got != 1 {
		t.Fatalf("tools length = %d, want 1; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "exec_command" {
		t.Fatalf("tool name = %q, want exec_command; body=%s", got, string(out))
	}
}

func TestClaudeResponsesRequestDefaultsEmptyToolSchemaToObjectSchema(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"tools": [
			{"type": "function", "name": "exec_command", "parameters": {}}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("minimax-test", input, false)
	if got := gjson.GetBytes(out, "tools.0.input_schema.type").String(); got != "object" {
		t.Fatalf("input_schema.type = %q, want object; body=%s", got, string(out))
	}
	if !gjson.GetBytes(out, "tools.0.input_schema.properties").Exists() {
		t.Fatalf("input_schema.properties missing; body=%s", string(out))
	}
}

func TestClaudeResponsesRequestCompletesPartialToolSchema(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"tools": [
			{"type": "function", "name": "exec_command", "parameters": {"required": ["cmd"]}}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("minimax-test", input, false)
	if got := gjson.GetBytes(out, "tools.0.input_schema.type").String(); got != "object" {
		t.Fatalf("input_schema.type = %q, want object; body=%s", got, string(out))
	}
	if !gjson.GetBytes(out, "tools.0.input_schema.properties").Exists() {
		t.Fatalf("input_schema.properties missing; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.input_schema.required.0").String(); got != "cmd" {
		t.Fatalf("input_schema.required[0] = %q, want cmd; body=%s", got, string(out))
	}
}
