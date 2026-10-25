package codexusage

import (
	"net/http"
	"testing"
	"time"
)

func TestParseAcceptsCamelCaseAndKeepsRelativeResets(t *testing.T) {
	headers, err := Parse([]byte(`{"rateLimit":{"allowed":true,"limitReached":false,"primaryWindow":{"usedPercent":25,"resetAfterSeconds":120}}}`))
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Unix(1_800_000_000, 0)
	snap := Read(headers.Get, observedAt)
	if !snap.Allowed || snap.LimitReached || !snap.Primary.HasUsedPercent || snap.Primary.UsedPercent != 25 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if want := observedAt.Add(120 * time.Second); !snap.Primary.ResetAt.Equal(want) {
		t.Fatalf("reset = %v, want %v (relative to the observation)", snap.Primary.ResetAt, want)
	}
}

func TestReadRejectsInvalidPercentagesAndDefaultsFlags(t *testing.T) {
	signals := map[string]string{
		SignalPrimary + "Used-Percent":   "NaN",
		SignalPrimary + "Reset-At":       "1800000100",
		SignalSecondary + "Used-Percent": "101",
		SignalSecondary + "Reset-At":     "1800000100",
		SignalAllowed:                    "not-a-bool",
	}
	snap := Read(func(name string) string { return signals[name] }, time.Unix(1_800_000_000, 0))
	if snap.Primary.HasUsedPercent || snap.Secondary.HasUsedPercent {
		t.Fatalf("invalid percentages accepted: %+v", snap)
	}
	if !snap.Allowed || snap.LimitReached {
		t.Fatalf("flags = allowed %v limit %v, want the defaults", snap.Allowed, snap.LimitReached)
	}
}

func TestFetchRequiresCredentialsBeforeTouchingTheNetwork(t *testing.T) {
	if _, err := Fetch(t.Context(), &http.Client{Transport: failingTransport{}}, "", "acct"); err == nil {
		t.Fatal("Fetch without a token succeeded")
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("network must not be reached")
}
