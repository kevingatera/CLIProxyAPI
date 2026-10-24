package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

const usageStatisticsFileName = "usage_stats.json"

// renameLegacyFile is swapped in tests to force the copy fallback.
var renameLegacyFile = os.Rename

func resolveRelativeToConfig(pathValue, configFilePath string) string {
	trimmed := strings.TrimSpace(pathValue)
	if trimmed == "" {
		return ""
	}
	if filepath.IsAbs(trimmed) {
		return trimmed
	}
	base := filepath.Dir(strings.TrimSpace(configFilePath))
	if base == "" || base == "." {
		if wd, err := os.Getwd(); err == nil && wd != "" {
			base = wd
		}
	}
	return filepath.Join(base, trimmed)
}

func resolveLegacyUsageStatisticsPath(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	authDir, err := util.ResolveAuthDir(strings.TrimSpace(cfg.AuthDir))
	if err != nil || authDir == "" {
		return ""
	}
	return filepath.Join(authDir, usageStatisticsFileName)
}

func resolveUsageStatisticsPath(cfg *config.Config, configFilePath string) string {
	if cfg == nil {
		return ""
	}
	if raw := strings.TrimSpace(cfg.UsageStatisticsFile); raw != "" {
		return resolveRelativeToConfig(raw, configFilePath)
	}
	logDir := strings.TrimSpace(logging.ResolveLogDirectory(cfg))
	if logDir == "" {
		return ""
	}
	return filepath.Join(resolveRelativeToConfig(logDir, configFilePath), usageStatisticsFileName)
}

func migrateUsageStatisticsFile(legacyPath, desiredPath string) error {
	legacyPath = strings.TrimSpace(legacyPath)
	desiredPath = strings.TrimSpace(desiredPath)
	if legacyPath == "" || desiredPath == "" || legacyPath == desiredPath {
		return nil
	}
	if _, err := os.Stat(desiredPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat desired usage stats path: %w", err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat legacy usage stats path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(desiredPath), 0o700); err != nil {
		return fmt.Errorf("prepare usage stats directory: %w", err)
	}
	if err := renameLegacyFile(legacyPath, desiredPath); err == nil {
		return nil
	}

	// Rename fails across filesystems. Copy through a synced same-directory temp file
	// and rename it into place. The legacy file is removed only after that commit.
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		return fmt.Errorf("read legacy usage stats file: %w", err)
	}
	if err := usage.WriteFileAtomic(desiredPath, data, 0o600); err != nil {
		return fmt.Errorf("copy legacy usage stats file: %w", err)
	}
	if err := os.Remove(legacyPath); err != nil {
		log.WithError(err).Warn("usage: migrated usage statistics; legacy file could not be removed")
	}
	return nil
}
