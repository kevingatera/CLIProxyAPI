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

// API key labels are a homelab-only management UI feature that lets operators
// assign human-readable names (e.g. "Cline", "Cursor laptop") to proxy API keys
// (which are otherwise opaque bearer tokens). The labels are persisted to a
// JSON file alongside the other management state and surfaced via the
// /api-key-labels management endpoints.
//
// Wire format: { "api-key-labels": { "<fingerprint>": "<label>", ... } }
// The fingerprint is a short, stable hash of the key (never the key itself),
// computed client-side so the plaintext key never reaches this endpoint.

const apiKeyLabelsFileName = "management-api-key-labels.json"

func (h *Handler) apiKeyLabelsPath() string {
	if h == nil {
		return ""
	}
	if logDir := strings.TrimSpace(h.logDirectory()); logDir != "" {
		return filepath.Join(logDir, apiKeyLabelsFileName)
	}
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, apiKeyLabelsFileName)
	}
	if strings.TrimSpace(h.configFilePath) != "" {
		return filepath.Join(filepath.Dir(h.configFilePath), apiKeyLabelsFileName)
	}
	return apiKeyLabelsFileName
}

func (h *Handler) loadAPIKeyLabelsLocked() (map[string]string, error) {
	path := h.apiKeyLabelsPath()
	if strings.TrimSpace(path) == "" {
		return map[string]string{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
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

// GetAPIKeyLabels returns the persisted api-key-labels map.
func (h *Handler) GetAPIKeyLabels(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	labels, err := h.loadAPIKeyLabelsLocked()
	if err != nil {
		c.JSON(500, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	c.JSON(200, gin.H{"api-key-labels": labels})
}

// PutAPIKeyLabels replaces the full api-key-labels map.
func (h *Handler) PutAPIKeyLabels(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed_to_read_body"})
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
			c.JSON(400, gin.H{"error": "invalid_body", "message": err.Error()})
			return
		}
		labels = normalizeAPIKeyLabels(bare)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.saveAPIKeyLabelsLocked(labels); err != nil {
		c.JSON(500, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "api-key-labels": labels, "changed": []string{"api-key-labels"}})
}
