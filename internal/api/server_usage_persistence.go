package api

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	log "github.com/sirupsen/logrus"
)

func (s *Server) configureUsagePersistence(oldCfg, newCfg *config.Config) {
	if s == nil {
		return
	}
	shouldPersist := newCfg != nil && newCfg.UsageStatisticsEnabled &&
		(newCfg.UsageStatisticsPersist || strings.TrimSpace(newCfg.UsageStatisticsFile) != "")
	desiredPath := ""
	desiredInterval := 60 * time.Second
	if newCfg != nil && newCfg.UsageStatisticsSaveIntervalSeconds > 0 {
		desiredInterval = time.Duration(newCfg.UsageStatisticsSaveIntervalSeconds) * time.Second
	}
	if shouldPersist {
		desiredPath = resolveUsageStatisticsPath(newCfg, s.configFilePath)
	}
	needsRestart := !shouldPersist && s.usagePersistence != nil
	if shouldPersist {
		needsRestart = s.usagePersistence == nil || oldCfg == nil ||
			oldCfg.UsageStatisticsEnabled != newCfg.UsageStatisticsEnabled ||
			oldCfg.UsageStatisticsPersist != newCfg.UsageStatisticsPersist ||
			strings.TrimSpace(oldCfg.UsageStatisticsFile) != strings.TrimSpace(newCfg.UsageStatisticsFile) ||
			oldCfg.UsageStatisticsSaveIntervalSeconds != newCfg.UsageStatisticsSaveIntervalSeconds
	}
	if !needsRestart {
		return
	}
	if s.usagePersistence != nil {
		s.usagePersistence.Stop()
		s.usagePersistence = nil
	}
	if !shouldPersist || desiredPath == "" {
		return
	}
	if strings.TrimSpace(newCfg.UsageStatisticsFile) == "" {
		if err := migrateUsageStatisticsFile(resolveLegacyUsageStatisticsPath(newCfg), desiredPath); err != nil {
			log.WithError(err).Warn("usage: failed to migrate persisted usage statistics")
		}
	}
	persistence := usage.NewPersistence(usage.PersistenceConfig{
		Stats: usage.GetRequestStatistics(), Path: desiredPath, Interval: desiredInterval,
	})
	if err := persistence.Load(); err != nil {
		log.WithError(err).Warn("usage: failed to load persisted usage statistics")
	}
	persistence.Start()
	s.usagePersistence = persistence
}
