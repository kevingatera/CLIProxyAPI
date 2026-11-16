package logging

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	log "github.com/sirupsen/logrus"
)

const (
	defaultArchiveMaxSizeGB        = 30
	defaultArchiveCaptureMB        = 32
	defaultArchiveConcurrentCaps   = 16
	archiveMinSecretFragmentLength = 1
	// archiveMinPrefixFragmentLength keeps ordinary one-letter text from being trimmed.
	archiveMinPrefixFragmentLength = 2
	archiveTruncatedMarker         = "[ARCHIVE TRUNCATED: per-request capture limit reached]"
	archiveConcurrencyMarker       = "[ARCHIVE OMITTED: concurrent capture limit reached]"
	archiveMetadataOnlyMarker      = "[ARCHIVE OMITTED: metadata-only mode]"
	archiveDecompressedMarker      = "[ARCHIVE TRUNCATED: decompressed body reached capture limit]"
	// archiveDirMarkerFile marks a directory the archive created. Retention only
	// runs in such a directory, so unrelated logs are never deleted.
	archiveDirMarkerFile = ".cpa-request-archive"
	// archiveCaptureContextKey stores the per-request ArchiveCaptureBudget on the Gin context.
	archiveCaptureContextKey = "ARCHIVE_CAPTURE_BUDGET"
	// archiveCredentialTailWindow bounds how far back a cut looks for a partial credential prefix.
	archiveCredentialTailWindow = 512
	// archiveDecompressWindowBytes bounds the zstd window a decompressed archive body may declare.
	archiveDecompressWindowBytes = 8 << 20
	// archiveTokenDelimiters end a credential token; a cut token with none of them is still open.
	archiveTokenDelimiters = " \t\r\n\"',;&(){}[]<>\\"
)

// archiveBoundaryMarkers are the markers that end a cut. Text before one of them
// is trimmed of partial credentials, because the cut may split a secret.
var archiveBoundaryMarkers = []string{archiveTruncatedMarker, archiveConcurrencyMarker, archiveMetadataOnlyMarker, archiveDecompressedMarker}

// archiveCredentialPrefixes start recognizable credentials. A cut that leaves one
// of these followed by an open token is trimmed, even when the secret is unknown.
var archiveCredentialPrefixes = []string{"sk-", "sk_", "ghp_", "gho_", "ghs_", "ghu_", "ghr_", "github_pat_", "AIza", "xox", "eyJ", "Bearer ", "Basic "}

// archiveDecompressAttempts counts decompressions started by the archive. Tests
// use it to prove metadata-only mode never decompresses.
var archiveDecompressAttempts atomic.Int64

// RequestArchive sanitizes before delegating to disk storage. It intentionally
// does not expose FileBodySource: upstream sections stay in memory until they
// can be redacted, so unmasked credentials never enter temporary log files.
type RequestArchive struct {
	file        *FileRequestLogger
	cfg         atomic.Pointer[config.Config]
	enabled     atomic.Bool
	secretsMu   sync.RWMutex
	secrets     func() []string
	retentionMu sync.Mutex
	startupErr  error
	// activeCaptures counts requests that hold a capture slot.
	activeCaptures atomic.Int64
	// begunMu guards begun, the captures started by the middleware and keyed by request ID.
	begunMu sync.Mutex
	begun   map[string]*ArchiveCaptureBudget
	// clock is replaceable in tests so age retention needs no sleeps.
	clock func() time.Time
}

// NewRequestArchive creates the archive under request-archive.directory, or under
// requests/ in the logs directory when unset. Relative paths resolve from configDir.
// The directory must be empty or already marked as an archive directory.
func NewRequestArchive(cfg *config.Config, configDir, logsDir string) *RequestArchive {
	dir := cfg.RequestArchive.Directory
	if dir == "" {
		dir = filepath.Join(logsDir, "requests")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(configDir, dir)
	}
	a := &RequestArchive{file: NewFileRequestLogger(true, dir, "", 0), clock: time.Now}
	a.startupErr = prepareArchiveDir(dir)
	if a.startupErr != nil {
		log.WithError(a.startupErr).Error("request archive disabled")
	}
	a.SetConfig(cfg)
	if a.startupErr == nil {
		a.startupErr = a.retain()
	}
	return a
}

// prepareArchiveDir creates the archive directory and its marker file. An existing
// directory without the marker is refused, so retention cannot reach foreign files.
func prepareArchiveDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	marker := filepath.Join(dir, archiveDirMarkerFile)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("request archive directory %s is not empty and is not an archive directory; use an empty directory", dir)
	}
	return os.WriteFile(marker, []byte("CLIProxyAPI request archive. Retention only deletes log files in this directory.\n"), 0600)
}

// SetConfig applies the live config. The archive records when either request-log
// or request-archive is on.
func (a *RequestArchive) SetConfig(cfg *config.Config) {
	a.cfg.Store(cfg)
	a.enabled.Store(cfg.RequestLog || cfg.RequestArchive.Enabled)
}

// SetEnabled overrides the enabled state.
func (a *RequestArchive) SetEnabled(enabled bool) { a.enabled.Store(enabled) }

// IsEnabled reports whether the archive currently records traffic.
func (a *RequestArchive) IsEnabled() bool { return a.enabled.Load() }

// LosslessStreaming tells the response writer to queue every chunk. Dropping
// chunks would leave gaps in the archived transcript.
func (a *RequestArchive) LosslessStreaming() bool { return true }

// SetSecretProvider registers a source of runtime credentials, such as OAuth tokens.
func (a *RequestArchive) SetSecretProvider(provider func() []string) {
	a.secretsMu.Lock()
	a.secrets = provider
	a.secretsMu.Unlock()
}

// ArchiveCaptureBudget is the admission for one archived HTTP request. Executor
// helpers charge upstream bodies against it before they are buffered. The
// streaming writer takes its own per-request budget from the same admission.
type ArchiveCaptureBudget struct {
	archive      *RequestArchive
	id           string
	admitted     bool
	metadataOnly bool
	marker       string
	streamCap    int64

	mu       sync.Mutex
	source   archiveBudget
	released bool
	// cutReported records that the marker was written, so later bodies add nothing.
	cutReported bool
}

// BeginCapture admits one request to the archive. Requests over max-concurrent-captures
// get an omission marker instead of bodies. The caller must call Release.
func (a *RequestArchive) BeginCapture(requestID string) *ArchiveCaptureBudget {
	cfg := a.cfg.Load()
	b := &ArchiveCaptureBudget{archive: a, id: requestID, marker: archiveTruncatedMarker}
	b.admitted = a.acquireCapture(concurrentCaptureLimit(cfg))
	b.metadataOnly = cfg.RequestArchive.MetadataOnly
	switch {
	case !b.admitted:
		b.marker = archiveConcurrencyMarker
	case b.metadataOnly:
		b.marker = archiveMetadataOnlyMarker
	default:
		b.streamCap = captureBudgetBytes(cfg)
	}
	b.source = archiveBudget{remaining: b.streamCap}
	if requestID != "" {
		a.begunMu.Lock()
		if a.begun == nil {
			a.begun = make(map[string]*ArchiveCaptureBudget)
		}
		a.begun[requestID] = b
		a.begunMu.Unlock()
	}
	return b
}

// Release ends the request's admission and frees its capture slot. It is idempotent.
func (b *ArchiveCaptureBudget) Release() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.released {
		b.mu.Unlock()
		return
	}
	b.released = true
	b.mu.Unlock()
	a := b.archive
	a.begunMu.Lock()
	if b.id != "" && a.begun[b.id] == b {
		delete(a.begun, b.id)
	}
	a.begunMu.Unlock()
	if b.admitted {
		a.activeCaptures.Add(-1)
	}
}

// admit charges an upstream body against the request budget. A cut appends the
// omission marker; the archive trims any partial credential before the cut.
func (b *ArchiveCaptureBudget) admit(data []byte) []byte {
	if b == nil || len(data) == 0 {
		return data
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	kept, cut := b.source.take(data)
	if !cut {
		return kept
	}
	if b.cutReported {
		return nil
	}
	b.cutReported = true
	out := make([]byte, 0, len(kept)+len(b.marker))
	out = append(out, kept...)
	return append(out, b.marker...)
}

// AttachArchiveCapture binds a request's capture budget to its Gin context so the
// executor helpers can admit bodies before they reach memory.
func AttachArchiveCapture(c *gin.Context, budget *ArchiveCaptureBudget) {
	if c == nil || budget == nil {
		return
	}
	c.Set(archiveCaptureContextKey, budget)
}

// AdmitArchiveCapture bounds one upstream body before the executor helpers buffer
// it. Without an archive budget on the context, data is returned unchanged.
func AdmitArchiveCapture(c *gin.Context, data []byte) []byte {
	if c == nil || len(data) == 0 {
		return data
	}
	value, ok := c.Get(archiveCaptureContextKey)
	if !ok {
		return data
	}
	budget, ok := value.(*ArchiveCaptureBudget)
	if !ok {
		return data
	}
	return budget.admit(data)
}

func (a *RequestArchive) begunCapture(requestID string) *ArchiveCaptureBudget {
	if requestID == "" {
		return nil
	}
	a.begunMu.Lock()
	defer a.begunMu.Unlock()
	return a.begun[requestID]
}

func (a *RequestArchive) redactor(headers map[string][]string) *ArchiveRedactor {
	cfg := a.cfg.Load()
	secrets := ArchiveSecrets(cfg)
	if cfg != nil {
		secrets = append(secrets, cfg.RemoteManagement.SecretKey)
	}
	a.secretsMu.RLock()
	if a.secrets != nil {
		secrets = append(secrets, a.secrets()...)
	}
	a.secretsMu.RUnlock()
	for name, values := range headers {
		if archiveSensitiveName(name) {
			for _, value := range values {
				secrets = append(secrets, value)
				if bytes.EqualFold([]byte(name), []byte("Cookie")) || bytes.EqualFold([]byte(name), []byte("Set-Cookie")) {
					for _, part := range strings.Split(value, ";") {
						if _, credential, ok := strings.Cut(part, "="); ok {
							secrets = append(secrets, strings.TrimSpace(credential))
						}
						if bytes.EqualFold([]byte(name), []byte("Set-Cookie")) {
							break
						}
					}
				}
				if len(value) > 7 && bytes.EqualFold([]byte(value[:7]), []byte("Bearer ")) {
					secrets = append(secrets, value[7:])
				}
			}
		}
	}
	return NewArchiveRedactor(secrets)
}

func (a *RequestArchive) retain() error {
	a.retentionMu.Lock()
	defer a.retentionMu.Unlock()
	return a.retainLocked()
}

func (a *RequestArchive) retainLocked() error {
	cfg := a.cfg.Load()
	if days := cfg.RequestArchive.MaxAgeDays; days > 0 {
		cutoff := a.clock().Add(-time.Duration(days) * 24 * time.Hour)
		if _, err := removeArchiveFilesBefore(a.file.logsDir, cutoff); err != nil {
			return err
		}
	}
	gb := cfg.RequestArchive.MaxSizeGB
	if gb <= 0 {
		gb = defaultArchiveMaxSizeGB
	}
	_, err := enforceLogDirSizeLimit(a.file.logsDir, int64(gb)*1000000000, "")
	return err
}

// removeArchiveFilesBefore deletes completed archive files last modified before
// cutoff. Temporary captures and non-log files are never matched.
func removeArchiveFilesBefore(dir string, cutoff time.Time) (int, error) {
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return 0, nil
		}
		return 0, errRead
	}
	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() || !isLogFileName(entry.Name()) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if errRemove := os.Remove(path); errRemove != nil {
			log.WithError(errRemove).Warnf("logging: failed to remove expired archive file: %s", entry.Name())
			continue
		}
		deleted++
	}
	return deleted, nil
}

// archiveBudget bounds the raw bytes one request may hold in memory.
type archiveBudget struct {
	remaining int64
}

// take returns the prefix of data that still fits and reports whether data was cut.
func (b *archiveBudget) take(data []byte) ([]byte, bool) {
	if int64(len(data)) <= b.remaining {
		b.remaining -= int64(len(data))
		return data, false
	}
	kept := data[:b.remaining]
	b.remaining = 0
	return kept, true
}

// admitRedacted charges masked bytes to the budget. Text before any cut marker is
// trimmed of partial credentials, then a cut keeps the prefix and appends the marker.
func admitRedacted(b *archiveBudget, data []byte, secrets []string, marker string) []byte {
	data = trimBeforeBoundaryMarkers(data, secrets)
	kept, cut := b.take(data)
	if !cut {
		return kept
	}
	return markCut(kept, secrets, marker)
}

func markCut(data []byte, secrets []string, marker string) []byte {
	trimmed := trimSecretFragment(data, secrets)
	out := make([]byte, 0, len(trimmed)+len(marker))
	out = append(out, trimmed...)
	return append(out, marker...)
}

// trimBeforeBoundaryMarkers trims the text before every cut marker, so a cut that
// landed inside a credential leaves no partial credential behind.
func trimBeforeBoundaryMarkers(data []byte, secrets []string) []byte {
	var out []byte
	found := false
	rest := data
	for {
		idx, marker := firstBoundaryMarker(rest)
		if idx < 0 {
			if !found {
				return data
			}
			return append(out, rest...)
		}
		found = true
		out = append(out, trimSecretFragment(rest[:idx], secrets)...)
		out = append(out, marker...)
		rest = rest[idx+len(marker):]
	}
}

func firstBoundaryMarker(data []byte) (int, string) {
	best, bestMarker := -1, ""
	for _, marker := range archiveBoundaryMarkers {
		if idx := bytes.Index(data, []byte(marker)); idx >= 0 && (best < 0 || idx < best) {
			best, bestMarker = idx, marker
		}
	}
	return best, bestMarker
}

// trimSecretFragment drops a tail that could begin a known secret or a recognizable
// credential. Any proper prefix counts, down to one byte, because a cut can land
// anywhere. A credential prefix followed by an open token is also dropped.
func trimSecretFragment(data []byte, secrets []string) []byte {
	cut := len(data)
	trimTail := func(candidate string, minLen int) {
		for k := min(len(candidate)-1, len(data)); k >= minLen; k-- {
			if bytes.HasSuffix(data, []byte(candidate[:k])) {
				if len(data)-k < cut {
					cut = len(data) - k
				}
				break
			}
		}
	}
	for _, secret := range secrets {
		trimTail(secret, archiveMinSecretFragmentLength)
	}
	for _, prefix := range archiveCredentialPrefixes {
		trimTail(prefix, archiveMinPrefixFragmentLength)
	}
	windowStart := max(0, len(data)-archiveCredentialTailWindow)
	window := data[windowStart:]
	for _, prefix := range archiveCredentialPrefixes {
		i := bytes.LastIndex(window, []byte(prefix))
		if i < 0 {
			continue
		}
		rest := window[i+len(prefix):]
		if !bytes.ContainsAny(rest, archiveTokenDelimiters) && windowStart+i < cut {
			cut = windowStart + i
		}
	}
	return data[:cut]
}

// captureBudgetBytes returns the per-request in-memory cap. Zero or unset uses the default.
func captureBudgetBytes(cfg *config.Config) int64 {
	mb := cfg.RequestArchive.MaxCaptureMB
	if mb <= 0 {
		mb = defaultArchiveCaptureMB
	}
	return int64(mb) << 20
}

func concurrentCaptureLimit(cfg *config.Config) int64 {
	if n := cfg.RequestArchive.MaxConcurrentCaptures; n > 0 {
		return int64(n)
	}
	return defaultArchiveConcurrentCaps
}

// acquireCapture reserves a capture slot. It returns false when the concurrent
// limit is reached; the caller then records an omission marker only.
func (a *RequestArchive) acquireCapture(limit int64) bool {
	for {
		n := a.activeCaptures.Load()
		if n >= limit {
			return false
		}
		if a.activeCaptures.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// LogRequest records a complete non-streaming request and response.
func (a *RequestArchive) LogRequest(url, method string, headers map[string][]string, body []byte, status int, responseHeaders map[string][]string, response, ws, upstreamRequest, upstreamResponse, upstreamWS []byte, apiErrors []*interfaces.ErrorMessage, id string, started, received time.Time) error {
	return a.LogRequestWithOptions(url, method, headers, body, status, responseHeaders, response, ws, upstreamRequest, upstreamResponse, upstreamWS, apiErrors, false, id, started, received)
}

// LogRequestWithOptions is LogRequest with force, which records even when the archive is off.
func (a *RequestArchive) LogRequestWithOptions(url, method string, headers map[string][]string, body []byte, status int, responseHeaders map[string][]string, response, ws, upstreamRequest, upstreamResponse, upstreamWS []byte, apiErrors []*interfaces.ErrorMessage, force bool, id string, started, received time.Time) error {
	if !a.IsEnabled() && !force {
		return nil
	}
	if a.startupErr != nil {
		return a.startupErr
	}
	cfg := a.cfg.Load()
	combinedHeaders := make(map[string][]string)
	for name, values := range headers {
		combinedHeaders[name] = append(combinedHeaders[name], values...)
	}
	for name, values := range responseHeaders {
		combinedHeaders[name] = append(combinedHeaders[name], values...)
	}
	r := a.redactor(combinedHeaders)
	// Each body section is bounded by the request's limit. Metadata-only and
	// over-limit requests get no decompression, so no unbounded work happens.
	bodyLimit, marker := captureBudgetBytes(cfg), archiveTruncatedMarker
	if capture := a.begunCapture(id); capture != nil {
		bodyLimit, marker = capture.streamCap, capture.marker
	}
	decoded := response
	if !cfg.RequestArchive.MetadataOnly && bodyLimit > 0 {
		var errDecode error
		decoded, errDecode = decompressArchiveBody(responseHeaders, response, bodyLimit)
		if errDecode != nil {
			decoded = []byte("[COMPRESSED PAYLOAD OMITTED: DECOMPRESSION FAILED]")
		}
	}
	safeHeaders := r.Headers(responseHeaders)
	for name := range safeHeaders {
		if bytes.EqualFold([]byte(name), []byte("Content-Encoding")) {
			delete(safeHeaders, name)
		}
	}
	// Error structures may carry arbitrary upstream text; serialize and redact
	// them rather than passing pointers to the raw errors to the file writer.
	if len(apiErrors) > 0 {
		safeErrors := make([]map[string]any, 0, len(apiErrors))
		for _, item := range apiErrors {
			if item == nil {
				continue
			}
			message := ""
			if item.Error != nil {
				message = string(r.Bytes([]byte(item.Error.Error())))
			}
			safeErrors = append(safeErrors, map[string]any{"status": item.StatusCode, "error": message, "body": string(r.Bytes(item.Body)), "headers": r.Headers(item.Headers), "addon": r.Headers(item.Addon)})
		}
		errorsJSON, _ := json.Marshal(safeErrors)
		upstreamResponse = append(append(bytes.Clone(upstreamResponse), '\n'), errorsJSON...)
	}
	budget := archiveBudget{remaining: bodyLimit}
	metadataOnly := cfg.RequestArchive.MetadataOnly
	bounded := func(data []byte) []byte {
		if len(data) == 0 {
			return data
		}
		if metadataOnly {
			return []byte(archiveMetadataOnlyMarker)
		}
		return admitRedacted(&budget, data, r.secrets, marker)
	}
	a.retentionMu.Lock()
	defer a.retentionMu.Unlock()
	err := a.file.LogRequest(string(r.Bytes([]byte(url))), method, r.Headers(headers), bounded(r.Bytes(body)), status, safeHeaders, bounded(r.Transcript(decoded)), bounded(r.Transcript(ws)), bounded(r.Bytes(upstreamRequest)), bounded(r.Transcript(upstreamResponse)), bounded(r.Transcript(upstreamWS)), nil, string(r.Bytes([]byte(id))), started, received)
	if err != nil {
		return err
	}
	return a.retainLocked()
}

// decompressArchiveBody decodes a compressed non-streaming body for masking. Output
// is capped at limit bytes, so an expansion bomb cannot exhaust memory.
func decompressArchiveBody(headers map[string][]string, body []byte, limit int64) ([]byte, error) {
	encoding := archiveContentEncoding(headers)
	if encoding == "" || encoding == "identity" || len(body) == 0 {
		return body, nil
	}
	archiveDecompressAttempts.Add(1)
	var reader io.Reader
	var closer func()
	switch encoding {
	case "gzip":
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		reader, closer = gr, func() { _ = gr.Close() }
	case "deflate":
		fr := flate.NewReader(bytes.NewReader(body))
		reader, closer = fr, func() { _ = fr.Close() }
	case "br":
		reader = brotli.NewReader(bytes.NewReader(body))
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(archiveDecompressWindowBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create zstd reader: %w", err)
		}
		reader, closer = zr, zr.Close
	default:
		return body, nil
	}
	if closer != nil {
		defer closer()
	}
	out, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to decompress %s body: %w", encoding, err)
	}
	if int64(len(out)) > limit {
		out = append(out[:limit:limit], archiveDecompressedMarker...)
	}
	return out, nil
}

func archiveContentEncoding(headers map[string][]string) string {
	for key, values := range headers {
		if strings.EqualFold(key, "Content-Encoding") && len(values) > 0 {
			return strings.ToLower(strings.TrimSpace(values[0]))
		}
	}
	return ""
}

// LogStreamingRequest starts a streaming capture. Chunks stay in memory, masked
// and bounded, until the stream closes.
func (a *RequestArchive) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, id string) (StreamingLogWriter, error) {
	if !a.IsEnabled() {
		return &NoOpStreamingLogWriter{}, nil
	}
	if a.startupErr != nil {
		return nil, a.startupErr
	}
	r := a.redactor(headers)
	capture := a.begunCapture(id)
	owned := capture == nil
	if owned {
		capture = a.BeginCapture(id)
	}
	stream := &archiveStream{
		redactor:     r,
		archive:      a,
		capture:      capture,
		owned:        owned,
		counted:      capture.admitted,
		metadataOnly: capture.metadataOnly,
		budget:       archiveBudget{remaining: capture.streamCap},
		limitMarker:  capture.marker,
	}
	if !capture.admitted {
		// Over the concurrent limit the request is still recorded, without transcript bytes.
		stream.pendingCut = true
	}
	// The file writer persists the body as soon as it is constructed, so the body is
	// admitted under the same rules as later sections before that call.
	admittedBody := stream.admitRequestBody(body)
	w, err := a.file.LogStreamingRequest(string(r.Bytes([]byte(url))), method, r.Headers(headers), admittedBody, string(r.Bytes([]byte(id))))
	if err != nil {
		stream.release()
		return nil, err
	}
	if file, ok := w.(*FileStreamingLogWriter); ok {
		file.lossless = true
	}
	stream.StreamingLogWriter = w
	return stream, nil
}

type archiveStream struct {
	StreamingLogWriter
	redactor *ArchiveRedactor
	archive  *RequestArchive
	capture  *ArchiveCaptureBudget
	// owned means LogStreamingRequest began the capture, so the stream releases it.
	owned        bool
	pending      []byte
	budget       archiveBudget
	pendingCut   bool
	counted      bool
	metadataOnly bool
	limitMarker  string
	finished     bool
}

// WriteChunkAsync keeps raw chunks in memory until Close. SSE deltas can split a
// credential across records, so masking needs the whole transcript.
func (w *archiveStream) WriteChunkAsync(chunk []byte) {
	kept, cut := w.budget.take(chunk)
	w.pending = append(w.pending, kept...)
	if cut {
		w.pendingCut = true
	}
}

func (w *archiveStream) WriteStatus(status int, headers map[string][]string) error {
	w.redactor = NewArchiveRedactor(append(w.redactor.secrets, w.archive.redactor(headers).secrets...))
	return w.StreamingLogWriter.WriteStatus(status, w.redactor.Headers(headers))
}

func (w *archiveStream) WriteAPIRequest(body []byte) error {
	return w.StreamingLogWriter.WriteAPIRequest(w.admit(body))
}

func (w *archiveStream) WriteAPIResponse(body []byte) error {
	return w.StreamingLogWriter.WriteAPIResponse(w.admit(body))
}

func (w *archiveStream) WriteAPIWebsocketTimeline(body []byte) error {
	return w.StreamingLogWriter.WriteAPIWebsocketTimeline(w.admit(body))
}

// WriteAPIResponseErrors masks error text before the file writer sees it. The
// file writer persists these messages verbatim, so the masking must happen here.
func (w *archiveStream) WriteAPIResponseErrors(errs []*interfaces.ErrorMessage) error {
	if len(errs) == 0 {
		return w.StreamingLogWriter.WriteAPIResponseErrors(nil)
	}
	safe := make([]*interfaces.ErrorMessage, 0, len(errs))
	for _, item := range errs {
		if item == nil {
			continue
		}
		entry := &interfaces.ErrorMessage{StatusCode: item.StatusCode}
		if item.Error != nil {
			masked := admitRedacted(&w.budget, w.redactor.Bytes([]byte(item.Error.Error())), w.redactor.secrets, w.limitMarker)
			entry.Error = errors.New(string(masked))
		}
		safe = append(safe, entry)
	}
	return w.StreamingLogWriter.WriteAPIResponseErrors(safe)
}

// admit masks an upstream section and charges it to the same budget as chunks.
func (w *archiveStream) admit(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	if w.metadataOnly {
		return []byte(archiveMetadataOnlyMarker)
	}
	masked := w.redactor.Transcript(body)
	return admitRedacted(&w.budget, masked, w.redactor.secrets, w.limitMarker)
}

// admitRequestBody masks the client request body and charges it to the same budget.
func (w *archiveStream) admitRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	if w.metadataOnly {
		return []byte(archiveMetadataOnlyMarker)
	}
	return admitRedacted(&w.budget, w.redactor.Bytes(body), w.redactor.secrets, w.limitMarker)
}

// Close writes the masked transcript. A stream that was in flight when the archive
// was disabled is discarded instead, so disabling never writes it.
func (w *archiveStream) Close() error {
	if w.finished {
		return nil
	}
	w.finished = true
	defer w.release()
	if !w.archive.IsEnabled() {
		return w.discardUnderlying()
	}
	w.redactor = NewArchiveRedactor(append(w.redactor.secrets, w.archive.redactor(nil).secrets...))
	if len(w.pending) > 0 || w.pendingCut {
		chunk := w.redactor.Transcript(w.pending)
		if w.pendingCut {
			chunk = markCut(chunk, w.redactor.secrets, w.limitMarker)
		}
		if len(chunk) > 0 {
			w.StreamingLogWriter.WriteChunkAsync(chunk)
		}
		w.pending = nil
	}
	w.archive.retentionMu.Lock()
	defer w.archive.retentionMu.Unlock()
	if err := w.StreamingLogWriter.Close(); err != nil {
		return err
	}
	return w.archive.retainLocked()
}

// Discard drops the stream without writing a log file. The response writer calls
// it when a streamed response should not be recorded.
func (w *archiveStream) Discard() error {
	if w.finished {
		return nil
	}
	w.finished = true
	defer w.release()
	return w.discardUnderlying()
}

func (w *archiveStream) discardUnderlying() error {
	w.pending = nil
	w.pendingCut = false
	if discarder, ok := w.StreamingLogWriter.(interface{ Discard() error }); ok {
		return discarder.Discard()
	}
	return w.StreamingLogWriter.Close()
}

// release frees the capture slot when LogStreamingRequest began it. A request
// admitted by the middleware is released there instead.
func (w *archiveStream) release() {
	if w.owned {
		w.capture.Release()
	}
}

var _ RequestLogger = (*RequestArchive)(nil)
