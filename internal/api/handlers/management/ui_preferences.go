package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

const uiPreferencesFileName = "management-ui-preferences.json"

func (h *Handler) uiPreferencesPath() string {
	if h == nil {
		return ""
	}
	if logDir := strings.TrimSpace(h.logDirectory()); logDir != "" {
		return filepath.Join(logDir, uiPreferencesFileName)
	}
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, uiPreferencesFileName)
	}
	if strings.TrimSpace(h.configFilePath) != "" {
		return filepath.Join(filepath.Dir(h.configFilePath), uiPreferencesFileName)
	}
	return uiPreferencesFileName

}

func (h *Handler) loadUIPreferencesLocked() (map[string]json.RawMessage, error) {
	path := h.uiPreferencesPath()
	if strings.TrimSpace(path) == "" {
		return map[string]json.RawMessage{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	items := map[string]json.RawMessage{}
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (h *Handler) saveUIPreferencesLocked(items map[string]json.RawMessage) error {
	path := h.uiPreferencesPath()
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("ui preferences path unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "ui-preferences-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err = tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

func (h *Handler) GetUIPreference(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		c.JSON(400, gin.H{"error": "missing key"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	items, err := h.loadUIPreferencesLocked()
	if err != nil {
		c.JSON(500, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	raw, ok := items[key]
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		c.Header("Content-Type", "application/json; charset=utf-8")
		_, _ = c.Writer.Write([]byte("{}"))
		return
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	_, _ = c.Writer.Write(raw)
}

func (h *Handler) PutUIPreference(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		c.JSON(400, gin.H{"error": "missing key"})
		return
	}
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	if !json.Valid(trimmed) {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	compact := &bytes.Buffer{}
	if err = json.Compact(compact, trimmed); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	items, err := h.loadUIPreferencesLocked()
	if err != nil {
		c.JSON(500, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	items[key] = compact.Bytes()
	if err = h.saveUIPreferencesLocked(items); err != nil {
		c.JSON(500, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "changed": []string{"ui-preferences", key}})
}

func (h *Handler) DeleteUIPreference(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		c.JSON(400, gin.H{"error": "missing key"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	items, err := h.loadUIPreferencesLocked()
	if err != nil {
		c.JSON(500, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	delete(items, key)
	if err = h.saveUIPreferencesLocked(items); err != nil {
		c.JSON(500, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "changed": []string{"ui-preferences", key}})
}
