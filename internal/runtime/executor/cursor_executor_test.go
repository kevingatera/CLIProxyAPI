package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCursorProviderModelID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "cursor/auto", want: "auto"},
		{in: "auto", want: "auto"},
		{in: "team/cursor/gpt-5.3-codex", want: "gpt-5.3-codex"},
		{in: "  cursor/sonnet-4.6  ", want: "sonnet-4.6"},
	}
	for _, tt := range tests {
		if got := cursorProviderModelID(tt.in); got != tt.want {
			t.Fatalf("cursorProviderModelID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseCursorAgentUsage(t *testing.T) {
	payload := []byte(`{"usage":{"inputTokens":11,"outputTokens":7,"cacheReadTokens":99}}`)
	detail := parseCursorAgentUsage(payload)
	if detail.InputTokens != 11 || detail.OutputTokens != 7 || detail.CachedTokens != 99 || detail.TotalTokens != 18 {
		t.Fatalf("unexpected usage detail: %+v", detail)
	}
}

func TestCursorPromptFromOpenAIPayload(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role":"system","content":"You are concise."},
			{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"text","text":"World"}]},
			{"role":"assistant","content":"Hi there"}
		]
	}`)
	prompt := cursorPromptFromOpenAIPayload(payload)
	if !strings.Contains(prompt, "SYSTEM:\nYou are concise.") {
		t.Fatalf("prompt missing system section: %q", prompt)
	}
	if !strings.Contains(prompt, "USER:\nHello\nWorld") {
		t.Fatalf("prompt missing user section: %q", prompt)
	}
	if !strings.Contains(prompt, "ASSISTANT:\nHi there") {
		t.Fatalf("prompt missing assistant section: %q", prompt)
	}
}

func TestCursorPromptFromOpenAIPayloadWithAttachments(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"What color is this?"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
			]}
		]
	}`)
	prompt, cleanup, err := cursorPromptFromOpenAIPayloadWithAttachments(payload)
	if err != nil {
		t.Fatalf("cursorPromptFromOpenAIPayloadWithAttachments() error: %v", err)
	}
	defer cleanup()
	if strings.Contains(prompt, "data:image/png") {
		t.Fatalf("prompt leaked image data URL: %q", prompt)
	}
	if !strings.Contains(prompt, "[image attached]") {
		t.Fatalf("prompt missing image placeholder: %q", prompt)
	}
	if !strings.Contains(prompt, "Attached image files for this request:") {
		t.Fatalf("prompt missing attachment list: %q", prompt)
	}
	idx := strings.Index(prompt, "- ")
	if idx < 0 {
		t.Fatalf("prompt missing attachment path: %q", prompt)
	}
	path := strings.Fields(prompt[idx+2:])[0]
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("attachment path was not written: %v", err)
	}
}

func TestCursorPayloadHasImage(t *testing.T) {
	payload := []byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`)
	if !cursorPayloadHasImage(payload) {
		t.Fatal("expected responses image payload to be detected")
	}
	if cursorPayloadHasImage([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)) {
		t.Fatal("unexpected image detection for text-only payload")
	}
}

func TestBuildCursorChatCompletionStreamChunks(t *testing.T) {
	chunks, err := buildCursorChatCompletionStreamChunks("chatcmpl-test", 123, "cursor/auto", "ok", parseCursorAgentUsage([]byte(`{"usage":{"inputTokens":1,"outputTokens":2}}`)))
	if err != nil {
		t.Fatalf("buildCursorChatCompletionStreamChunks() error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(chunks))
	}
	if !strings.Contains(string(chunks[0]), `"object":"chat.completion.chunk"`) {
		t.Fatalf("first chunk is not a completion chunk: %q", chunks[0])
	}
	if !strings.Contains(string(chunks[2]), "data: [DONE]") {
		t.Fatalf("missing done chunk: %q", chunks[2])
	}
}

func TestCursorChatCompletionStreamChunksTranslateToResponses(t *testing.T) {
	upstreamChunks, err := buildCursorChatCompletionStreamChunks("chatcmpl-test", 123, "cursor/auto", "ok", parseCursorAgentUsage([]byte(`{"usage":{"inputTokens":1,"outputTokens":2}}`)))
	if err != nil {
		t.Fatalf("buildCursorChatCompletionStreamChunks() error: %v", err)
	}

	from := sdktranslator.FromString("openai-response")
	to := sdktranslator.FromString("openai")
	var param any
	var translated [][]byte
	for _, chunk := range upstreamChunks {
		translated = append(translated, sdktranslator.TranslateStream(context.Background(), to, from, "cursor/auto", nil, []byte(`{"model":"cursor/auto"}`), chunk, &param)...)
	}

	parts := make([]string, len(translated))
	for i, b := range translated {
		parts[i] = string(b)
	}
	stream := strings.Join(parts, "\n")
	if strings.Contains(stream, `"object":"chat.completion.chunk"`) {
		t.Fatalf("chat completion chunk leaked into responses stream: %s", stream)
	}
	if !strings.Contains(stream, "event: response.completed") {
		t.Fatalf("translated stream missing response.completed: %s", stream)
	}
}

func TestCursorCanonicalRequestModel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "cursor/auto", want: "auto"},
		{in: "team/cursor/gpt-5.3-codex", want: "gpt-5.3-codex"},
		{in: "cursor/gpt-5.4(high)", want: "gpt-5.4(high)"},
		{in: "   ", want: "auto"},
	}
	for _, tt := range tests {
		if got := cursorCanonicalRequestModel(tt.in); got != tt.want {
			t.Fatalf("cursorCanonicalRequestModel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShouldFallbackToCursorAgent(t *testing.T) {
	if !shouldFallbackToCursorAgent(statusErr{code: http.StatusBadGateway, msg: "bad gateway"}) {
		t.Fatal("expected gateway status error to fallback")
	}
	if shouldFallbackToCursorAgent(statusErr{code: http.StatusBadRequest, msg: "bad request"}) {
		t.Fatal("unexpected fallback on 400 status")
	}
	if !shouldFallbackToCursorAgent(context.DeadlineExceeded) {
		t.Fatal("expected context deadline exceeded to fallback")
	}
	if !shouldFallbackToCursorAgent(assertErr("dial tcp 127.0.0.1:32124: connect: connection refused")) {
		t.Fatal("expected connection refused to fallback")
	}
}

func TestCursorACPReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exec := NewCursorExecutor(nil)
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{"base_url": srv.URL + "/v1"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !exec.cursorACPReachable(ctx, auth) {
		t.Fatal("expected ACP probe to succeed for live server")
	}

	deadAuth := &cliproxyauth.Auth{
		Attributes: map[string]string{"base_url": "http://127.0.0.1:1/v1"},
	}
	if exec.cursorACPReachable(ctx, deadAuth) {
		t.Fatal("expected ACP probe to fail for unreachable endpoint")
	}
}

func assertErr(msg string) error { return statusWrap{msg: msg} }

type statusWrap struct{ msg string }

func (s statusWrap) Error() string { return s.msg }
