package management

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestOpenCodeQuotaExhaustedReport(t *testing.T) {
	b := []byte(`{"usage":{"rolling":{"percent":0},"weekly":{"percent":25,"resetsAt":"2026-10-12T00:00:00Z"},"monthly":{"percent":100,"status":"rate-limited"}}}`)
	report, err := parseOpenCodeQuota(b, 403)
	if err != nil {
		t.Fatal(err)
	}
	buckets := report.Groups[0].Buckets
	if buckets[0].RemainingFraction != 1 || buckets[1].RemainingFraction != .75 || buckets[2].RemainingFraction != 0 {
		t.Fatalf("wrong remaining allowances: %+v", buckets)
	}
	if _, err = parseOpenCodeQuota([]byte(`{"error":"forbidden"}`), 403); err == nil {
		t.Fatal("authentication errors must not become zero quota")
	}
	if _, err = parseOpenCodeQuota(b, 401); err == nil {
		t.Fatal("unauthorized responses must not become quota")
	}
}
func TestCommandCodeQuotaSanitizesBilling(t *testing.T) {
	b := []byte(`{"credits":{"monthlyCredits":0,"purchasedCredits":8.89,"freeCredits":0,"secret":"private"},"windowLimits":{"weekly":{"used":55,"cap":90,"resetAt":1791421240668}},"email":"private@example.com"}`)
	report, err := parseCommandCodeQuota(b, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Summary) != 3 || report.Summary[1].Value != 8.89 {
		t.Fatalf("missing billing credit: %+v", report)
	}
	out, _ := json.Marshal(report)
	if strings.Contains(string(out), "private") {
		t.Fatal("account metadata leaked into quota")
	}
	if len(report.Groups[0].Buckets) != 1 || report.Groups[0].Buckets[0].ResetTime == "" {
		t.Fatal("missing usage window")
	}
	if _, err = parseCommandCodeQuota([]byte(`{}`), 200); err == nil {
		t.Fatal("missing report must not be shown as zero")
	}
}
func TestNativeQuotaInactiveAndMalformed(t *testing.T) {
	if _, err := parseMiniMaxQuota([]byte(`{"base_resp":{"status_code":2062},"model_remains":null}`), 200); err == nil || !strings.Contains(err.Error(), "no active") {
		t.Fatalf("missing inactive-plan explanation: %v", err)
	}
	if _, err := parseKimiNativeQuota([]byte(`{"usages":{"limit_5h":{"used_ratio":2}}}`), 200); err == nil {
		t.Fatal("invalid quota ratio accepted")
	}
	if _, err := parseKimiNativeQuota([]byte(`{"usages":{"limit_5h":{"used_ratio":0},"limit_7d":{"used_ratio":0.5}}}`), 200); err != nil {
		t.Fatal(err)
	}
}
func TestNativeQuotaProviderBoundaries(t *testing.T) {
	for _, base := range []string{"https://opencode.ai.evil.test/zen/go/v1", "https://opencode.ai/zen/v1", "http://private.example/v1"} {
		if got := nativeQuotaProvider(&coreauth.Auth{Attributes: map[string]string{"base_url": base}}); got != "" {
			t.Fatalf("unexpected quota adapter for %s: %s", base, got)
		}
	}
	if got := nativeQuotaProvider(&coreauth.Auth{Attributes: map[string]string{"base_url": "https://opencode.ai/zen/go/v1"}}); got != "opencode-go" {
		t.Fatalf("provider=%s", got)
	}
}

func TestConfiguredQuotaCredentialsShareAccountAcrossProtocols(t *testing.T) {
	makeAuth := func(id, label, key string, disabled bool) *coreauth.Auth {
		return &coreauth.Auth{ID: id, Label: label, Disabled: disabled, Attributes: map[string]string{"api_key": key, "base_url": "https://api.commandcode.ai/provider/v1", "source": "config:" + label}}
	}
	a := makeAuth("claude-route", "claude-apikey", "same-secret", true)
	b := makeAuth("openai-route", "command-code", "same-secret", false)
	other := makeAuth("another-account", "command-code", "different-secret", false)
	files := configuredQuotaCredentials([]*coreauth.Auth{a, b, other})
	if len(files) != 2 {
		t.Fatalf("got %d cards, want two distinct credentials", len(files))
	}
	if len(files[0]["connections"].([]string)) != 2 || files[0]["disabled"] != false || files[0]["auth_index"] != b.Index {
		t.Fatalf("wrong shared account: %+v", files[0])
	}
	out, _ := json.Marshal(files)
	if strings.Contains(string(out), "secret") {
		t.Fatal("credential leaked")
	}
}

// fixtureTransport serves sanitized provider responses by URL and records each request.
type fixtureTransport struct {
	t         *testing.T
	responses map[string]fixtureResponse
	requested []string
}

type fixtureResponse struct {
	status int
	file   string
}

func (f *fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requested = append(f.requested, req.URL.String())
	if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
		f.t.Errorf("%s: Authorization=%q", req.URL, got)
	}
	resp, ok := f.responses[req.URL.String()]
	if !ok {
		f.t.Errorf("unexpected provider request %s", req.URL)
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: req}, nil
	}
	body, err := os.ReadFile(filepath.Join("testdata", "native_quota", resp.file))
	if err != nil {
		f.t.Fatal(err)
	}
	return &http.Response{StatusCode: resp.status, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}, Request: req}, nil
}

func TestNativeQuotaHTTPAdapters(t *testing.T) {
	for _, tc := range []struct {
		provider  string
		responses map[string]fixtureResponse
		wantURLs  []string
		check     func(*testing.T, pluginapi.QuotaFetchResponse)
		wantErr   string
	}{
		{
			provider:  "opencode-go",
			responses: map[string]fixtureResponse{"https://opencode.ai/zen/go/v1/usage": {http.StatusForbidden, "opencode-go-usage-exhausted.json"}},
			check: func(t *testing.T, r pluginapi.QuotaFetchResponse) {
				b := r.Groups[0].Buckets
				if len(b) != 3 || b[0].RemainingFraction != .875 || b[2].RemainingFraction != 0 || b[1].ResetTime == "" {
					t.Fatalf("buckets=%+v", b)
				}
			},
		},
		{
			provider:  "kimi",
			responses: map[string]fixtureResponse{"https://api.kimi.com/coding/v1/usages": {http.StatusOK, "kimi-usages.json"}},
			check: func(t *testing.T, r pluginapi.QuotaFetchResponse) {
				b := r.Groups[0].Buckets
				if len(b) != 2 || b[0].RemainingFraction != .75 || b[1].RemainingFraction != .5 {
					t.Fatalf("buckets=%+v", b)
				}
			},
		},
		{
			provider:  "minimax",
			responses: map[string]fixtureResponse{"https://www.minimax.io/v1/token_plan/remains": {http.StatusOK, "minimax-token-plan-remains.json"}},
			check: func(t *testing.T, r pluginapi.QuotaFetchResponse) {
				if b := r.Groups[0].Buckets; len(b) != 1 || b[0].RemainingFraction != .25 {
					t.Fatalf("buckets=%+v", b)
				}
			},
		},
		{
			provider:  "minimax",
			responses: map[string]fixtureResponse{"https://www.minimax.io/v1/token_plan/remains": {http.StatusOK, "minimax-token-plan-inactive.json"}},
			wantErr:   "no active token plan",
		},
		{
			provider: "commandcode",
			responses: map[string]fixtureResponse{
				"https://api.commandcode.ai/alpha/whoami?limits=1":                    {http.StatusOK, "commandcode-whoami.json"},
				"https://api.commandcode.ai/alpha/billing/credits?orgId=org-redacted": {http.StatusOK, "commandcode-billing-credits.json"},
			},
			wantURLs: []string{"https://api.commandcode.ai/alpha/whoami?limits=1", "https://api.commandcode.ai/alpha/billing/credits?orgId=org-redacted"},
			check: func(t *testing.T, r pluginapi.QuotaFetchResponse) {
				if len(r.Summary) != 3 || r.Summary[1].Value != 8.89 || len(r.Groups[0].Buckets) != 2 {
					t.Fatalf("report=%+v", r)
				}
				out, _ := json.Marshal(r)
				if strings.Contains(string(out), "redacted") {
					t.Fatal("account identity leaked into quota report")
				}
			},
		},
		{
			provider:  "commandcode",
			responses: map[string]fixtureResponse{"https://api.commandcode.ai/alpha/whoami?limits=1": {http.StatusUnauthorized, "commandcode-whoami.json"}},
			wantErr:   "HTTP 401",
		},
	} {
		t.Run(tc.provider+"/"+tc.wantErr, func(t *testing.T) {
			transport := &fixtureTransport{t: t, responses: tc.responses}
			report, err := fetchNativeQuotaReport(context.Background(), &http.Client{Transport: transport}, tc.provider, "test-key")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantURLs != nil && !slices.Equal(transport.requested, tc.wantURLs) {
				t.Fatalf("requested=%v", transport.requested)
			}
			tc.check(t, report)
		})
	}
}

func TestFetchCredentialQuotaV8Dispatch(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	register := func(id, provider, baseURL, key string) string {
		a := &coreauth.Auth{ID: id, Provider: provider, Attributes: map[string]string{"api_key": key, "base_url": baseURL, "source": "config:" + id}}
		index := a.EnsureIndex()
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		return index
	}
	pluginBacked := register("plugin-backed", "opencode-go", "https://opencode.ai/zen/go/v1", "k")
	unsupported := register("unsupported", "openai-compatibility", "https://relay.example/v1", "k")
	missingKey := register("missing-key", "openai-compatibility", "https://opencode.ai/zen/go/v1", "")

	host := pluginhost.New()
	host.RegisterPluginForTest("opencode-plugin", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{identifier: "opencode-go"}}})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(host)

	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{}`, http.StatusBadRequest},
		{`{"auth_index":"missing"}`, http.StatusNotFound},
		{`{"auth_index":"` + pluginBacked + `"}`, http.StatusOK},
		{`{"auth_index":"` + unsupported + `"}`, http.StatusNotImplemented},
		{`{"auth_index":"` + missingKey + `"}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/credentials/quota", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.FetchCredentialQuotaV8(c)
		if rec.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.body, rec.Code, rec.Body.String())
		}
	}
}

func TestFetchCredentialQuotaV8PrefersPluginReport(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	a := &coreauth.Auth{ID: "opencode", Provider: "opencode-go", Attributes: map[string]string{"api_key": "k", "base_url": "https://opencode.ai/zen/go/v1", "source": "config:opencode"}}
	index := a.EnsureIndex()
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	calls := 0
	host := pluginhost.New()
	host.RegisterPluginForTest("opencode-plugin", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{
		identifier: "opencode-go",
		fetchFn: func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
			calls++
			return pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{TierID: "plugin-tier"}}, nil
		},
	}}})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(host)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/credentials/quota", strings.NewReader(`{"auth_index":"`+index+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuotaV8(c)
	if rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	var report pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Subscription == nil || report.Subscription.TierID != "plugin-tier" {
		t.Fatalf("report=%+v; the plugin report must be served without the built-in adapter", report)
	}
}

// Config reloads swap h.cfg while native fetches read the proxy transport. Both proxies point at
// closed local ports, so the fetch fails without network access; the race detector checks the locking.
func TestNativeQuotaFetchReadsConfigSnapshotUnderReload(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	a := &coreauth.Auth{ID: "native", Provider: "openai-compatibility", Attributes: map[string]string{"api_key": "k", "base_url": "https://opencode.ai/zen/go/v1", "source": "config:native"}}
	index := a.EnsureIndex()
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:1"}}, manager)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.SetConfig(&config.Config{AuthDir: authDir, SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:" + strconv.Itoa(1+i%2)}})
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v8/management/credentials/quota", strings.NewReader(`{"auth_index":"`+index+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h.FetchCredentialQuotaV8(c)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("iteration %d: status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
}
