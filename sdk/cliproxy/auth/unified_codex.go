package auth

import (
	"slices"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/codexusage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// CodexUnifiedReport normalizes one Codex usage read into the unified quota shape. The windows are
// account-wide, so every upstream model in models reads the same buckets. A window without a reset is
// not reported, and a window whose reset has passed is omitted, which leaves it unknown until the next
// read. Named additional limits are not mapped to models.
func CodexUnifiedReport(snap codexusage.Snapshot, models []string, now time.Time) pluginapi.QuotaFetchResponse {
	report := pluginapi.QuotaFetchResponse{}
	if len(models) == 0 {
		return report
	}
	group := pluginapi.QuotaGroup{DisplayName: "Codex usage windows"}
	var nearestReset time.Time
	expired, exhausted := false, false
	for _, named := range []struct {
		name   string
		window codexusage.Window
	}{{"primary", snap.Primary}, {"secondary", snap.Secondary}} {
		w := named.window
		if w.ResetAt.IsZero() {
			continue
		}
		if !w.ResetAt.After(now) {
			expired = true
			continue
		}
		if nearestReset.IsZero() || w.ResetAt.Before(nearestReset) {
			nearestReset = w.ResetAt
		}
		if !w.HasUsedPercent {
			continue
		}
		remaining := 1 - w.UsedPercent/100
		exhausted = exhausted || remaining <= 0
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
			Scope:             "model",
			Models:            slices.Clone(models),
			Window:            named.name,
			RemainingFraction: max(remaining, 0),
			ResetTime:         w.ResetAt.Format(time.RFC3339Nano),
			Description:       "Provider-reported allowance remaining",
		})
	}
	// The flags name no window. A passed reset most likely owns them, so they expire with it unless
	// a live window is exhausted. A flag bucket without a reset stays exhausted until the next read.
	flagged := !snap.Allowed || snap.LimitReached
	if flagged && expired && !exhausted {
		flagged = false
	}
	if flagged {
		limit := pluginapi.QuotaBucket{Scope: "model", Models: slices.Clone(models), Window: "limit-reached", Description: "Provider reports the account limit reached"}
		if !nearestReset.IsZero() {
			limit.ResetTime = nearestReset.Format(time.RFC3339Nano)
		}
		group.Buckets = append(group.Buckets, limit)
	}
	if len(group.Buckets) > 0 {
		report.Groups = []pluginapi.QuotaGroup{group}
	}
	return report
}
