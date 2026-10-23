package helps

import (
	"net/http"
	"strings"
	"testing"
)

func envValue(env []string, key string) (string, int) {
	count := 0
	value := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			count++
			value = strings.TrimPrefix(entry, key+"=")
		}
	}
	return value, count
}

func TestCursorAgentEnvDropsForeignCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai-leak")
	t.Setenv("ANTHROPIC_API_KEY", "sk-anthropic-leak")
	t.Setenv("CURSOR_API_KEY", "process-level-key")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")

	env := CursorAgentEnv("")
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CURSOR_API_KEY"} {
		if _, count := envValue(env, key); count != 0 {
			t.Fatalf("%s leaked into child env without a selected credential", key)
		}
	}
	if got, count := envValue(env, "HTTPS_PROXY"); count != 1 || got != "http://proxy.example:3128" {
		t.Fatalf("HTTPS_PROXY = %q (count %d), want proxy forwarded", got, count)
	}
	if got, _ := envValue(env, "NO_COLOR"); got != "1" {
		t.Fatalf("NO_COLOR = %q, want 1", got)
	}
}

func TestCursorAgentEnvCarriesOnlySelectedKey(t *testing.T) {
	t.Setenv("CURSOR_API_KEY", "process-level-key")
	env := CursorAgentEnv("selected-key")
	got, count := envValue(env, "CURSOR_API_KEY")
	if count != 1 || got != "selected-key" {
		t.Fatalf("CURSOR_API_KEY = %q (count %d), want exactly the selected key", got, count)
	}
}

func TestParseCursorEvent(t *testing.T) {
	ev, ok := parseCursorEvent([]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hel"},{"type":"tool_use"},{"type":"text","text":"lo"}]}}`))
	if !ok || ev.kind != cursorEventAssistant || ev.text != "Hello" {
		t.Fatalf("assistant event = %+v ok=%v, want text Hello", ev, ok)
	}

	ev, ok = parseCursorEvent([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"Hello","usage":{"inputTokens":11,"outputTokens":7,"cacheReadTokens":99}}`))
	if !ok || ev.kind != cursorEventResult || ev.text != "Hello" || ev.isError {
		t.Fatalf("result event = %+v ok=%v", ev, ok)
	}
	if ev.usage.InputTokens != 11 || ev.usage.OutputTokens != 7 || ev.usage.CachedTokens != 99 || ev.usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", ev.usage)
	}

	ev, ok = parseCursorEvent([]byte(`{"type":"result","is_error":true,"result":"Not logged in"}`))
	if !ok || !ev.isError || ev.errorMessage != "Not logged in" {
		t.Fatalf("error result = %+v ok=%v", ev, ok)
	}

	for _, line := range []string{"", "Loading models...", `{"type":"system","subtype":"init"}`, `{"type":"tool_call"`} {
		if _, ok := parseCursorEvent([]byte(line)); ok {
			t.Fatalf("parseCursorEvent(%q) should be ignored", line)
		}
	}
}

func TestCursorFailureClassifiesStatus(t *testing.T) {
	tests := []struct {
		msg  string
		want int
	}{
		{msg: "Not logged in. Run cursor-agent login.", want: http.StatusUnauthorized},
		{msg: "Rate limit exceeded for plan", want: http.StatusTooManyRequests},
		{msg: "upstream exploded", want: http.StatusBadGateway},
	}
	for _, tt := range tests {
		err := cursorFailure(tt.msg, http.StatusBadGateway)
		status, ok := err.(CursorStatusError)
		if !ok || status.StatusCode() != tt.want {
			t.Fatalf("cursorFailure(%q) = %#v, want status %d", tt.msg, err, tt.want)
		}
	}
}

func TestCappedBufferKeepsPrefix(t *testing.T) {
	buf := &cappedBuffer{limit: 4}
	n, err := buf.Write([]byte("abcdef"))
	if err != nil || n != 6 {
		t.Fatalf("Write() = %d, %v; want 6, nil", n, err)
	}
	if buf.String() != "abcd" {
		t.Fatalf("buffer = %q, want abcd", buf.String())
	}
}
