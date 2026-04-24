package executor

import (
	"net/http"
	"testing"
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
