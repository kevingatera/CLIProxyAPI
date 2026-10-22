package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// API key labels are management state that gives proxy API keys human-readable
// names (e.g. "Cline", "Cursor laptop"). Keys are identified by a client-side
// fingerprint, so the plaintext key never reaches these endpoints.
//
// Wire format: { "api-key-labels": { "<fingerprint>": "<label>", ... } }
// Served at /v8/management/api-key-labels. Reads return an ETag; writes honour
// If-Match exactly like v8 config writes and answer 412 on a stale revision.
//
// Storage: <WRITABLE_PATH or config directory>/management-api-key-labels.json.
// It is deliberately outside the log directory, which rotates and is cleared
// by log cleanup. Back it up with config.yaml. Labels written before this
// location existed are migrated once from the log directory and left in place.

const apiKeyLabelsFileName = "management-api-key-labels.json"

func (h *Handler) apiKeyLabelsPath() string {
	if h == nil {
		return ""
	}
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, apiKeyLabelsFileName)
	}
	if strings.TrimSpace(h.configFilePath) != "" {
		return filepath.Join(filepath.Dir(h.configFilePath), apiKeyLabelsFileName)
	}
	return apiKeyLabelsFileName
}

// legacyAPIKeyLabelsPath is the pre-revision location. It is read only for migration.
func (h *Handler) legacyAPIKeyLabelsPath() string {
	if h == nil {
		return ""
	}
	if logDir := strings.TrimSpace(h.logDirectory()); logDir != "" {
		return filepath.Join(logDir, apiKeyLabelsFileName)
	}
	return ""
}

func (h *Handler) loadAPIKeyLabelsLocked() (map[string]string, error) {
	path := h.apiKeyLabelsPath()
	if strings.TrimSpace(path) == "" {
		return map[string]string{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return h.migrateLegacyAPIKeyLabelsLocked(path)
		}
		return nil, err
	}
	return parseAPIKeyLabels(data)
}

// migrateLegacyAPIKeyLabelsLocked copies labels from the legacy log-directory
// file when the stable file does not exist yet. The legacy file is not deleted,
// so an older build still finds its labels after a rollback.
func (h *Handler) migrateLegacyAPIKeyLabelsLocked(path string) (map[string]string, error) {
	legacy := h.legacyAPIKeyLabelsPath()
	if legacy == "" || filepath.Clean(legacy) == filepath.Clean(path) {
		return map[string]string{}, nil
	}
	data, err := os.ReadFile(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	labels, err := parseAPIKeyLabels(data)
	if err != nil {
		return nil, fmt.Errorf("parse legacy api-key-labels: %w", err)
	}
	if len(labels) == 0 {
		return labels, nil
	}
	if err = h.saveAPIKeyLabelsLocked(labels); err != nil {
		return nil, fmt.Errorf("migrate api-key-labels: %w", err)
	}
	log.Infof("migrated api-key labels to %s", path)
	return labels, nil
}

func parseAPIKeyLabels(data []byte) (map[string]string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return map[string]string{}, nil
	}

	// Accept both the wire envelope {"api-key-labels": {...}} and a bare map.
	var envelope struct {
		Labels map[string]string `json:"api-key-labels"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil && envelope.Labels != nil {
		return normalizeAPIKeyLabels(envelope.Labels), nil
	}

	var bare map[string]string
	if err := json.Unmarshal(trimmed, &bare); err != nil {
		return nil, err
	}
	return normalizeAPIKeyLabels(bare), nil
}

func normalizeAPIKeyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for fingerprint, label := range in {
		fp := strings.TrimSpace(fingerprint)
		l := strings.TrimSpace(label)
		if fp == "" || l == "" {
			continue
		}
		out[fp] = l
	}
	return out
}

// apiKeyLabelsRevision hashes the canonical labels encoding. json.Marshal sorts
// map keys, so the revision depends only on content, not on file formatting.
func apiKeyLabelsRevision(labels map[string]string) (string, error) {
	encoded, err := json.Marshal(labels)
	if err != nil {
		return "", err
	}
	return config.ComputeSourceHash(encoded), nil
}

func (h *Handler) saveAPIKeyLabelsLocked(labels map[string]string) error {
	path := h.apiKeyLabelsPath()
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("api-key-labels path unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	envelope := struct {
		Labels map[string]string `json:"api-key-labels"`
	}{
		Labels: labels,
	}
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "api-key-labels-*.json")
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

// GetAPIKeyLabels returns the persisted api-key-labels map and its ETag.
func (h *Handler) GetAPIKeyLabels(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	labels, err := h.loadAPIKeyLabelsLocked()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	revision, err := apiKeyLabelsRevision(labels)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	c.Header("ETag", configV8ETag(revision))
	c.JSON(http.StatusOK, gin.H{"api-key-labels": labels})
}

// PutAPIKeyLabels replaces the full api-key-labels map. An If-Match that does
// not name the current revision gets 412 and the current ETag, so a stale tab
// cannot overwrite a newer save. Without If-Match the write is unconditional.
func (h *Handler) PutAPIKeyLabels(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed_to_read_body"})
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}

	// Accept both the envelope and a bare map.
	labels := map[string]string{}
	var envelope struct {
		Labels map[string]string `json:"api-key-labels"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil && envelope.Labels != nil {
		labels = normalizeAPIKeyLabels(envelope.Labels)
	} else {
		var bare map[string]string
		if err := json.Unmarshal(trimmed, &bare); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": err.Error()})
			return
		}
		labels = normalizeAPIKeyLabels(bare)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	current, err := h.loadAPIKeyLabelsLocked()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	currentRevision, err := apiKeyLabelsRevision(current)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	if !ifMatchSatisfied(c.Request.Header.Values("If-Match"), currentRevision) {
		c.Header("ETag", configV8ETag(currentRevision))
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "precondition_failed"})
		return
	}
	if err := h.saveAPIKeyLabelsLocked(labels); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	revision, err := apiKeyLabelsRevision(labels)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	c.Header("ETag", configV8ETag(revision))
	c.JSON(http.StatusOK, gin.H{"ok": true, "api-key-labels": labels, "changed": []string{"api-key-labels"}})
}
