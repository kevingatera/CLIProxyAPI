package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAI_StreamSetsModelFromResponseCreated(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.created","response":{"id":"resp_123","created_at":1700000000,"model":"gpt-5.3-codex"}}`), &param)
	if len(out) != 0 {
		t.Fatalf("expected no output for response.created, got %d chunks", len(out))
	}

	out = ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.Get(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAI_FirstChunkUsesRequestModelName(t *testing.T) {
	ctx := context.Background()
	var param any

	modelName := "gpt-5.3-codex"

	out := ConvertCodexResponseToOpenAI(ctx, modelName, nil, nil, []byte(`data: {"type":"response.output_text.delta","delta":"hello"}`), &param)
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(out))
	}

	gotModel := gjson.Get(out[0], "model").String()
	if gotModel != modelName {
		t.Fatalf("expected model %q, got %q", modelName, gotModel)
	}
}

func TestConvertCodexResponseToOpenAINonStream_PreservesLastNonEmptyMessage(t *testing.T) {
	raw := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"created_at":1700000000,
			"model":"gpt-5.4",
			"status":"completed",
			"output":[
				{
					"type":"message",
					"phase":"commentary",
					"content":[{"type":"output_text","text":"{\"tool\":\"search_entities\"}"}]
				},
				{
					"type":"message",
					"phase":"final_answer",
					"content":[{"type":"output_text","text":""}]
				}
			]
		}
	}`)

	out := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.4", nil, nil, raw, nil)
	if got := gjson.Get(out, "choices.0.message.content").String(); got != `{"tool":"search_entities"}` {
		t.Fatalf("content = %q, want preserved non-empty commentary text", got)
	}
}

func TestConvertCodexResponseToOpenAINonStream_JoinsNonEmptyOutputTextParts(t *testing.T) {
	raw := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"created_at":1700000000,
			"model":"gpt-5.4",
			"status":"completed",
			"output":[
				{
					"type":"message",
					"content":[
						{"type":"output_text","text":""},
						{"type":"output_text","text":"first"},
						{"type":"output_text","text":"second"}
					]
				}
			]
		}
	}`)

	out := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.4", nil, nil, raw, nil)
	if got := gjson.Get(out, "choices.0.message.content").String(); got != "first\nsecond" {
		t.Fatalf("content = %q, want joined non-empty text parts", got)
	}
}

func TestConvertCodexResponseToOpenAINonStream_AcceptsScalarMessageContent(t *testing.T) {
	raw := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"created_at":1700000000,
			"model":"gpt-5.4",
			"status":"completed",
			"output":[
				{
					"type":"message",
					"content":"direct scalar content"
				}
			]
		}
	}`)

	out := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.4", nil, nil, raw, nil)
	if got := gjson.Get(out, "choices.0.message.content").String(); got != "direct scalar content" {
		t.Fatalf("content = %q, want scalar content", got)
	}
}
