package api

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	log "github.com/sirupsen/logrus"
)

// defaultUsageSaveInterval is the periodic snapshot interval when the config leaves it unset.
const defaultUsageSaveInterval = 60 * time.Second

// configureUsagePersistence starts, restarts, or stops usage persistence to match newCfg.
// It runs at startup and on every reload. Persistence needs usage-statistics-enabled and
// usage-statistics-persist; otherwise the aggregate stops collecting.
func (s *Server) configureUsagePersistence(newCfg *config.Config) {
	if s == nil {
		return
	}
	s.usagePersistenceMu.Lock()
	defer s.usagePersistenceMu.Unlock()
	if s.usagePersistenceClosed {
		// Shutdown already flushed and stopped persistence; a late reload must not restart it.
		return
	}

	if newCfg == nil || !newCfg.UsageStatisticsEnabled || !newCfg.UsageStatisticsPersist {
		usage.GetRequestStatistics().SetCollecting(false)
		s.stopUsagePersistenceLocked()
		return
	}

	path := resolveUsageStatisticsPath(newCfg, s.configFilePath)
	interval := defaultUsageSaveInterval
	if newCfg.UsageStatisticsSaveIntervalSeconds > 0 {
		interval = time.Duration(newCfg.UsageStatisticsSaveIntervalSeconds) * time.Second
	}
	key := fmt.Sprintf("%s|%s", path, interval)
	if s.usagePersistence != nil && s.usagePersistenceKey == key {
		return
	}

	s.stopUsagePersistenceLocked()
	if path == "" {
		log.Error("usage: cannot resolve usage-statistics path; persistence not started")
		return
	}
	persistence := usage.NewPersistence(usage.PersistenceConfig{
		Stats: usage.GetRequestStatistics(), Path: path, Interval: interval,
	})
	if err := persistence.Load(); err != nil {
		// Not started: a rejected file must stay as it is. The next config reload retries.
		log.WithError(err).Error("usage: failed to load persisted usage statistics; the file is unchanged and persistence is not started")
		return
	}
	usage.GetRequestStatistics().SetCollecting(true)
	persistence.Start()
	s.usagePersistence = persistence
	s.usagePersistenceKey = key
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

// stopUsagePersistenceLocked stops the current persistence handle and stops collecting.
// The caller must hold usagePersistenceMu.
func (s *Server) stopUsagePersistenceLocked() {
	usage.GetRequestStatistics().SetCollecting(false)
	if s.usagePersistence == nil {
		return
	}
	s.usagePersistence.Stop()
	s.usagePersistence = nil
	s.usagePersistenceKey = ""
}
