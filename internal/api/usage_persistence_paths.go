package api

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// usageStatisticsFileName is the default snapshot name, placed next to the config file.
const usageStatisticsFileName = "usage_stats.json"

// resolveRelativeToConfig resolves pathValue against the config file directory, or the
// working directory when the config path is unknown.
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

// resolveUsageStatisticsPath returns usage-statistics-file when set, otherwise
// usage_stats.json beside the config file.
func resolveUsageStatisticsPath(cfg *config.Config, configFilePath string) string {
	if cfg == nil {
		return ""
	}
	if raw := strings.TrimSpace(cfg.UsageStatisticsFile); raw != "" {
		return resolveRelativeToConfig(raw, configFilePath)
	}
	return resolveRelativeToConfig(usageStatisticsFileName, configFilePath)
}
