package logging

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

func newTestArchive(t *testing.T) *RequestArchive {
	t.Helper()
	cfg := &config.Config{RequestArchive: config.RequestArchiveConfig{Enabled: true, MaxSizeGB: 30}}
	cfg.APIKeys = []string{"consumer-secret-123456789"}
	archive := NewRequestArchive(cfg, "", t.TempDir())
	archive.SetSecretProvider(func() []string { return []string{"oauth-secret-987654321"} })
	return archive
}

func readArchive(t *testing.T, a *RequestArchive) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(a.file.logsDir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out.Write(b)
		info, _ := e.Info()
		if info.Mode().Perm() != 0600 {
			t.Errorf("file mode %v", info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestRequestArchiveMasksAllSectionsAndTemporaryFiles(t *testing.T) {
	a := newTestArchive(t)
	header := map[string][]string{"Authorization": {"Bearer consumer-secret-123456789"}, "Cookie": {"sid=cookie-value; other=second-cookie"}}
	body := []byte(`{"messages":[{"role":"user","content":"archive prompt marker consumer-secret-123456789"}],"credentials":{"custom":"nested-secret"},"api_key":"body-secret"}`)
	upstream := []byte("Upstream URL: https://user:proxy-secret@example.com/?key=query-secret\nAuthorization: Bearer oauth-secret-987654321\nCookie: sid=cookie-value; other=second-cookie\nBody:\n" + string(body))
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte(`{"result":"response marker oauth-secret-987654321","password":"reply-secret"}`))
	_ = zw.Close()
	err := a.LogRequest("/v1/messages?api_key=url-secret", "POST", header, body, 200, map[string][]string{"Content-Encoding": {"gzip"}, "Set-Cookie": {"session=response-cookie"}}, compressed.Bytes(), nil, upstream, []byte(`{"refresh_token":"refresh-secret"}`), nil, nil, "redaction", time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.LogRequest("/v1/responses", "GET", nil, nil, 101, nil, nil, []byte(`{"token":"ws-secret"}`), nil, nil, nil, nil, "websocket", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	log := readArchive(t, a)
	for _, secret := range []string{"consumer-secret-123456789", "oauth-secret-987654321", "cookie-value", "second-cookie", "nested-secret", "body-secret", "url-secret", "query-secret", "proxy-secret", "response-cookie", "reply-secret", "ws-secret", "refresh-secret"} {
		if strings.Contains(log, secret) {
			t.Errorf("secret leaked: %s", secret)
		}
	}
	for _, marker := range []string{"archive prompt marker", "response marker", "[REDACTED]", "API REQUEST", "API RESPONSE"} {
		if !strings.Contains(log, marker) {
			t.Errorf("missing %q", marker)
		}
	}
	entries, _ := os.ReadDir(a.file.logsDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || e.IsDir() {
			t.Errorf("temporary capture remains: %s", e.Name())
		}
	}
}

func TestRequestArchiveStreamingMasksSplitCredentialsWithoutDroppingChunks(t *testing.T) {
	a := newTestArchive(t)
	w, err := a.LogStreamingRequest("/v1/chat/completions", "POST", nil, []byte(`{"messages":[{"role":"user","content":"stream prompt marker"}]}`), "stream")
	if err != nil {
		t.Fatal(err)
	}
	_ = w.WriteStatus(200, map[string][]string{"Content-Type": {"text/event-stream"}, "Authorization": {"oauth-secret-987654321"}})
	transcript := "data: {\"choices\":[{\"delta\":{\"content\":\"oauth-secret-\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"987654321\"}}]}\n\n"
	for i := 0; i < 400; i++ {
		transcript += fmt.Sprintf("data: {\"delta\":\"stream-marker-%d\"}\n\n", i)
	}
	for _, b := range []byte(transcript) {
		w.WriteChunkAsync([]byte{b})
	}
	// Inspect spools before Close, not only the completed artifact.
	before := readArchive(t, a)
	if strings.Contains(before, "oauth-secret-") {
		t.Fatal("raw credential in temporary spool")
	}
	_ = w.WriteAPIRequest([]byte("Authorization: Bearer consumer-secret-123456789\nBody: prompt marker"))
	_ = w.WriteAPIResponse([]byte(transcript))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log := readArchive(t, a)
	for _, secret := range []string{"oauth-secret-", "987654321", "consumer-secret-123456789"} {
		if strings.Contains(log, secret) {
			t.Errorf("split credential leaked %q", secret)
		}
	}
	for i := 0; i < 400; i++ {
		if !strings.Contains(log, fmt.Sprintf("stream-marker-%d", i)) {
			t.Errorf("missing chunk %d", i)
		}
	}
}

func TestArchiveRedactorKnownAndStructuredSecrets(t *testing.T) {
	r := NewArchiveRedactor([]string{"known-secret-value"})
	body := []byte(`{"prompt":"keep my prompt","password":"unknown-password","credentials":{"custom":"custom-secret"},"nested":[{"authorization":"Bearer abc"}],"text":"known-secret-value"}`)
	out := string(r.Bytes(body))
	for _, secret := range []string{"unknown-password", "custom-secret", "Bearer abc", "known-secret-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("leaked %q", secret)
		}
	}
	if !strings.Contains(out, "keep my prompt") {
		t.Fatal("prompt lost")
	}
	got := ArchiveSecrets(map[string]any{"api-key": "config-secret", "headers": map[string]string{"Authorization": "Bearer header-secret"}, "model": "not-a-secret"})
	if len(got) != 2 {
		t.Fatalf("secrets %v", got)
	}
}

func TestArchiveRetentionOnlyRemovesOldCompletedLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, name := range []string{"old.log", "new.log", "active.tmp", "usage_stats.json"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("123456"), 0600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, now.Add(time.Duration(i)*time.Minute), now.Add(time.Duration(i)*time.Minute))
	}
	n, err := enforceLogDirSizeLimit(dir, 6, "")
	if err != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.log")); !os.IsNotExist(err) {
		t.Fatal("old log retained")
	}
	for _, name := range []string{"new.log", "active.tmp", "usage_stats.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("removed %s", name)
		}
	}
}

func TestRequestArchiveForcedErrorsMaskErrorTextAndDirectBodies(t *testing.T) {
	a := newTestArchive(t)
	a.SetEnabled(false)
	apiErrors := []*interfaces.ErrorMessage{{StatusCode: 400, Error: errors.New("password=error-secret"), Body: []byte(`{"api_key":"direct-body-secret"}`)}}
	if err := a.LogRequestWithOptions("/v1/messages", "POST", nil, []byte("error prompt marker"), 400, nil, []byte("error response marker"), nil, nil, nil, nil, apiErrors, true, "error", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	log := readArchive(t, a)
	for _, secret := range []string{"error-secret", "direct-body-secret"} {
		if strings.Contains(log, secret) {
			t.Errorf("error secret leaked %q", secret)
		}
	}
	for _, marker := range []string{"error prompt marker", "error response marker", "400", "[REDACTED]"} {
		if !strings.Contains(log, marker) {
			t.Errorf("missing %s", marker)
		}
	}
}

func TestArchiveRedactorDecodedEscapesCannotHideKnownCredentials(t *testing.T) {
	r := NewArchiveRedactor([]string{"consumer-secret-123456789"})
	input := []byte(`{"prompt":"consumer\u002dsecret-123456789","tool":{"data":["consumer\u002dsecret-123456789"]}}`)
	out := string(r.Bytes(input))
	if strings.Contains(out, "123456789") || strings.Contains(out, "consumer") {
		t.Fatalf("escaped key leaked: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatal("mask missing")
	}
}

func newLimitedArchive(t *testing.T, rc config.RequestArchiveConfig) *RequestArchive {
	t.Helper()
	rc.Enabled = true
	cfg := &config.Config{RequestArchive: rc}
	cfg.APIKeys = []string{"consumer-secret-123456789"}
	return NewRequestArchive(cfg, "", t.TempDir())
}

func TestRequestArchiveStreamingCaptureCapWritesMarker(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxCaptureMB: 1})
	w, err := a.LogStreamingRequest("/v1/chat/completions", "POST", nil, []byte("prompt"), "cap")
	if err != nil {
		t.Fatal(err)
	}
	_ = w.WriteStatus(200, nil)
	chunk := []byte(strings.Repeat("a", 64<<10))
	for i := 0; i < 64; i++ {
		w.WriteChunkAsync(chunk)
	}
	stream := w.(*archiveStream)
	if got := len(stream.pending); got != 1<<20 {
		t.Fatalf("pending grew to %d bytes, want cap %d", got, 1<<20)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log := readArchive(t, a)
	if !strings.Contains(log, archiveTruncatedMarker) {
		t.Fatal("missing truncation marker")
	}
	if len(log) > 2<<20 {
		t.Fatalf("archive %d bytes exceeds capture cap", len(log))
	}
}

func TestRequestArchiveCaptureCutDropsSecretFragment(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxCaptureMB: 1})
	w, err := a.LogStreamingRequest("/v1/chat/completions", "POST", nil, nil, "frag")
	if err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat("b", (1<<20)-10)
	w.WriteChunkAsync([]byte(filler + "consumer-secret-123456789"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log := readArchive(t, a)
	if strings.Contains(log, "consumer-s") {
		t.Fatal("secret prefix persisted at the capture cut")
	}
	if !strings.Contains(log, archiveTruncatedMarker) {
		t.Fatal("missing truncation marker")
	}
}

func TestRequestArchiveConcurrentCaptureLimit(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxConcurrentCaptures: 1})
	first, err := a.LogStreamingRequest("/v1/a", "POST", nil, nil, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.LogStreamingRequest("/v1/b", "POST", nil, nil, "second")
	if err != nil {
		t.Fatal(err)
	}
	second.WriteChunkAsync([]byte("second-body-marker"))
	if len(second.(*archiveStream).pending) != 0 {
		t.Fatal("over-limit capture buffered bytes")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	first.WriteChunkAsync([]byte("first-body-marker"))
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if n := a.activeCaptures.Load(); n != 0 {
		t.Fatalf("active captures leaked: %d", n)
	}
	log := readArchive(t, a)
	if !strings.Contains(log, archiveConcurrencyMarker) || strings.Contains(log, "second-body-marker") {
		t.Fatal("over-limit capture not replaced by marker")
	}
	if !strings.Contains(log, "first-body-marker") {
		t.Fatal("admitted capture lost")
	}
	third, err := a.LogStreamingRequest("/v1/c", "POST", nil, nil, "third")
	if err != nil {
		t.Fatal(err)
	}
	if !third.(*archiveStream).counted {
		t.Fatal("slot not released after close")
	}
	_ = third.Close()
}

func TestRequestArchiveNonStreamingBodiesAreBoundedAndMasked(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxCaptureMB: 1})
	body := []byte(`{"prompt":"masked consumer-secret-123456789"}`)
	response := []byte(strings.Repeat("r", 2<<20))
	if err := a.LogRequest("/v1/messages", "POST", nil, body, 200, nil, response, nil, nil, nil, nil, nil, "bounded", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	log := readArchive(t, a)
	if strings.Contains(log, "consumer-secret-123456789") || !strings.Contains(log, "[REDACTED]") {
		t.Fatal("masking not applied before bounding")
	}
	if !strings.Contains(log, archiveTruncatedMarker) || len(log) > 2<<20 {
		t.Fatalf("response not bounded: %d bytes", len(log))
	}
}

func TestRequestArchiveMetadataOnlyOmitsBodies(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MetadataOnly: true})
	if err := a.LogRequest("/v1/messages", "POST", map[string][]string{"X-Trace": {"trace-marker"}}, []byte("body-marker"), 200, nil, []byte("response-marker"), nil, nil, nil, nil, nil, "meta", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	w, err := a.LogStreamingRequest("/v1/stream", "POST", nil, nil, "meta-stream")
	if err != nil {
		t.Fatal(err)
	}
	w.WriteChunkAsync([]byte("stream-marker"))
	_ = w.WriteAPIResponse([]byte("upstream-marker"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log := readArchive(t, a)
	for _, body := range []string{"body-marker", "response-marker", "stream-marker", "upstream-marker"} {
		if strings.Contains(log, body) {
			t.Errorf("metadata-only kept %q", body)
		}
	}
	if !strings.Contains(log, "trace-marker") || !strings.Contains(log, archiveMetadataOnlyMarker) {
		t.Fatal("metadata or omission marker missing")
	}
}

func TestRequestArchiveAgeRetentionUsesClock(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxAgeDays: 7})
	now := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
	a.clock = func() time.Time { return now }
	dir := a.file.logsDir
	for name, age := range map[string]time.Duration{"expired.log": 8 * 24 * time.Hour, "fresh.log": 6 * 24 * time.Hour, "expired.tmp": 30 * 24 * time.Hour} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	if err := a.retain(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "expired.log")); !os.IsNotExist(err) {
		t.Fatal("expired log retained")
	}
	for _, name := range []string{"fresh.log", "expired.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("removed %s", name)
		}
	}
}

func TestRequestArchiveSizeRetentionStillAppliesWithAgeLimit(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxAgeDays: 365})
	now := time.Now()
	a.clock = func() time.Time { return now }
	dir := a.file.logsDir
	for i, name := range []string{"old.log", "new.log"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("123456"), 0600); err != nil {
			t.Fatal(err)
		}
		ts := now.Add(time.Duration(i-2) * time.Minute)
		_ = os.Chtimes(p, ts, ts)
	}
	if _, err := enforceLogDirSizeLimit(dir, 6, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.retain(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.log")); !os.IsNotExist(err) {
		t.Fatal("size retention did not remove oldest log")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.log")); err != nil {
		t.Fatal("age limit removed a fresh log")
	}
}
