package executor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const fakeCursorAgentArgsAndEnv = `
printf '%s\n' "$@" > "{{OUT}}/argv.txt"
cat > "{{OUT}}/stdin.txt"
env > "{{OUT}}/env.txt"
cat <<'EOF'
{"type":"system","subtype":"init"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Hello","usage":{"inputTokens":3,"outputTokens":2}}
EOF
`

// fakeCursorAgentStreamGate emits a first segment, then waits for the test to create
// "release". Without real streaming the first segment cannot reach the test before the gate.
const fakeCursorAgentStreamGate = `
cat > "{{OUT}}/stdin.txt"
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hel"}]}}'
i=0
while [ ! -f "{{OUT}}/release" ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
if [ ! -f "{{OUT}}/release" ]; then touch "{{OUT}}/timed_out"; fi
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"lo"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"Hello","usage":{"inputTokens":3,"outputTokens":2}}'
`

// installFakeCursorAgent puts a cursor-agent stub first on PATH. The stub's body
// runs with {{OUT}} replaced by a fresh directory, which is returned.
func installFakeCursorAgent(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub requires a POSIX shell")
	}
	binDir := t.TempDir()
	outDir := t.TempDir()
	script := "#!/bin/sh\n" + strings.ReplaceAll(body, "{{OUT}}", outDir)
	if err := os.WriteFile(filepath.Join(binDir, "cursor-agent"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return outDir
}

func readStubFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func envHasKey(envDump, key string) bool {
	for _, line := range strings.Split(envDump, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return true
		}
	}
	return false
}

func cursorTestRequest() (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	req := cliproxyexecutor.Request{
		Model:   "cursor/auto",
		Payload: []byte(`{"model":"cursor/auto","messages":[{"role":"user","content":"Explain the proxy"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}
	return req, opts
}

func legacyCursorAuth() *cliproxyauth.Auth {
	// Shape of auth files written by earlier builds: no secret, base_url present but unused.
	return &cliproxyauth.Auth{
		Provider: "cursor",
		Metadata: map[string]any{
			"type":         "cursor",
			"provider_key": "cursor",
			"compat_name":  "cursor",
			"base_url":     "http://127.0.0.1:32124/v1",
			"models":       []any{"auto"},
			"label":        "kevin",
			"timestamp":    float64(1765000000000),
		},
	}
}

func TestCursorExecutorExecuteSendsPromptOnStdinAndScopesKey(t *testing.T) {
	out := installFakeCursorAgent(t, fakeCursorAgentArgsAndEnv)
	t.Setenv("CURSOR_API_KEY", "sk-selected-key")
	t.Setenv("OPENAI_API_KEY", "sk-unrelated-openai")

	auth := legacyCursorAuth()
	auth.Metadata["api_key_env"] = "CURSOR_API_KEY"
	req, opts := cursorTestRequest()

	resp, err := NewCursorExecutor(&config.Config{}).Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !strings.Contains(string(resp.Payload), "Hello") {
		t.Fatalf("response missing assistant text: %s", resp.Payload)
	}

	argv := readStubFile(t, out, "argv.txt")
	if strings.Contains(argv, "Explain the proxy") {
		t.Fatalf("prompt leaked into argv: %s", argv)
	}
	if !strings.Contains(readStubFile(t, out, "stdin.txt"), "USER:\nExplain the proxy") {
		t.Fatal("prompt was not delivered on stdin")
	}

	envDump := readStubFile(t, out, "env.txt")
	if !strings.Contains(envDump, "CURSOR_API_KEY=sk-selected-key") {
		t.Fatal("selected credential was not passed to the child")
	}
	if envHasKey(envDump, "OPENAI_API_KEY") || strings.Contains(envDump, "sk-unrelated-openai") {
		t.Fatal("unrelated provider key leaked into the cursor-agent environment")
	}
}

func TestCursorExecutorWithoutReferenceSendsNoKey(t *testing.T) {
	out := installFakeCursorAgent(t, fakeCursorAgentArgsAndEnv)
	t.Setenv("CURSOR_API_KEY", "sk-process-key")

	req, opts := cursorTestRequest()
	if _, err := NewCursorExecutor(&config.Config{}).Execute(context.Background(), legacyCursorAuth(), req, opts); err != nil {
		t.Fatalf("Execute() with legacy auth error: %v", err)
	}
	if envHasKey(readStubFile(t, out, "env.txt"), "CURSOR_API_KEY") {
		t.Fatal("process-level CURSOR_API_KEY was forwarded without an auth reference")
	}
}

func TestCursorExecutorMapsCLIErrorToStatus(t *testing.T) {
	installFakeCursorAgent(t, `
cat > /dev/null
echo '{"type":"result","subtype":"error","is_error":true,"result":"Not logged in. Run cursor-agent login."}'
`)
	req, opts := cursorTestRequest()
	_, err := NewCursorExecutor(&config.Config{}).Execute(context.Background(), legacyCursorAuth(), req, opts)
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("Execute() error = %#v, want 401 status", err)
	}
}

func TestCursorExecutorExecuteStreamForwardsSegmentsAsTheyArrive(t *testing.T) {
	out := installFakeCursorAgent(t, fakeCursorAgentStreamGate)
	req, opts := cursorTestRequest()

	stream, err := NewCursorExecutor(&config.Config{}).ExecuteStream(context.Background(), legacyCursorAuth(), req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error: %v", err)
	}

	var first []byte
	select {
	case chunk, ok := <-stream.Chunks:
		if !ok || chunk.Err != nil {
			t.Fatalf("first chunk = %+v ok=%v", chunk, ok)
		}
		first = chunk.Payload
	case <-time.After(20 * time.Second):
		t.Fatal("no chunk arrived before the CLI finished: output is not streamed")
	}
	if !strings.Contains(string(first), "Hel") {
		t.Fatalf("first chunk = %s, want the first segment", first)
	}
	if err := os.WriteFile(filepath.Join(out, "release"), nil, 0o600); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	var all strings.Builder
	all.Write(first)
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		all.Write(chunk.Payload)
	}
	if _, err := os.Stat(filepath.Join(out, "timed_out")); err == nil {
		t.Fatal("stub gave up waiting: the first segment was not delivered before completion")
	}
	text := all.String()
	if !strings.Contains(text, "lo") || !strings.Contains(text, "finish_reason") {
		t.Fatalf("stream missing later segment or finish: %s", text)
	}
}

func TestCursorExecutorPayloadRulesApplyAfterBuiltIns(t *testing.T) {
	executor := NewCursorExecutor(&config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "auto", Protocol: "openai"}},
		Params: map[string]any{"reasoning_effort": "low"},
	}}}})
	req := cliproxyexecutor.Request{
		Model:   "cursor/auto(high)",
		Payload: []byte(`{"model":"cursor/auto","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}

	payload, err := executor.translatePayload(req, opts, "auto", false)
	if err != nil {
		t.Fatalf("translatePayload() error: %v", err)
	}
	if got := gjson.GetBytes(payload, "reasoning_effort").String(); got != "low" {
		t.Fatalf("reasoning_effort = %q, want the payload override to win; payload=%s", got, payload)
	}
}
