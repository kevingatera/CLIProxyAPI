package helps

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// cursorEnvAllowlist is the only host environment a cursor-agent child inherits.
// Other providers' keys stay out of the CLI. The API key is added per invocation.
var cursorEnvAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

const (
	cursorStderrLimit = 16 << 10
	cursorMessageMax  = 512
)

// CursorStatusError is an error carrying the HTTP status to report for a failed cursor-agent run.
type CursorStatusError struct {
	Code int
	Msg  string
}

func (e CursorStatusError) Error() string   { return e.Msg }
func (e CursorStatusError) StatusCode() int { return e.Code }

// CursorRun describes one print-mode invocation of cursor-agent.
type CursorRun struct {
	Model string
	// Prompt is written to the child's stdin. It is never placed in argv.
	Prompt string
	// APIKey is passed only to this child's environment. Empty means cursor-agent's own login.
	APIKey string
	// Workspace is an empty per-invocation directory used as cwd and --workspace.
	// The caller creates and removes it.
	Workspace string
}

// CursorResult is the final outcome of a completed invocation.
type CursorResult struct {
	Text  string
	Usage usage.Detail
}

// CursorAgentEnv returns the full environment for one cursor-agent child process.
// It is built from an allowlist rather than os.Environ so that no other credential leaks in.
func CursorAgentEnv(apiKey string) []string {
	env := make([]string, 0, len(cursorEnvAllowlist)+2)
	for _, key := range cursorEnvAllowlist {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, "NO_COLOR=1")
	if apiKey != "" {
		env = append(env, sdkauth.CursorAPIKeyEnv+"="+apiKey)
	}
	return env
}

// RunCursorAgent runs one print-mode turn and calls onText for each assistant text
// segment as the CLI emits it. A nil onText collects the text without streaming.
// The returned error is a CursorStatusError for CLI failures, or the error from onText.
func RunCursorAgent(ctx context.Context, run CursorRun, onText func(text string) error) (CursorResult, error) {
	bin, err := sdkauth.ResolveCursorAgentPath()
	if err != nil {
		return CursorResult{}, CursorStatusError{Code: http.StatusInternalServerError, Msg: err.Error()}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// With no positional prompt, cursor-agent reads a non-TTY stdin as the prompt.
	cmd := exec.CommandContext(runCtx, bin,
		"--print",
		"--output-format", "stream-json",
		"--mode", "ask",
		"--trust",
		"--workspace", run.Workspace,
		"--model", run.Model,
	)
	cmd.Dir = run.Workspace
	cmd.Env = CursorAgentEnv(run.APIKey)
	cmd.Stdin = strings.NewReader(run.Prompt)
	stderr := &cappedBuffer{limit: cursorStderrLimit}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return CursorResult{}, CursorStatusError{Code: http.StatusInternalServerError, Msg: fmt.Sprintf("cursor executor: stdout pipe: %v", err)}
	}
	if err = cmd.Start(); err != nil {
		return CursorResult{}, CursorStatusError{Code: http.StatusInternalServerError, Msg: fmt.Sprintf("cursor executor: start cursor-agent: %v", err)}
	}

	var text strings.Builder
	var final *cursorEvent
	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadBytes('\n')
		if event, ok := parseCursorEvent(line); ok {
			switch event.kind {
			case cursorEventAssistant:
				text.WriteString(event.text)
				if onText != nil && event.text != "" {
					if errText := onText(event.text); errText != nil {
						cancel()
						_ = cmd.Wait()
						return CursorResult{}, errText
					}
				}
			case cursorEventResult:
				final = &event
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			cancel()
			_ = cmd.Wait()
			return CursorResult{}, fmt.Errorf("cursor executor: read cursor-agent output: %w", readErr)
		}
	}
	waitErr := cmd.Wait()
	if errCtx := ctx.Err(); errCtx != nil {
		return CursorResult{}, errCtx
	}

	if final != nil && final.isError {
		return CursorResult{}, cursorFailure(final.errorMessage, http.StatusBadGateway)
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return CursorResult{}, cursorFailure(msg, http.StatusBadGateway)
	}
	if final == nil && text.Len() == 0 {
		return CursorResult{}, CursorStatusError{Code: http.StatusBadGateway, Msg: "cursor-agent returned no result"}
	}

	result := CursorResult{Text: text.String()}
	if final != nil {
		if final.text != "" {
			result.Text = final.text
		}
		result.Usage = final.usage
	}
	return result, nil
}

type cursorEventKind int

const (
	cursorEventOther cursorEventKind = iota
	cursorEventAssistant
	cursorEventResult
)

type cursorEvent struct {
	kind         cursorEventKind
	text         string
	isError      bool
	errorMessage string
	usage        usage.Detail
}

// parseCursorEvent decodes one stream-json line. Non-JSON lines are ignored.
func parseCursorEvent(line []byte) (cursorEvent, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' || !gjson.ValidBytes(line) {
		return cursorEvent{}, false
	}
	switch gjson.GetBytes(line, "type").String() {
	case "assistant":
		var b strings.Builder
		gjson.GetBytes(line, "message.content").ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "text" {
				b.WriteString(item.Get("text").String())
			}
			return true
		})
		return cursorEvent{kind: cursorEventAssistant, text: b.String()}, true
	case "result":
		event := cursorEvent{
			kind:    cursorEventResult,
			text:    gjson.GetBytes(line, "result").String(),
			isError: gjson.GetBytes(line, "is_error").Bool(),
			usage:   ParseCursorUsage(line),
		}
		event.errorMessage = strings.TrimSpace(gjson.GetBytes(line, "error.message").String())
		if event.errorMessage == "" {
			event.errorMessage = event.text
		}
		return event, true
	}
	return cursorEvent{}, false
}

func cursorFailure(msg string, fallback int) error {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "cursor-agent returned an error"
	}
	if len(msg) > cursorMessageMax {
		msg = msg[:cursorMessageMax]
	}
	return CursorStatusError{Code: classifyCursorErrorCode(msg, fallback), Msg: msg}
}

func classifyCursorErrorCode(message string, fallback int) int {
	lower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(lower, "not logged in"),
		strings.Contains(lower, "authentication"),
		strings.Contains(lower, "authorization"),
		strings.Contains(lower, "login"):
		return http.StatusUnauthorized
	case strings.Contains(lower, "quota"),
		strings.Contains(lower, "rate limit"),
		strings.Contains(lower, "too many requests"):
		return http.StatusTooManyRequests
	default:
		return fallback
	}
}

// ParseCursorUsage reads the usage object from a cursor-agent result event.
func ParseCursorUsage(payload []byte) usage.Detail {
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	detail := usage.Detail{
		InputTokens:  usageNode.Get("inputTokens").Int(),
		OutputTokens: usageNode.Get("outputTokens").Int(),
		CachedTokens: usageNode.Get("cacheReadTokens").Int(),
	}
	if detail.InputTokens == 0 {
		detail.InputTokens = usageNode.Get("input_tokens").Int()
	}
	if detail.OutputTokens == 0 {
		detail.OutputTokens = usageNode.Get("output_tokens").Int()
	}
	if detail.CachedTokens == 0 {
		detail.CachedTokens = usageNode.Get("cache_read_tokens").Int()
	}
	detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	if total := usageNode.Get("totalTokens").Int(); total > 0 {
		detail.TotalTokens = total
	}
	if total := usageNode.Get("total_tokens").Int(); total > 0 {
		detail.TotalTokens = total
	}
	return detail
}

// cappedBuffer keeps the first limit bytes written and discards the rest.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return n, nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
