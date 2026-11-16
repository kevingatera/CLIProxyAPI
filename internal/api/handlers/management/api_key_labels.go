package management

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// API key labels give client API keys human-readable names such as "Team laptop".
// Keys are identified by a fingerprint computed by the caller, so raw keys never
// reach these endpoints.
//
// Fingerprint format: "key:sha256:" followed by the first 16 lowercase hex digits
// of SHA-256 over the raw key bytes. The server stores any non-empty trimmed
// fingerprint string as given; the format is a client contract.
//
// Wire format: { "api-key-labels": { "<fingerprint>": "<label>", ... } }
// Served at /v8/management/api-key-labels. Reads return an ETag; writes honour
// If-Match exactly like v8 config writes and answer 412 on a stale revision.
//
// Storage: <WRITABLE_PATH, or the config directory>/management-api-key-labels.json,
// written with mode 0600 through a temp file and rename. Back it up with config.yaml.

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

func (h *Handler) loadAPIKeyLabelsLocked() (map[string]string, error) {
	data, err := os.ReadFile(h.apiKeyLabelsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return parseAPIKeyLabels(data)
}

// parseAPIKeyLabels accepts the wire envelope and a bare fingerprint-to-label map.
func parseAPIKeyLabels(data []byte) (map[string]string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return map[string]string{}, nil
	}

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

// normalizeAPIKeyLabels trims both sides and drops entries with an empty fingerprint or label.
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

// apiKeyLabelsRevision hashes the canonical labels encoding. Map keys are sorted
// by the encoder, so the revision depends on content only, not file formatting.
func apiKeyLabelsRevision(labels map[string]string) (string, error) {
	encoded, err := json.Marshal(labels)
	if err != nil {
		return "", err
	}
	return config.ComputeSourceHash(encoded), nil
}

func (h *Handler) saveAPIKeyLabelsLocked(labels map[string]string) error {
	path := h.apiKeyLabelsPath()
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
	// os.CreateTemp creates the file with mode 0600, and rename keeps that mode.
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

// GetAPIKeyLabels returns the api-key-labels map and its ETag.
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

// PutAPIKeyLabels replaces the full api-key-labels map. When If-Match is present
// and does not name the current revision, it answers 412 with the current ETag,
// so a stale client cannot overwrite a newer save. Without If-Match the write is
// unconditional.
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
	labels, err := parseAPIKeyLabels(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": err.Error()})
		return
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
