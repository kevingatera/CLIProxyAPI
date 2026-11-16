package usage

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func init() {
	coreusage.RegisterPlugin(&usagePlugin{stats: defaultRequestStatistics})
}

// usagePlugin feeds usage records from the core usage manager into the aggregate.
// It runs beside the redis usage queue plugin and honors the same
// usage-statistics-enabled switch. The aggregate also needs SetCollecting(true),
// which persistence enables only when it is configured.
type usagePlugin struct {
	stats *RequestStatistics
}

// HandleUsage implements coreusage.Plugin.
func (p *usagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !redisqueue.UsageStatisticsEnabled() {
		return
	}
	p.stats.RecordCollected(ctx, record)
}
