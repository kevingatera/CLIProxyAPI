package api

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// defaultUsageSaveInterval is the periodic snapshot interval when the config leaves it unset.
const defaultUsageSaveInterval = 60 * time.Second

// configureUsagePersistence applies newCfg at startup and on every reload.
//
// Path semantics:
//   - Same path and interval: nothing changes.
//   - Interval change: restart on the same path; the aggregate is kept.
//   - Path change: the old file is flushed and stays as it is. The aggregate is reset, and
//     the new file is loaded into it. History is not moved between files.
//   - Disabled: collection stops and the file is flushed. The in-memory aggregate is kept.
//
// A load failure leaves the file untouched and collection off. The next reload retries.
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
	stats := usage.GetRequestStatistics()
	if path != s.usagePersistencePath {
		stats.Reset()
	}
	persistence := usage.NewPersistence(usage.PersistenceConfig{
		Stats: stats, Path: path, Interval: interval,
	})
	if err := persistence.Load(); err != nil {
		// Not started: a rejected file must stay as it is. The next config reload retries.
		log.WithError(err).Error("usage: failed to load persisted usage statistics; the file is unchanged and persistence is not started")
		s.usagePersistencePath = path
		return
	}
	s.usagePersistencePath = path
	stats.SetCollecting(true)
	persistence.Start()
	s.usagePersistence = persistence
	s.usagePersistenceKey = key
}

// shutdownUsagePersistence delivers queued usage, stops collection, and writes a final
// snapshot. Later reloads are ignored.
func (s *Server) shutdownUsagePersistence() {
	if s == nil {
		return
	}
	s.usagePersistenceMu.Lock()
	defer s.usagePersistenceMu.Unlock()
	s.usagePersistenceClosed = true
	s.stopUsagePersistenceLocked()
}

// stopUsagePersistenceLocked runs in a fixed order so the final snapshot is complete:
//  1. Drain the usage queue. New publishers wait, and publishers already admitted are
//     queued first, so every record admitted before this point reaches the aggregate.
//  2. Fence collection off. SetCollecting waits for plugin calls already recording, and
//     no record can land after it returns.
//  3. Stop the persistence handle, which writes the final snapshot.
//
// The caller must hold usagePersistenceMu. Drain never waits on this lock, because
// plugins do not call back into the server.
func (s *Server) stopUsagePersistenceLocked() {
	if s.usagePersistence == nil {
		usage.GetRequestStatistics().SetCollecting(false)
		return
	}
	coreusage.DrainDefault()
	usage.GetRequestStatistics().SetCollecting(false)
	s.usagePersistence.Stop()
	s.usagePersistence = nil
	s.usagePersistenceKey = ""
}
