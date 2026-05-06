package responses

import (
	"strings"
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

func TestResponsesRequestDefaultsEmptyToolParametersToObjectSchema(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"tools": [
			{"type": "function", "name": "search", "description": "valid", "parameters": {}}
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

func TestResponsesRequestCompletesPartialToolParameterSchema(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"tools": [
			{"type": "function", "name": "search", "description": "valid", "parameters": {"required": ["query"]}}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("minimax-test", input, false)
	if got := gjson.GetBytes(out, "tools.0.function.parameters.type").String(); got != "object" {
		t.Fatalf("parameters.type = %q, want object; body=%s", got, string(out))
	}
	if !gjson.GetBytes(out, "tools.0.function.parameters.properties").Exists() {
		t.Fatalf("parameters.properties missing; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.required.0").String(); got != "query" {
		t.Fatalf("parameters.required[0] = %q, want query; body=%s", got, string(out))
	}
}

func TestResponsesRequestMapsTextFormatToResponseFormat(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"text": {
			"format": {
				"type": "json_schema",
				"name": "answer_schema",
				"strict": true,
				"schema": {
					"type": "object",
					"properties": {"answer": {"type": "string"}},
					"required": ["answer"],
					"additionalProperties": false
				}
			}
		}
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("minimax-test", input, false)
	if got := gjson.GetBytes(out, "response_format.type").String(); got != "json_schema" {
		t.Fatalf("response_format.type = %q, want json_schema; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "response_format.json_schema.name").String(); got != "answer_schema" {
		t.Fatalf("response_format.json_schema.name = %q, want answer_schema; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "response_format.json_schema.schema.required.0").String(); got != "answer" {
		t.Fatalf("response_format schema missing required field; body=%s", string(out))
	}
}

func TestResponsesRequestMapsResponsesToolChoiceObjectToChatToolChoice(t *testing.T) {
	input := []byte(`{
		"input": "hello",
		"tools": [
			{"type": "function", "name": "get_magic_number"}
		],
		"tool_choice": {"type": "function", "name": "get_magic_number"}
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("minimax-test", input, false)
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hello" {
		t.Fatalf("messages.0.content = %q, want hello; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "function" {
		t.Fatalf("tool_choice.type = %q, want function; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice.function.name").String(); got != "get_magic_number" {
		t.Fatalf("tool_choice.function.name = %q, want get_magic_number; body=%s", got, string(out))
	}
}

func TestResponsesRequestTextualizesDeepSeekV4ToolTurns(t *testing.T) {
	input := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"file.txt"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("opencode-go/deepseek-v4-pro", input, false)
	if gjson.GetBytes(out, "messages.0.tool_calls").Exists() {
		t.Fatalf("tool_calls should be textualized for DeepSeek v4; body=%s", string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want assistant; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user; body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.content").String(); !strings.Contains(got, "file.txt") {
		t.Fatalf("tool output content = %q, want file.txt; body=%s", got, string(out))
	}
}
