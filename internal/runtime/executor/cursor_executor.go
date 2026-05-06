package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// CursorExecutor executes requests through cursor-agent CLI.
type CursorExecutor struct {
	cfg    *config.Config
	compat *OpenAICompatExecutor
}

func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	return &CursorExecutor{
		cfg:    cfg,
		compat: NewOpenAICompatExecutor("cursor", cfg),
	}
}

func (e *CursorExecutor) Identifier() string { return "cursor" }

func (e *CursorExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if auth != nil && auth.Attributes != nil {
		if apiKey := strings.TrimSpace(auth.Attributes["api_key"]); apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}
	return nil
}

func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("cursor executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	model := cursorProviderModelID(thinking.ParseSuffix(req.Model).ModelName)
	if model == "" {
		model = "auto"
	}
	canonicalModel := cursorCanonicalRequestModel(req.Model)

	if e.shouldTryCursorACP(auth) {
		compatReq := req
		compatReq.Model = canonicalModel
		if e.cursorACPReachable(ctx, auth) {
			compatResp, compatErr := e.compat.Execute(ctx, auth, compatReq, opts)
			if compatErr == nil {
				return compatResp, nil
			}
			if !shouldFallbackToCursorAgent(compatErr) {
				return resp, compatErr
			}
		}
	}

	reporter := newUsageReporter(ctx, e.Identifier(), model, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, model, originalPayload, false)
	translated := sdktranslator.TranslateRequest(from, to, model, req.Payload, false)

	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, model, to.String(), "", translated, originalTranslated, requestedModel)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	prompt := cursorPromptFromOpenAIPayload(translated)
	if strings.TrimSpace(prompt) == "" {
		return resp, statusErr{code: http.StatusBadRequest, msg: "cursor executor: empty prompt"}
	}

	result, runErr := runCursorAgent(ctx, model, prompt)
	if runErr != nil {
		return resp, runErr
	}
	reporter.publish(ctx, result.Usage)
	reporter.ensurePublished(ctx)

	assistantText := strings.TrimLeft(result.Content, "\n")
	upstreamBody, buildErr := buildCursorChatCompletionResponse(req.Model, assistantText, result.Usage)
	if buildErr != nil {
		return resp, buildErr
	}

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, upstreamBody, &param)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: headers}, nil
}

func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	model := cursorProviderModelID(thinking.ParseSuffix(req.Model).ModelName)
	if model == "" {
		model = "auto"
	}
	canonicalModel := cursorCanonicalRequestModel(req.Model)

	if e.shouldTryCursorACP(auth) {
		compatReq := req
		compatReq.Model = canonicalModel
		if e.cursorACPReachable(ctx, auth) {
			compatStream, compatErr := e.compat.ExecuteStream(ctx, auth, compatReq, opts)
			if compatErr == nil {
				return compatStream, nil
			}
			if !shouldFallbackToCursorAgent(compatErr) {
				return nil, compatErr
			}
		}
	}

	reporter := newUsageReporter(ctx, e.Identifier(), model, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, model, originalPayload, true)
	translated := sdktranslator.TranslateRequest(from, to, model, req.Payload, true)

	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, model, to.String(), "", translated, originalTranslated, requestedModel)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	prompt := cursorPromptFromOpenAIPayload(translated)
	if strings.TrimSpace(prompt) == "" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "cursor executor: empty prompt"}
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)

		result, runErr := runCursorAgent(ctx, model, prompt)
		if runErr != nil {
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: runErr}
			return
		}
		reporter.publish(ctx, result.Usage)
		reporter.ensurePublished(ctx)

		assistantText := strings.TrimLeft(result.Content, "\n")
		chunkID := "chatcmpl-" + uuid.NewString()
		created := time.Now().Unix()
		upstreamChunks, buildErr := buildCursorChatCompletionStreamChunks(chunkID, created, req.Model, assistantText, result.Usage)
		if buildErr != nil {
			out <- cliproxyexecutor.StreamChunk{Err: buildErr}
			return
		}
		var param any
		for _, payload := range upstreamChunks {
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, payload, &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func (e *CursorExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	model := cursorProviderModelID(thinking.ParseSuffix(req.Model).ModelName)
	if model == "" {
		model = "auto"
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, model, req.Payload, false)

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := tokenizerForModel(model)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor executor: tokenizer init failed: %w", err)
	}
	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor executor: token counting failed: %w", err)
	}
	usageJSON := buildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translatedUsage)}, nil
}

func (e *CursorExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

type cursorRunResult struct {
	Content string
	Usage   usage.Detail
}

func runCursorAgent(ctx context.Context, model, prompt string) (cursorRunResult, error) {
	bin, err := resolveCursorAgentPath()
	if err != nil {
		return cursorRunResult{}, statusErr{code: http.StatusInternalServerError, msg: err.Error()}
	}
	args := []string{
		"--print",
		"--output-format", "json",
		"--mode", "ask",
		"--trust",
		"--model", model,
		prompt,
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = runErr.Error()
		}
		code := classifyCursorErrorCode(msg, http.StatusBadGateway)
		return cursorRunResult{}, statusErr{code: code, msg: msg}
	}
	payload := extractLastValidJSONObjectLine(stdout.Bytes())
	if len(payload) == 0 {
		msg := strings.TrimSpace(stdout.String())
		if msg == "" {
			msg = "cursor-agent returned no JSON output"
		}
		return cursorRunResult{}, statusErr{code: http.StatusBadGateway, msg: msg}
	}

	if gjson.GetBytes(payload, "is_error").Bool() {
		msg := strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
		if msg == "" {
			msg = strings.TrimSpace(gjson.GetBytes(payload, "result").String())
		}
		if msg == "" {
			msg = "cursor-agent returned error"
		}
		code := classifyCursorErrorCode(msg, http.StatusBadGateway)
		return cursorRunResult{}, statusErr{code: code, msg: msg}
	}

	content := gjson.GetBytes(payload, "result").String()
	if content == "" {
		content = gjson.GetBytes(payload, "message.content").String()
	}
	return cursorRunResult{
		Content: content,
		Usage:   parseCursorAgentUsage(payload),
	}, nil
}

func resolveCursorAgentPath() (string, error) {
	if path, err := exec.LookPath("cursor-agent"); err == nil && strings.TrimSpace(path) != "" {
		return path, nil
	}
	for _, candidate := range []string{
		"/usr/local/bin/cursor-agent",
		"/root/.local/bin/cursor-agent",
		"/root/.local/bin/agent",
	} {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cursor-agent binary not found")
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

func parseCursorAgentUsage(payload []byte) usage.Detail {
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

func extractLastValidJSONObjectLine(raw []byte) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(nil, 2_097_152)
	var last []byte
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		if gjson.ValidBytes(line) {
			last = append(last[:0], line...)
		}
	}
	if len(last) == 0 {
		return nil
	}
	return append([]byte(nil), last...)
}

func cursorProviderModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(model), "cursor/") {
		model = strings.TrimSpace(model[len("cursor/"):])
	}
	if strings.Contains(model, "/") {
		parts := strings.Split(model, "/")
		model = strings.TrimSpace(parts[len(parts)-1])
	}
	return model
}

func cursorPromptFromOpenAIPayload(payload []byte) string {
	root := gjson.ParseBytes(payload)
	segments := make([]string, 0, 16)

	messages := root.Get("messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, message gjson.Result) bool {
			role := strings.TrimSpace(message.Get("role").String())
			if role == "" {
				role = "user"
			}
			text := cursorExtractMessageContent(message)
			if text == "" {
				return true
			}
			segments = append(segments, strings.ToUpper(role)+":\n"+text)
			return true
		})
	}
	if len(segments) == 0 {
		fallback := make([]string, 0, 8)
		collectOpenAIMessages(root.Get("messages"), &fallback)
		collectOpenAITools(root.Get("tools"), &fallback)
		collectOpenAIFunctions(root.Get("functions"), &fallback)
		collectOpenAIToolChoice(root.Get("tool_choice"), &fallback)
		collectOpenAIResponseFormat(root.Get("response_format"), &fallback)
		addIfNotEmpty(&fallback, root.Get("input").String())
		addIfNotEmpty(&fallback, root.Get("prompt").String())
		return strings.TrimSpace(strings.Join(fallback, "\n"))
	}
	return strings.TrimSpace(strings.Join(segments, "\n\n"))
}

func cursorExtractMessageContent(message gjson.Result) string {
	content := cursorFlattenContent(message.Get("content"))
	if content == "" {
		return ""
	}
	name := strings.TrimSpace(message.Get("name").String())
	if name == "" {
		return content
	}
	return fmt.Sprintf("name=%s\n%s", name, content)
}

func cursorFlattenContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	switch content.Type {
	case gjson.String:
		return strings.TrimSpace(content.String())
	case gjson.JSON:
		if content.IsArray() {
			parts := make([]string, 0, len(content.Array()))
			content.ForEach(func(_, item gjson.Result) bool {
				flat := cursorFlattenContent(item)
				if flat != "" {
					parts = append(parts, flat)
				}
				return true
			})
			return strings.TrimSpace(strings.Join(parts, "\n"))
		}
		if text := strings.TrimSpace(content.Get("text").String()); text != "" {
			return text
		}
		if content.Get("type").String() == "image_url" {
			if url := strings.TrimSpace(content.Get("image_url.url").String()); url != "" {
				return "[image] " + url
			}
		}
		return strings.TrimSpace(content.Raw)
	default:
		return strings.TrimSpace(content.String())
	}
}

func buildCursorChatCompletionResponse(model, content string, detail usage.Detail) ([]byte, error) {
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	usageMap := map[string]any{
		"prompt_tokens":     detail.InputTokens,
		"completion_tokens": detail.OutputTokens,
		"total_tokens":      cursorTotalTokens(detail),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": detail.CachedTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": detail.ReasoningTokens,
		},
	}
	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": usageMap,
	}
	return json.Marshal(response)
}

func buildCursorChatCompletionStreamChunks(id string, created int64, model, content string, detail usage.Detail) ([][]byte, error) {
	usageMap := map[string]any{
		"prompt_tokens":     detail.InputTokens,
		"completion_tokens": detail.OutputTokens,
		"total_tokens":      cursorTotalTokens(detail),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": detail.CachedTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": detail.ReasoningTokens,
		},
	}
	first := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": nil,
			},
		},
	}
	second := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
		"usage": usageMap,
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		return nil, err
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		return nil, err
	}
	return [][]byte{
		[]byte("data: " + string(firstJSON) + "\n\n"),
		[]byte("data: " + string(secondJSON) + "\n\n"),
		[]byte("data: [DONE]\n\n"),
	}, nil
}

func cursorTotalTokens(detail usage.Detail) int64 {
	if detail.TotalTokens > 0 {
		return detail.TotalTokens
	}
	return detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
}

func cursorCanonicalRequestModel(model string) string {
	parsed := thinking.ParseSuffix(strings.TrimSpace(model))
	base := cursorProviderModelID(parsed.ModelName)
	if base == "" {
		base = "auto"
	}
	if parsed.HasSuffix && strings.TrimSpace(parsed.RawSuffix) != "" {
		return base + "(" + strings.TrimSpace(parsed.RawSuffix) + ")"
	}
	return base
}

func (e *CursorExecutor) shouldTryCursorACP(auth *cliproxyauth.Auth) bool {
	if e == nil || e.compat == nil {
		return false
	}
	return cursorACPBaseURL(auth) != ""
}

func cursorACPBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["base_url"])
}

func (e *CursorExecutor) cursorACPReachable(ctx context.Context, auth *cliproxyauth.Auth) bool {
	baseURL := cursorACPBaseURL(auth)
	if baseURL == "" {
		return false
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || strings.TrimSpace(parsed.Host) == "" {
		return false
	}
	hostPort := strings.TrimSpace(parsed.Host)
	if !strings.Contains(hostPort, ":") {
		switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
		case "https":
			hostPort += ":443"
		default:
			hostPort += ":80"
		}
	}
	timeout := 150 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(probeCtx, "tcp", hostPort)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func shouldFallbackToCursorAgent(err error) bool {
	if err == nil {
		return false
	}
	var status statusErr
	if errors.As(err, &status) {
		switch status.StatusCode() {
		case http.StatusUnauthorized,
			http.StatusForbidden,
			http.StatusNotFound,
			http.StatusRequestTimeout,
			http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(err.Error()))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "connect: cannot assign requested address") ||
		strings.Contains(lower, "no such host") ||
		strings.Contains(lower, "network is unreachable") ||
		strings.Contains(lower, "context deadline exceeded") ||
		strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "eof")
}
