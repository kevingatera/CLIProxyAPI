package management

import (
	"encoding/json"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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
