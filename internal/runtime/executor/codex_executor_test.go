package executor

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexStreamTerminalErrorResponseFailed(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"code":"model_not_found","message":"The model ` + "`gpt-5.5`" + ` does not exist or you do not have access to it."}}}`)

	err, ok := codexStreamTerminalError(raw)
	if !ok {
		t.Fatalf("expected terminal error")
	}
	if err.StatusCode() != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", err.StatusCode(), http.StatusNotFound)
	}
	if err.Error() != "The model `gpt-5.5` does not exist or you do not have access to it." {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestCodexStreamTerminalErrorEvent(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"code":"usage_limit_reached","message":"quota exhausted"}}`)

	err, ok := codexStreamTerminalError(raw)
	if !ok {
		t.Fatalf("expected terminal error")
	}
	if err.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", err.StatusCode(), http.StatusTooManyRequests)
	}
	if err.Error() != "quota exhausted" {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestHydrateCodexCompletedOutputUsesOutputItemDone(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)
	item := `{"id":"msg_1","type":"message","status":"completed","content":[{"type":"output_text","text":"ok"}],"role":"assistant"}`

	out := hydrateCodexCompletedOutput(completed, []string{item})

	text := gjson.GetBytes(out, "response.output.0.content.0.text").String()
	if text != "ok" {
		t.Fatalf("hydrated text = %q, want ok; payload=%s", text, string(out))
	}
}

func TestHydrateCodexCompletedOutputPreservesExistingOutput(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"existing"}]}]}}`)
	item := `{"type":"message","content":[{"type":"output_text","text":"replacement"}]}`

	out := hydrateCodexCompletedOutput(completed, []string{item})

	text := gjson.GetBytes(out, "response.output.0.content.0.text").String()
	if text != "existing" {
		t.Fatalf("hydrated text = %q, want existing; payload=%s", text, string(out))
	}
}
