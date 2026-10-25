// Package codexusage reads Codex account usage windows from the wham/usage endpoint. Native quota
// reports and the quota-aware selector share this one fetch, parse and read path.
package codexusage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	Endpoint = "https://chatgpt.com/backend-api/wham/usage"
	// UserAgent makes the usage endpoint answer with the shape it returns to the codex CLI.
	UserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
	bodyLimit = 512 * 1024
)

// Observation names, as upstream stores them in response headers and Auth.Quota.Signals.
const (
	SignalAllowed      = "X-Codex-Allowed"
	SignalLimitReached = "X-Codex-Limit-Reached"
	SignalPrimary      = "X-Codex-Primary-"
	SignalSecondary    = "X-Codex-Secondary-"
)

// Window is one usage window. HasUsedPercent is false when the percentage is missing or outside 0 to 100.
type Window struct {
	UsedPercent    float64
	HasUsedPercent bool
	ResetAt        time.Time
}

// Snapshot is the account-level view of one read. Allowed and LimitReached name no window.
type Snapshot struct {
	Allowed      bool
	LimitReached bool
	Primary      Window
	Secondary    Window
}

// Fetch GETs wham/usage with the caller's client and returns the windows in observation form.
// It sets no timeout; ctx cancels the call.
func Fetch(ctx context.Context, client *http.Client, token, accountID string) (http.Header, error) {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(accountID) == "" {
		return nil, errors.New("codex usage: missing access token or account_id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Chatgpt-Account-Id", accountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("codex usage: close body: %v", errClose)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codex usage: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return nil, err
	}
	return Parse(body)
}

// Parse converts a wham/usage payload into the X-Codex-* observation names. Reset offsets stay
// relative; Read resolves them against the observation time.
func Parse(body []byte) (http.Header, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("codex usage: payload is not an object")
	}
	rl := firstResult(root, "rate_limit", "rateLimit")
	if !rl.IsObject() {
		return nil, fmt.Errorf("codex usage: no rate_limit object")
	}
	headers := make(http.Header)
	setScalar(headers, SignalAllowed, firstResult(rl, "allowed"))
	setScalar(headers, SignalLimitReached, firstResult(rl, "limit_reached", "limitReached"))
	setWindow(headers, SignalPrimary, firstResult(rl, "primary_window", "primaryWindow"))
	setWindow(headers, SignalSecondary, firstResult(rl, "secondary_window", "secondaryWindow"))
	if headers.Get(SignalPrimary+"Used-Percent") == "" && headers.Get(SignalSecondary+"Used-Percent") == "" {
		return nil, fmt.Errorf("codex usage: no usage windows")
	}
	return headers, nil
}

func setWindow(headers http.Header, prefix string, window gjson.Result) {
	if !window.IsObject() {
		return
	}
	setScalar(headers, prefix+"Used-Percent", firstResult(window, "used_percent", "usedPercent"))
	setScalar(headers, prefix+"Reset-At", firstResult(window, "reset_at", "resetAt"))
	setScalar(headers, prefix+"Reset-After-Seconds", firstResult(window, "reset_after_seconds", "resetAfterSeconds"))
}

func setScalar(headers http.Header, name string, value gjson.Result) {
	switch value.Type {
	case gjson.Number, gjson.True, gjson.False:
		headers.Set(name, strings.TrimSpace(value.Raw))
	case gjson.String:
		if s := strings.TrimSpace(value.Str); s != "" {
			headers.Set(name, s)
		}
	}
}

func firstResult(node gjson.Result, paths ...string) gjson.Result {
	for _, p := range paths {
		if v := node.Get(p); v.Exists() {
			return v
		}
	}
	return gjson.Result{}
}

// Read builds a snapshot from observation values. get returns the stored value of one signal name.
// Reset offsets are relative to observedAt, so the read does not depend on when it happens.
func Read(get func(name string) string, observedAt time.Time) Snapshot {
	return Snapshot{
		Allowed:      boolSignal(get(SignalAllowed), true),
		LimitReached: boolSignal(get(SignalLimitReached), false),
		Primary:      readWindow(get, SignalPrimary, observedAt),
		Secondary:    readWindow(get, SignalSecondary, observedAt),
	}
}

func boolSignal(value string, fallback bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return v
}

func readWindow(get func(string) string, prefix string, observedAt time.Time) Window {
	var w Window
	if f, err := strconv.ParseFloat(strings.TrimSpace(get(prefix+"Used-Percent")), 64); err == nil && f >= 0 && f <= 100 {
		w.UsedPercent = f
		w.HasUsedPercent = true
	}
	if sec, err := strconv.ParseInt(strings.TrimSpace(get(prefix+"Reset-At")), 10, 64); err == nil && sec > 0 {
		w.ResetAt = time.Unix(sec, 0)
	} else if sec, err := strconv.ParseInt(strings.TrimSpace(get(prefix+"Reset-After-Seconds")), 10, 64); err == nil && sec > 0 {
		w.ResetAt = observedAt.Add(time.Duration(sec) * time.Second)
	}
	return w
}
