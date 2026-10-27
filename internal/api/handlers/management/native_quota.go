package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// ListConfiguredQuotaCredentials exposes identifiers, never the configured API keys.
func (h *Handler) ListConfiguredQuotaCredentials(c *gin.Context) {
	files := []gin.H{}
	if h.authManager != nil {
		files = configuredQuotaCredentials(h.authManager.List())
	}
	c.JSON(http.StatusOK, gin.H{"files": files})
}

func configuredQuotaCredentials(auths []*coreauth.Auth) []gin.H {
	files := []gin.H{}
	byCredential := map[string]int{}
	for _, a := range auths {
		if a == nil || a.Attributes["api_key"] == "" || !strings.HasPrefix(a.Attributes["source"], "config:") {
			continue
		}
		a.EnsureIndex()
		label := a.Label
		if label == "" {
			label = a.Provider
		}
		provider := nativeQuotaProvider(a)
		scope := provider
		if scope == "" {
			scope = a.Attributes["base_url"]
		}
		// Multiple protocol routes can share one account credential and allowance.
		key := scope + "\x00" + a.Attributes["api_key"]
		if i, exists := byCredential[key]; exists {
			connections := files[i]["connections"].([]string)
			if !slices.Contains(connections, label) {
				files[i]["connections"] = append(connections, label)
			}
			if !a.Disabled {
				files[i]["disabled"] = false
				files[i]["auth_index"] = a.Index
			}
			continue
		}
		display := map[string]string{"commandcode": "CommandCode", "kimi": "Kimi Coding", "opencode-go": "OpenCode Go", "minimax": "MiniMax"}[provider]
		if display == "" {
			display = label
		}
		byCredential[key] = len(files)
		files = append(files, gin.H{"name": display + " (" + a.Index + ")", "provider": display, "auth_index": a.Index, "disabled": a.Disabled, "connections": []string{label}})
	}
	return files
}

func nativeQuotaProvider(a *coreauth.Auth) string {
	if a == nil {
		return ""
	}
	if a.Provider == "kimi" && a.AuthKind() == coreauth.AuthKindOAuth {
		return "kimi"
	}
	u, err := url.Parse(a.Attributes["base_url"])
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "opencode.ai":
		if strings.HasPrefix(u.Path, "/zen/go/") {
			return "opencode-go"
		}
	case "api.kimi.com":
		if strings.HasPrefix(u.Path, "/coding/") {
			return "kimi"
		}
	case "api.minimax.io":
		return "minimax"
	case "api.commandcode.ai":
		return "commandcode"
	}
	return ""
}

var errNativeQuotaKeyMissing = errors.New("configured API key not found")

const nativeQuotaUnsupported = "This provider does not expose a supported account quota API. Proxy request telemetry is not an account allowance."

// FetchCredentialQuotaV8 uses the plugin quota dispatch first, then built-in account adapters.
// It also serves the older /credentials/quota/native path for one release.
func (h *Handler) FetchCredentialQuotaV8(c *gin.Context) {
	var body credentialQuotaRequest
	if c.ShouldBindJSON(&body) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	authIndex := body.resolveAuthIndex()
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	a := h.authByIndex(authIndex)
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}
	ctx := c.Request.Context()
	report, handled, err := h.fetchPluginQuota(ctx, a, body.PluginID, body.Provider)
	if !handled {
		report, handled, err = h.fetchNativeQuota(ctx, a)
		if handled && err == nil {
			h.observeNativeQuota(a, report)
		}
	}
	switch {
	case !handled:
		c.JSON(http.StatusNotImplemented, gin.H{"error": nativeQuotaUnsupported})
	case errors.Is(err, errNativeQuotaKeyMissing):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case err != nil:
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusOK, report)
	}
}

// observeNativeQuota feeds a native report to every credential that shares the same account.
func (h *Handler) observeNativeQuota(a *coreauth.Auth, report pluginapi.QuotaFetchResponse) {
	if h.authManager == nil {
		return
	}
	for _, peer := range h.authManager.List() {
		if sameNativeAccount(a, peer) {
			h.authManager.ObserveUnifiedQuota(peer.ID, report, time.Now())
		}
	}
}

// unifiedQuotaFetch adapts the built-in adapters to the routing refresh. An unsupported account is an
// error here, so it never becomes an empty report.
func (h *Handler) unifiedQuotaFetch(ctx context.Context, a *coreauth.Auth) (pluginapi.QuotaFetchResponse, error) {
	// Shared plugin dispatch goes first, as in the management quota endpoint, so plugin-only
	// credentials refresh even when no built-in adapter matches.
	if report, handled, err := h.fetchPluginQuota(ctx, a, "", ""); handled || err != nil {
		return report, err
	}
	report, handled, err := h.fetchNativeQuota(ctx, a)
	if err == nil && !handled {
		return report, errors.New(nativeQuotaUnsupported)
	}
	return report, err
}

// fetchNativeQuota reports handled=false when no built-in adapter matches the credential.
func (h *Handler) fetchNativeQuota(ctx context.Context, a *coreauth.Auth) (pluginapi.QuotaFetchResponse, bool, error) {
	provider := nativeQuotaProvider(a)
	if provider == "" {
		return pluginapi.QuotaFetchResponse{}, false, nil
	}
	key := a.Attributes["api_key"]
	if key == "" && provider == "kimi" && a.AuthKind() == coreauth.AuthKindOAuth {
		key, _ = a.Metadata["access_token"].(string)
	}
	if key == "" {
		return pluginapi.QuotaFetchResponse{}, true, errNativeQuotaKeyMissing
	}
	// Config reload replaces h.cfg; proxy selection must read one snapshot under the lock.
	h.mu.Lock()
	transport := h.apiCallTransport(a, "")
	h.mu.Unlock()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	report, err := fetchNativeQuotaReport(ctx, client, provider, key)
	return report, true, err
}

// fetchNativeQuotaReport calls the provider account API and normalizes its report.
func fetchNativeQuotaReport(ctx context.Context, client *http.Client, provider, key string) (pluginapi.QuotaFetchResponse, error) {
	read := func(endpoint string) ([]byte, int, error) {
		return readNativeQuota(ctx, client, endpoint, key, provider)
	}
	var report pluginapi.QuotaFetchResponse
	var b []byte
	var status int
	var err error
	switch provider {
	case "opencode-go":
		if b, status, err = read("https://opencode.ai/zen/go/v1/usage"); err == nil {
			report, err = parseOpenCodeQuota(b, status)
		}
	case "kimi":
		if b, status, err = read("https://api.kimi.com/coding/v1/usages"); err == nil {
			report, err = parseKimiNativeQuota(b, status)
		}
	case "minimax":
		if b, status, err = read("https://www.minimax.io/v1/token_plan/remains"); err == nil {
			report, err = parseMiniMaxQuota(b, status)
		}
	case "commandcode":
		b, status, err = read("https://api.commandcode.ai/alpha/whoami?limits=1")
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("provider authentication returned HTTP %d", status)
		}
		if err != nil {
			break
		}
		query := ""
		if id := gjson.GetBytes(b, "org.id").String(); id != "" {
			query = "?orgId=" + url.QueryEscape(id)
		}
		if b, status, err = read("https://api.commandcode.ai/alpha/billing/credits" + query); err == nil {
			report, err = parseCommandCodeQuota(b, status)
		}
	default:
		err = errors.New(nativeQuotaUnsupported)
	}
	return report, err
}

func readNativeQuota(ctx context.Context, client *http.Client, endpoint, key, provider string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to create quota request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode/1.3.0")
	if provider == "kimi" {
		req.Header.Set("User-Agent", "kimi-code-cli/1.0")
		req.Header.Set("X-Msh-Platform", "kimi_code_cli")
	}
	if provider == "commandcode" {
		req.Header.Set("User-Agent", "cli")
		req.Header.Set("x-command-code-version", "1.74.1")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("provider quota request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil || !json.Valid(b) {
		return nil, resp.StatusCode, fmt.Errorf("provider returned an invalid quota response (HTTP %d)", resp.StatusCode)
	}
	return b, resp.StatusCode, nil
}

func quotaNumber(r gjson.Result) (float64, bool) {
	if !r.Exists() || (r.Type != gjson.Number && r.Type != gjson.String) {
		return 0, false
	}
	var n float64
	if r.Type == gjson.Number {
		n = r.Float()
	} else {
		var err error
		n, err = strconv.ParseFloat(r.String(), 64)
		if err != nil {
			return 0, false
		}
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}
func quotaRemaining(n float64) float64 { return math.Max(0, math.Min(1, n)) }
func parseOpenCodeQuota(b []byte, status int) (pluginapi.QuotaFetchResponse, error) {
	report := pluginapi.QuotaFetchResponse{}
	// The native API intentionally returns 403 alongside a valid exhausted-window report.
	if status != 200 && status != 403 {
		return report, fmt.Errorf("OpenCode quota returned HTTP %d", status)
	}
	group := pluginapi.QuotaGroup{DisplayName: "OpenCode Go"}
	for _, window := range []string{"rolling", "weekly", "monthly"} {
		r := gjson.GetBytes(b, "usage."+window)
		used, ok := quotaNumber(r.Get("percent"))
		if !ok || used < 0 || used > 100 {
			return report, fmt.Errorf("OpenCode returned no valid %s allowance", window)
		}
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: window, RemainingFraction: 1 - used/100, ResetTime: r.Get("resetsAt").String(), Description: "Provider-reported allowance remaining"})
	}
	report.Groups = []pluginapi.QuotaGroup{group}
	return report, nil
}
func parseKimiNativeQuota(b []byte, status int) (pluginapi.QuotaFetchResponse, error) {
	report := pluginapi.QuotaFetchResponse{}
	if status != 200 {
		return report, fmt.Errorf("Kimi quota returned HTTP %d", status)
	}
	group := pluginapi.QuotaGroup{DisplayName: "Kimi Coding"}
	for _, p := range []struct{ path, label string }{{"usages.limit_5h", "5-hour"}, {"usages.limit_7d", "weekly"}} {
		r := gjson.GetBytes(b, p.path)
		used, ok := quotaNumber(r.Get("used_ratio"))
		if !ok || used < 0 || used > 1 {
			continue
		}
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: p.label, RemainingFraction: 1 - used, ResetTime: r.Get("reset_time").String()})
	}
	if len(group.Buckets) == 0 {
		return report, fmt.Errorf("Kimi returned no valid allowance")
	}
	report.Groups = []pluginapi.QuotaGroup{group}
	return report, nil
}
func parseMiniMaxQuota(b []byte, status int) (pluginapi.QuotaFetchResponse, error) {
	report := pluginapi.QuotaFetchResponse{}
	if status != 200 {
		return report, fmt.Errorf("MiniMax quota returned HTTP %d", status)
	}
	code := gjson.GetBytes(b, "base_resp.status_code").Int()
	if code == 2062 {
		return report, fmt.Errorf("MiniMax reports no active token plan subscription; this is not a zero remaining quota")
	}
	if code != 0 {
		return report, fmt.Errorf("MiniMax quota returned provider code %d", code)
	}
	group := pluginapi.QuotaGroup{DisplayName: "MiniMax Coding Plan"}
	for _, r := range gjson.GetBytes(b, "model_remains").Array() {
		total, ok := quotaNumber(r.Get("current_interval_total_count"))
		remaining, okRemaining := quotaNumber(r.Get("current_interval_remain_count"))
		if !ok || !okRemaining || total <= 0 || remaining < 0 {
			continue
		}
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Scope: "model", Models: []string{r.Get("model_name").String()}, Window: r.Get("model_name").String(), RemainingFraction: quotaRemaining(remaining / total), ResetTime: r.Get("end_time").String()})
	}
	if len(group.Buckets) == 0 {
		return report, fmt.Errorf("MiniMax returned no valid token plan allowance")
	}
	report.Groups = []pluginapi.QuotaGroup{group}
	return report, nil
}
func parseCommandCodeQuota(b []byte, status int) (pluginapi.QuotaFetchResponse, error) {
	report := pluginapi.QuotaFetchResponse{}
	if status != 200 {
		return report, fmt.Errorf("CommandCode billing returned HTTP %d", status)
	}
	for _, p := range []struct{ key, label string }{{"monthlyCredits", "Monthly credit remaining"}, {"purchasedCredits", "Purchased credit remaining"}, {"freeCredits", "Free credit remaining"}} {
		n, ok := quotaNumber(gjson.GetBytes(b, "credits."+p.key))
		if ok {
			report.Summary = append(report.Summary, pluginapi.QuotaMetric{Key: p.key, Label: p.label, Value: n, Format: "currency", Currency: "USD"})
		}
	}
	group := pluginapi.QuotaGroup{DisplayName: "CommandCode usage windows"}
	for _, window := range []string{"fiveHour", "weekly"} {
		r := gjson.GetBytes(b, "windowLimits."+window)
		used, ok := quotaNumber(r.Get("used"))
		cap, okCap := quotaNumber(r.Get("cap"))
		if !ok || !okCap || cap <= 0 || used < 0 {
			continue
		}
		reset := ""
		if n, exists := quotaNumber(r.Get("resetAt")); exists && n > 0 {
			reset = time.UnixMilli(int64(n)).UTC().Format(time.RFC3339)
		}
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: window, RemainingFraction: quotaRemaining(1 - used/cap), ResetTime: reset})
	}
	if len(group.Buckets) > 0 {
		report.Groups = []pluginapi.QuotaGroup{group}
	}
	if len(report.Groups) == 0 && len(report.Summary) == 0 {
		return report, fmt.Errorf("CommandCode returned no valid account billing report")
	}
	return report, nil
}
