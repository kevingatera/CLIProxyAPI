package management

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// API key labels give client API keys human-readable names such as "Team laptop".
// Keys are addressed by a fingerprint computed by the caller, so raw keys never
// reach these endpoints.
//
// Fingerprint syntax: "key:sha256:" followed by exactly 16 lowercase hex digits,
// the first half of SHA-256 over the raw key bytes. The server checks only this
// syntax. It cannot prove that a value is a hash, so a client that sends a
// fingerprint-shaped raw key would still store it.
//
// Wire format: { "api-key-labels": { "<fingerprint>": "<label>", ... } }. A bare map
// of the same shape is also accepted. A PUT body must be a JSON object: empty
// input, null, and non-object JSON get 400. {} clears every label on purpose.
// Blank labels are dropped, which removes that key's label.
//
// Served at /v8/management/api-key-labels. Reads return an ETag. Writes honour
// If-Match exactly like v8 config writes and answer 412 on a stale revision.
//
// Storage: <WRITABLE_PATH, or the config directory>/management-api-key-labels.json,
// written with mode 0600 through a temp file and rename. Back it up with config.yaml.
// Requests that share a labels file serialize on a lock keyed by its cleaned
// absolute path, so independent handlers in one process cannot both pass If-Match.
// The lock does not span processes. Run one server process per labels file; two
// processes can each pass If-Match and lose an update.

const (
	apiKeyLabelsFileName    = "management-api-key-labels.json"
	apiKeyLabelsJSONKey     = "api-key-labels"
	apiKeyFingerprintPrefix = "key:sha256:"
	apiKeyFingerprintDigits = 16
)

var errInvalidAPIKeyFingerprint = errors.New("invalid fingerprint: expected key:sha256: followed by 16 lowercase hex digits")

// apiKeyLabelsLocks maps a cleaned absolute labels path to its lock. One entry
// exists per path, so the map stays small.
var apiKeyLabelsLocks = struct {
	mu     sync.Mutex
	byPath map[string]*sync.Mutex
}{byPath: make(map[string]*sync.Mutex)}

// lockAPIKeyLabelsPath locks the labels file at path and returns the unlock func.
func lockAPIKeyLabelsPath(path string) func() {
	key := filepath.Clean(path)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	apiKeyLabelsLocks.mu.Lock()
	lock, ok := apiKeyLabelsLocks.byPath[key]
	if !ok {
		lock = &sync.Mutex{}
		apiKeyLabelsLocks.byPath[key] = lock
	}
	apiKeyLabelsLocks.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

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

// validAPIKeyFingerprint reports whether fingerprint matches the documented syntax.
// It never echoes its input, so a raw key cannot leak through an error message.
func validAPIKeyFingerprint(fingerprint string) bool {
	digest, ok := strings.CutPrefix(fingerprint, apiKeyFingerprintPrefix)
	if !ok || len(digest) != apiKeyFingerprintDigits {
		return false
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// loadAPIKeyLabelsLocked returns an empty map when the file does not exist yet.
// An empty, null, or otherwise invalid file is an error, not an empty label set.
func (h *Handler) loadAPIKeyLabelsLocked() (map[string]string, error) {
	data, err := os.ReadFile(h.apiKeyLabelsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	labels, errParse := parseAPIKeyLabels(data)
	if errParse != nil {
		return nil, fmt.Errorf("parse api-key-labels file: %w", errParse)
	}
	return labels, nil
}

// parseAPIKeyLabels decodes the envelope or a bare fingerprint-to-label map.
func parseAPIKeyLabels(data []byte) (map[string]string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("empty document")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("expected a JSON object")
	}
	if raw, ok := fields[apiKeyLabelsJSONKey]; ok {
		var labels map[string]string
		if err := json.Unmarshal(raw, &labels); err != nil || labels == nil {
			return nil, errors.New("api-key-labels must be a JSON object")
		}
		return normalizeAPIKeyLabels(labels)
	}
	var bare map[string]string
	if err := json.Unmarshal(trimmed, &bare); err != nil {
		return nil, err
	}
	return normalizeAPIKeyLabels(bare)
}

// normalizeAPIKeyLabels trims fingerprints, rejects any that break the syntax,
// and drops entries whose label is blank.
func normalizeAPIKeyLabels(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for fingerprint, label := range in {
		fp := strings.TrimSpace(fingerprint)
		if !validAPIKeyFingerprint(fp) {
			return nil, errInvalidAPIKeyFingerprint
		}
		if l := strings.TrimSpace(label); l != "" {
			out[fp] = l
		}
	}
	return out, nil
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
	committed := false
	defer func() {
		if committed {
			return
		}
		if errRemove := os.Remove(tmpPath); errRemove != nil && !os.IsNotExist(errRemove) {
			log.Warnf("remove unused api-key-labels temp file: %v", errRemove)
		}
	}()
	if _, err = tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// GetAPIKeyLabels returns the api-key-labels map and its ETag.
func (h *Handler) GetAPIKeyLabels(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	unlock := lockAPIKeyLabelsPath(h.apiKeyLabelsPath())
	defer unlock()
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "body must be a JSON object of fingerprint to label"})
		return
	}

	unlock := lockAPIKeyLabelsPath(h.apiKeyLabelsPath())
	defer unlock()
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
