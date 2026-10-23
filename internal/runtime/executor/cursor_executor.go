package executor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// CursorExecutor runs chat requests through the cursor-agent CLI in print mode.
type CursorExecutor struct {
	cfg *config.Config
}

func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	return &CursorExecutor{cfg: cfg}
}

func (e *CursorExecutor) Identifier() string { return "cursor" }

// HttpRequest is unsupported: cursor auth has no HTTP endpoint to call.
func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, statusErr{code: http.StatusNotImplemented, msg: "cursor executor: direct HTTP requests are not supported"}
}

// cursorTurn is one prepared invocation shared by Execute, ExecuteStream, and CountTokens.
type cursorTurn struct {
	model      string
	apiKey     string
	prompt     string
	workspace  string
	translated []byte
}

func (t *cursorTurn) run() helps.CursorRun {
	return helps.CursorRun{Model: t.model, Prompt: t.prompt, APIKey: t.apiKey, Workspace: t.workspace}
}

// close removes the per-invocation workspace, including any image attachments.
func (t *cursorTurn) close() {
	_ = os.RemoveAll(t.workspace)
}

func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, cursorModelFromRequest(req.Model), auth)
	defer reporter.TrackFailure(ctx, &err)

	turn, err := e.prepare(auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	defer turn.close()

	result, err := helps.RunCursorAgent(ctx, turn.run(), nil)
	if err != nil {
		return resp, err
	}
	reporter.Publish(ctx, result.Usage)

	body, err := helps.CursorChatCompletion(newCursorChatID(), time.Now().Unix(), req.Model, strings.TrimLeft(result.Text, "\n"), result.Usage)
	if err != nil {
		return resp, err
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, turn.translated, body, &param)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: headers}, nil
}

func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, cursorModelFromRequest(req.Model), auth)
	defer reporter.TrackFailure(ctx, &err)

	turn, err := e.prepare(auth, req, opts, true)
	if err != nil {
		return nil, err
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer turn.close()

		from := opts.SourceFormat
		to := sdktranslator.FromString("openai")
		var param any
		chunkID := newCursorChatID()
		created := time.Now().Unix()
		sentRole := false

		// forward translates one OpenAI SSE frame and sends the result downstream.
		forward := func(frame []byte) error {
			for _, chunk := range sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, turn.translated, frame, &param) {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}

		result, runErr := helps.RunCursorAgent(ctx, turn.run(), func(text string) error {
			payload, errChunk := helps.CursorChatChunk(chunkID, created, req.Model, text, !sentRole)
			if errChunk != nil {
				return errChunk
			}
			sentRole = true
			return forward(append([]byte("data: "), payload...))
		})
		if runErr != nil {
			reporter.PublishFailure(ctx, runErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: runErr}:
			case <-ctx.Done():
			}
			return
		}
		reporter.Publish(ctx, result.Usage)

		final, errFinal := helps.CursorChatFinalChunk(chunkID, created, req.Model, result.Usage)
		if errFinal != nil {
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errFinal}:
			case <-ctx.Done():
			}
			return
		}
		if errFinish := forward(append([]byte("data: "), final...)); errFinish != nil {
			return
		}
		_ = forward([]byte("data: [DONE]"))
	}()

	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func (e *CursorExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	model := cursorModelFromRequest(req.Model)
	translated, err := e.translatePayload(req, opts, model, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	enc, err := helps.TokenizerForModel(model)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor executor: token counting failed: %w", err)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, sdktranslator.FromString("openai"), opts.SourceFormat, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translatedUsage)}, nil
}

func (e *CursorExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

// prepare builds everything one invocation needs: the translated payload, the
// prompt (with images written into a fresh workspace), and the selected credential.
// The caller must call close on the returned turn.
func (e *CursorExecutor) prepare(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*cursorTurn, error) {
	model := cursorModelFromRequest(req.Model)
	translated, err := e.translatePayload(req, opts, model, stream)
	if err != nil {
		return nil, err
	}

	workspace, err := os.MkdirTemp("", "cliproxy-cursor-*")
	if err != nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: fmt.Sprintf("cursor executor: create workspace: %v", err)}
	}
	turn := &cursorTurn{
		model:      model,
		apiKey:     cursorAPIKeyFor(auth),
		workspace:  workspace,
		translated: translated,
	}
	prompt, err := helps.CursorPromptFromOpenAIPayloadWithImages(translated, workspace)
	if err != nil {
		turn.close()
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		turn.close()
		return nil, statusErr{code: http.StatusBadRequest, msg: "cursor executor: empty prompt"}
	}
	turn.prompt = prompt
	return turn, nil
}

// translatePayload runs built-in translation and thinking first, then applies
// user payload rules once as the last semantic step.
func (e *CursorExecutor) translatePayload(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, model string, stream bool) ([]byte, error) {
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, model, originalSource, stream)
	translated := sdktranslator.TranslateRequest(from, to, model, req.Payload, stream)

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	return helps.ApplyPayloadConfigWithRoot(e.cfg, model, to.String(), "", translated, originalTranslated, requestedModel, ""), nil
}

// cursorAPIKeyFor returns the key for this auth only when its record references
// CURSOR_API_KEY. The value is read from the process env at request time and never stored.
func cursorAPIKeyFor(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if name, _ := auth.Metadata[sdkauth.CursorAPIKeyEnvMetadataKey].(string); strings.TrimSpace(name) != sdkauth.CursorAPIKeyEnv {
		return ""
	}
	return strings.TrimSpace(os.Getenv(sdkauth.CursorAPIKeyEnv))
}

func cursorModelFromRequest(requested string) string {
	model := helps.CursorProviderModelID(thinking.ParseSuffix(requested).ModelName)
	if model == "" {
		return "auto"
	}
	return model
}

func newCursorChatID() string {
	return "chatcmpl-" + uuid.NewString()
}
