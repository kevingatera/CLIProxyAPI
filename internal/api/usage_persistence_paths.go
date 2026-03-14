package api

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const usageStatisticsFileName = "usage_stats.json"

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
	if err := os.Rename(legacyPath, desiredPath); err == nil {
		return nil
	}

	src, err := os.Open(legacyPath)
	if err != nil {
		return fmt.Errorf("open legacy usage stats file: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(desiredPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create migrated usage stats file: %w", err)
	}
	copyErr := error(nil)
	if _, err = io.Copy(dst, src); err != nil {
		copyErr = fmt.Errorf("copy usage stats file: %w", err)
	}
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(desiredPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(desiredPath)
		return fmt.Errorf("close migrated usage stats file: %w", closeErr)
	}
	if err := os.Remove(legacyPath); err != nil {
		return fmt.Errorf("remove legacy usage stats file: %w", err)
	}
	return nil
}
