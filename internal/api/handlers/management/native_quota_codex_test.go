package management

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/codexusage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// codexFixtureTransport answers every request with one canned response and records the last request.
type codexFixtureTransport struct {
	status int
	body   string
	last   *http.Request
}

func (f *codexFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.last = req
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body)), Header: make(http.Header), Request: req}, nil
}

// codexWham builds a wham/usage body with the real response shape: plan, rate_limit windows,
// code_review_rate_limit, additional_rate_limits and credits. Empty window arguments are omitted.
func codexWham(allowed, limitReached bool, primary, secondary string) string {
	windows := ""
	if primary != "" {
		windows += `"primary_window":` + primary
	}
	if secondary != "" {
		if windows != "" {
			windows += ","
		}
		windows += `"secondary_window":` + secondary
	}
	return fmt.Sprintf(`{"plan_type":"plus","rate_limit":{"allowed":%t,"limit_reached":%t,%s},`+
		`"code_review_rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800,"reset_at":1800604800}},`+
		`"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","metered_feature":"codex_bengalfox","rate_limit":{"allowed":true,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":900,"reset_at":1800000900}}}],`+
		`"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`, allowed, limitReached, windows)
}

// codexWindow is one wham window with an absolute reset_at, as the endpoint returns it.
func codexWindow(used string, resetAt int64) string {
	return fmt.Sprintf(`{"used_percent":%s,"limit_window_seconds":18000,"reset_after_seconds":60,"reset_at":%d}`, used, resetAt)
}

// codexFixtureReport runs the production fetch path against a canned body, then normalizes it.
func codexFixtureReport(t *testing.T, body string, now time.Time, models []string) pluginapi.QuotaFetchResponse {
	t.Helper()
	client := &http.Client{Transport: &codexFixtureTransport{status: http.StatusOK, body: body}}
	headers, err := codexusage.Fetch(context.Background(), client, "token", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	return coreauth.CodexUnifiedReport(codexusage.Read(headers.Get, now), models, now)
}

func TestCodexFetchSendsAccountHeadersAndRejectsBadResponses(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := codexWham(true, false, codexWindow("42", now.Unix()+1800), codexWindow("12", now.Unix()+3600))
	transport := &codexFixtureTransport{status: http.StatusOK, body: body}
	if _, err := codexusage.Fetch(context.Background(), &http.Client{Transport: transport}, "token", "acct-1"); err != nil {
		t.Fatal(err)
	}
	req := transport.last
	if req.URL.String() != codexusage.Endpoint || req.Header.Get("Authorization") != "Bearer token" || req.Header.Get("Chatgpt-Account-Id") != "acct-1" || req.Header.Get("User-Agent") != codexusage.UserAgent {
		t.Fatalf("request %s headers %v", req.URL, req.Header)
	}

	for _, tc := range []struct {
		name      string
		status    int
		body      string
		token, id string
	}{
		{"unauthorized", http.StatusUnauthorized, body, "token", "acct-1"},
		{"no usage windows", http.StatusOK, `{"rate_limit":{"allowed":true}}`, "token", "acct-1"},
		{"not an object", http.StatusOK, `[]`, "token", "acct-1"},
		{"missing token", http.StatusOK, body, "", "acct-1"},
		{"missing account", http.StatusOK, body, "token", ""},
	} {
		client := &http.Client{Transport: &codexFixtureTransport{status: tc.status, body: tc.body}}
		if _, err := codexusage.Fetch(context.Background(), client, tc.token, tc.id); err == nil {
			t.Fatalf("%s: Fetch accepted the response", tc.name)
		}
	}
}

func TestCodexQuotaMissingCredentialMaps400Not502(t *testing.T) {
	h := &Handler{}
	a := &coreauth.Auth{ID: "codex-a", Provider: "codex", Metadata: map[string]any{"access_token": "token"}}
	if _, handled, err := h.fetchCodexQuota(context.Background(), a); !handled || err == nil {
		t.Fatalf("handled=%v err=%v, want handled missing-credential error", handled, err)
	}
}

func TestNativeQuotaProviderCodexOnlyForOAuth(t *testing.T) {
	oauth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "t", "account_id": "a"}}
	if got := nativeQuotaProvider(oauth); got != "codex" {
		t.Fatalf("codex OAuth provider = %q, want codex", got)
	}
	apiKey := &coreauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}
	if got := nativeQuotaProvider(apiKey); got != "" {
		t.Fatalf("codex API key provider = %q, want none", got)
	}
}

// Each fixture is the real response shape with one window or flag changed. Expected values are the
// model-scoped buckets; a missing window, stale reset or invalid value must not appear as a bucket.
func TestCodexReportNormalizesFixtures(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	type want struct {
		window    string
		remaining float64
		resetAt   int64
	}
	for _, tc := range []struct {
		name string
		body string
		want []want
	}{
		{
			name: "real shape: both windows live, named limit ignored",
			body: codexWham(true, false, codexWindow("42", now.Unix()+1800), codexWindow("12", now.Unix()+3600)),
			want: []want{{"primary", .58, now.Unix() + 1800}, {"secondary", .88, now.Unix() + 3600}},
		},
		{
			name: "stale: primary reset already passed is dropped",
			body: codexWham(true, false, codexWindow("42", now.Unix()-1), codexWindow("12", now.Unix()+3600)),
			want: []want{{"secondary", .88, now.Unix() + 3600}},
		},
		{
			name: "bad percentages never become usable windows",
			body: codexWham(true, false, codexWindow("150", now.Unix()+1800), codexWindow(`"abc"`, now.Unix()+3600)),
		},
		{
			name: "negative percentage is not unlimited",
			body: codexWham(true, false, codexWindow("-5", now.Unix()+1800), codexWindow("12", now.Unix()+3600)),
			want: []want{{"secondary", .88, now.Unix() + 3600}},
		},
		{
			name: "window without reset is not reported",
			body: codexWham(true, false, `{"used_percent":10}`, codexWindow("12", now.Unix()+3600)),
			want: []want{{"secondary", .88, now.Unix() + 3600}},
		},
		{
			name: "exhausted live window reports zero with its reset",
			body: codexWham(true, false, codexWindow("100", now.Unix()+900), codexWindow("12", now.Unix()+3600)),
			want: []want{{"primary", 0, now.Unix() + 900}, {"secondary", .88, now.Unix() + 3600}},
		},
		{
			name: "allowed=false flag applies until the nearest live reset",
			body: codexWham(false, false, codexWindow("30", now.Unix()+100), codexWindow("12", now.Unix()+3600)),
			want: []want{{"primary", .7, now.Unix() + 100}, {"secondary", .88, now.Unix() + 3600}, {"limit-reached", 0, now.Unix() + 100}},
		},
		{
			name: "limit_reached flag expires with a passed reset when no live window is exhausted",
			body: codexWham(true, true, codexWindow("10", now.Unix()-1), codexWindow("20", now.Unix()+3600)),
			want: []want{{"secondary", .8, now.Unix() + 3600}},
		},
		{
			name: "limit_reached flag holds while a live window is exhausted",
			body: codexWham(true, true, codexWindow("100", now.Unix()+100), codexWindow("20", now.Unix()+3600)),
			want: []want{{"primary", 0, now.Unix() + 100}, {"secondary", .8, now.Unix() + 3600}, {"limit-reached", 0, now.Unix() + 100}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := codexFixtureReport(t, tc.body, now, []string{"gpt-5.4"})
			var got []pluginapi.QuotaBucket
			for _, group := range report.Groups {
				got = append(got, group.Buckets...)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("buckets = %+v, want %d", got, len(tc.want))
			}
			for i, w := range tc.want {
				b := got[i]
				reset, err := time.Parse(time.RFC3339Nano, b.ResetTime)
				if err != nil {
					t.Fatalf("bucket %d reset %q: %v", i, b.ResetTime, err)
				}
				if b.Window != w.window || math.Abs(b.RemainingFraction-w.remaining) > 1e-9 || reset.Unix() != w.resetAt {
					t.Fatalf("bucket %d = %+v, want window %s remaining %v reset %d", i, b, w.window, w.remaining, w.resetAt)
				}
				if b.Scope != "model" || len(b.Models) != 1 || b.Models[0] != "gpt-5.4" {
					t.Fatalf("bucket %d scope = %q models %v, want model-scoped gpt-5.4", i, b.Scope, b.Models)
				}
			}
		})
	}
}

// A report with no models is empty, so an account whose routes are not in use never ranks.
func TestCodexReportWithoutModelsIsEmpty(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	report := codexFixtureReport(t, codexWham(true, false, codexWindow("42", now.Unix()+1800), ""), now, nil)
	if len(report.Groups) != 0 {
		t.Fatalf("report without models = %+v", report)
	}
}

func TestCodexUnifiedModelsFollowLiveRoutes(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{
		{ID: "unified-a", Routes: []config.UnifiedModelRoute{
			{Provider: "codex", Source: "codex/gpt-5.4", Model: "gpt-5.4", Plan: "included"},
			{Provider: "generic", Source: "generic/other", Model: "generic-wire", Plan: "included"},
		}},
		{ID: "unified-b", Routes: []config.UnifiedModelRoute{
			{Provider: "codex", Source: "codex/unregistered", Model: "gpt-unregistered", Plan: "included"},
		}},
	}}}}
	a := &coreauth.Auth{ID: "codex-live", Provider: "codex", Metadata: map[string]any{"access_token": "t", "account_id": "a"}}
	registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: "codex/gpt-5.4"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	if got := codexUnifiedModels(cfg, a); len(got) != 1 || got[0] != "gpt-5.4" {
		t.Fatalf("models = %v, want only the registered codex route", got)
	}
	if got := codexUnifiedModels(&config.Config{}, a); got != nil {
		t.Fatalf("disabled unified models = %v, want none", got)
	}
}

// Codex and a generic provider with the same windows must rank identically: the codex report is only a
// different producer of the same model-scoped buckets.
func TestCodexNativeReportScoresLikeNormalizedReports(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cfg := &config.Config{Routing: config.RoutingConfig{UnifiedModels: config.UnifiedModels{Enabled: true, Models: []config.UnifiedModel{{ID: "unified-test", Routes: []config.UnifiedModelRoute{
		{Provider: "codex", Source: "codex/native", Model: "gpt-5.4", Plan: "included"},
		{Provider: "generic", Source: "generic/native", Model: "generic-wire", Plan: "included"},
	}}}}}}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	codex := &coreauth.Auth{ID: "codex-a", Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "t", "account_id": "acct-1"}}
	generic := &coreauth.Auth{ID: "generic-a", Provider: "generic", Status: coreauth.StatusActive}
	for _, a := range []*coreauth.Auth{codex, generic} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	registry.GetGlobalRegistry().RegisterClient("codex-a", "codex", []*registry.ModelInfo{{ID: "codex/native"}, {ID: "cliproxy/unified-test"}})
	registry.GetGlobalRegistry().RegisterClient("generic-a", "generic", []*registry.ModelInfo{{ID: "generic/native"}, {ID: "cliproxy/unified-test"}})

	m.ObserveUnifiedQuota("codex-a", codexFixtureReport(t, codexWham(true, false, codexWindow("42", now.Unix()+1800), codexWindow("12", now.Unix()+3600)), now, []string{"gpt-5.4"}), now)
	reset := func(sec int64) string { return time.Unix(sec, 0).Format(time.RFC3339Nano) }
	m.ObserveUnifiedQuota("generic-a", pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "Generic usage", Buckets: []pluginapi.QuotaBucket{
		{Scope: "model", Models: []string{"generic-wire"}, Window: "primary", RemainingFraction: .58, ResetTime: reset(now.Unix() + 1800)},
		{Scope: "model", Models: []string{"generic-wire"}, Window: "secondary", RemainingFraction: .88, ResetTime: reset(now.Unix() + 3600)},
	}}}}, now)

	byID := map[string]coreauth.UnifiedRouteStatus{}
	for _, r := range m.UnifiedRoutes("cliproxy/unified-test", now) {
		byID[r.AuthID] = r
	}
	c, g := byID["codex-a"], byID["generic-a"]
	if !c.QuotaKnown || !g.QuotaKnown || c.Remaining == nil || g.Remaining == nil {
		t.Fatalf("routes not known: codex %+v generic %+v", c, g)
	}
	if math.Abs(*c.Remaining-*g.Remaining) > 1e-9 || math.Abs(c.CapacityScore-g.CapacityScore) > 1e-9 || c.ResetTime != g.ResetTime {
		t.Fatalf("codex %+v scores differently from generic %+v", c, g)
	}
	if math.Abs(*c.Remaining-.58) > 1e-9 {
		t.Fatalf("codex binding remaining = %v, want .58", *c.Remaining)
	}
}
