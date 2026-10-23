package usage

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func init() {
	coreusage.RegisterPlugin(&usagePlugin{stats: defaultRequestStatistics})
}

// usagePlugin feeds usage records into the durable aggregate. It shares the
// usage-statistics-enabled switch with the upstream usage queue.
type usagePlugin struct {
	stats *RequestStatistics
}

// HandleUsage implements coreusage.Plugin.
func (p *usagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.stats == nil || !redisqueue.UsageStatisticsEnabled() {
		return
	}
	p.stats.Record(ctx, record)
}
