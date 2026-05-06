package responses

import (
	"context"
	"strings"
	"testing"
)

func TestClaudeResponseToOpenAIResponsesDoneEventsKeepText(t *testing.T) {
	lines := []string{
		`data: {"type":"message_start","message":{"id":"msg-test","type":"message","role":"assistant","content":[],"model":"MiniMax-M2.7","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"checking"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"\n\ngreen"}}`,
		`data: {"type":"content_block_stop","index":1}`,
	}

	var param any
	var out []string
	for _, line := range lines {
		out = append(out, ConvertClaudeResponseToOpenAIResponses(context.Background(), "opencode-go/minimax-m2.5", nil, nil, []byte(line), &param)...)
	}
	stream := strings.Join(out, "\n")
	for _, want := range []string{
		`"type":"response.output_text.delta"`,
		`"delta":"\n\ngreen"`,
		`"type":"response.output_text.done"`,
		`"text":"\n\ngreen"`,
		`"type":"response.content_part.done"`,
		`"type":"response.output_item.done"`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("translated stream missing %q:\n%s", want, stream)
		}
	}
}
