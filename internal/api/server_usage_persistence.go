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
	s.usagePersistenceMu.Lock()
	defer s.usagePersistenceMu.Unlock()
	if s.usagePersistenceClosed {
		// Shutdown already flushed and stopped persistence; a late reload must not restart it.
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
	s.stopUsagePersistenceLocked()
	if !shouldPersist || desiredPath == "" {
		return
	}
	if strings.TrimSpace(newCfg.UsageStatisticsFile) == "" {
		if err := migrateUsageStatisticsFile(resolveLegacyUsageStatisticsPath(newCfg), desiredPath); err != nil {
			// Not started: starting would create the destination and skip the migration on later runs.
			log.WithError(err).Error("usage: failed to migrate persisted usage statistics; persistence not started")
			return
		}
	}
	persistence := usage.NewPersistence(usage.PersistenceConfig{
		Stats: usage.GetRequestStatistics(), Path: desiredPath, Interval: desiredInterval,
	})
	if err := persistence.Load(); err != nil {
		// Not started: a rejected file must stay as it is. The next config reload retries.
		log.WithError(err).Error("usage: failed to load persisted usage statistics; the file is unchanged and persistence is not started")
		return
	}
	persistence.Start()
	s.usagePersistence = persistence
}

// shutdownUsagePersistence stops periodic flushing and writes a final snapshot. Server
// shutdown calls this so usage recorded since the last periodic flush is not lost. Later
// reloads are ignored.
func (s *Server) shutdownUsagePersistence() {
	if s == nil {
		return
	}
	s.usagePersistenceMu.Lock()
	defer s.usagePersistenceMu.Unlock()
	s.usagePersistenceClosed = true
	s.stopUsagePersistenceLocked()
}

// stopUsagePersistenceLocked stops the current persistence handle. The caller must hold
// usagePersistenceMu.
func (s *Server) stopUsagePersistenceLocked() {
	if s.usagePersistence == nil {
		return
	}
	s.usagePersistence.Stop()
	s.usagePersistence = nil
}
