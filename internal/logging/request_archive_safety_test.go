package logging

import (
	"bytes"
	"compress/gzip"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// archiveLogFiles lists completed log files, ignoring the directory marker.
func archiveLogFiles(t *testing.T, a *RequestArchive) []string {
	t.Helper()
	entries, err := os.ReadDir(a.file.logsDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() == archiveDirMarkerFile {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

// Finding 1: disabling the archive mid-stream must not write the in-flight stream,
// and the capture slot must come back.
func TestArchiveDisabledMidStreamWritesNothing(t *testing.T) {
	a := newTestArchive(t)
	w, err := a.LogStreamingRequest("/v1/chat/completions", "POST", nil, []byte("mid-stream prompt"), "disabled-mid")
	if err != nil {
		t.Fatal(err)
	}
	_ = w.WriteStatus(200, nil)
	w.WriteChunkAsync([]byte("mid-stream chunk marker"))

	// Config reload that turns the archive off while the stream is open.
	a.SetConfig(&config.Config{})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if files := archiveLogFiles(t, a); len(files) != 0 {
		t.Fatalf("disabled archive wrote in-flight stream: %v", files)
	}
	if n := a.activeCaptures.Load(); n != 0 {
		t.Fatalf("capture slot leaked: %d", n)
	}
}

func TestArchiveStreamDiscardWritesNothingAndReleasesSlot(t *testing.T) {
	a := newTestArchive(t)
	w, err := a.LogStreamingRequest("/v1/chat/completions", "POST", nil, []byte("discarded prompt"), "discard")
	if err != nil {
		t.Fatal(err)
	}
	w.WriteChunkAsync([]byte("discarded chunk"))
	discarder, ok := w.(interface{ Discard() error })
	if !ok {
		t.Fatalf("archive stream %T does not implement Discard", w)
	}
	if err := discarder.Discard(); err != nil {
		t.Fatal(err)
	}
	if files := archiveLogFiles(t, a); len(files) != 0 {
		t.Fatalf("discard wrote files: %v", files)
	}
	if n := a.activeCaptures.Load(); n != 0 {
		t.Fatalf("capture slot leaked after discard: %d", n)
	}
}

// Admission is shared by upstream sources and the stream: over the limit, a
// request holds no slot and its bodies become markers until a slot is released.
func TestArchiveBeginCaptureAdmissionAndRelease(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxConcurrentCaptures: 1})
	first := a.BeginCapture("first")
	second := a.BeginCapture("second")
	if !first.admitted || second.admitted {
		t.Fatalf("admission first=%v second=%v, want true false", first.admitted, second.admitted)
	}
	if got := string(second.admit([]byte("over-limit body"))); !strings.Contains(got, archiveConcurrencyMarker) || strings.Contains(got, "over-limit") {
		t.Fatalf("over-limit source kept body: %q", got)
	}
	first.Release()
	first.Release() // idempotent
	third := a.BeginCapture("third")
	if !third.admitted {
		t.Fatal("slot not released for the next request")
	}
	third.Release()
	second.Release()
	if n := a.activeCaptures.Load(); n != 0 {
		t.Fatalf("active captures = %d after releases", n)
	}
}

// Finding 3 (archive side): a source admission never keeps more than the per-request cap.
func TestArchiveSourceAdmissionCapsBufferedBody(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxCaptureMB: 1})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	budget := a.BeginCapture("source-cap")
	AttachArchiveCapture(c, budget)
	defer budget.Release()
	big := bytes.Repeat([]byte("s"), 4<<20)
	got := AdmitArchiveCapture(c, big)
	if len(got) > 1<<20+len(archiveTruncatedMarker) {
		t.Fatalf("admitted %d bytes, cap is 1 MiB", len(got))
	}
	if !bytes.HasSuffix(got, []byte(archiveTruncatedMarker)) {
		t.Fatal("cut without marker")
	}
}

// Finding 2: decompression output is bounded by the capture limit.
func TestArchiveDecompressionIsBounded(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write(make([]byte, 32<<20))
	_ = zw.Close()

	headers := map[string][]string{"Content-Encoding": {"gzip"}}
	out, err := decompressArchiveBody(headers, compressed.Bytes(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(out)) > 1<<20+int64(len(archiveDecompressedMarker)) {
		t.Fatalf("decompressed %d bytes past a 1 MiB limit", len(out))
	}
	if !bytes.HasSuffix(out, []byte(archiveDecompressedMarker)) {
		t.Fatal("missing decompression truncation marker")
	}
}

// Finding 2: metadata-only never decompresses a response body.
func TestArchiveMetadataOnlyNeverDecompresses(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MetadataOnly: true})
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte(`{"response":"meta-only-secret-body"}`))
	_ = zw.Close()

	before := archiveDecompressAttempts.Load()
	err := a.LogRequest("/v1/messages", "POST", nil, []byte("body"), 200, map[string][]string{"Content-Encoding": {"gzip"}}, compressed.Bytes(), nil, nil, nil, nil, nil, "meta-decompress", time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n := archiveDecompressAttempts.Load() - before; n != 0 {
		t.Fatalf("metadata-only decompressed %d bodies", n)
	}
	if log := readArchive(t, a); strings.Contains(log, "meta-only-secret-body") {
		t.Fatal("metadata-only archive kept the response body")
	}
}

// Finding 4: a directory the archive did not create is refused, so age and size
// retention cannot delete unrelated logs in it.
func TestArchiveRefusesForeignDirectoryAndKeepsUnrelatedLogs(t *testing.T) {
	dir := t.TempDir()
	unrelated := filepath.Join(dir, "unrelated.log")
	if err := os.WriteFile(unrelated, []byte("shared log"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-400 * 24 * time.Hour)
	_ = os.Chtimes(unrelated, old, old)

	cfg := &config.Config{RequestArchive: config.RequestArchiveConfig{Enabled: true, Directory: dir, MaxAgeDays: 1, MaxSizeGB: 1}}
	a := NewRequestArchive(cfg, "", t.TempDir())
	if a.startupErr == nil {
		t.Fatal("archive accepted a non-empty directory it did not create")
	}
	if err := a.LogRequest("/v1/messages", "POST", nil, []byte("body"), 200, nil, nil, nil, nil, nil, nil, nil, "foreign", time.Now(), time.Now()); err == nil {
		t.Fatal("archive wrote into a foreign directory")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated log was deleted: %v", err)
	}
}

// Finding 6: a cut inside a known secret leaves no partial secret, even one byte long.
func TestTrimSecretFragmentAtAnyCutBoundary(t *testing.T) {
	secrets := []string{"consumer-secret-123456789"}
	for n := 1; n <= 4; n++ {
		data := []byte("prompt " + secrets[0][:n])
		if got := string(trimSecretFragment(data, secrets)); got != "prompt " {
			t.Fatalf("cut after %d secret bytes kept %q", n, got)
		}
	}
	cases := map[string]string{
		"x sk-ab":         "x ",
		"x Bearer tok":    "x ",
		"x eyJhbGciOi":    "x ",
		"plain text here": "plain text here",
		"ends Bearer ":    "ends ",
	}
	for input, want := range cases {
		if got := string(trimSecretFragment([]byte(input), secrets)); got != want {
			t.Errorf("trimSecretFragment(%q) = %q, want %q", input, got, want)
		}
	}
}

// Finding 6 through the stream path: a budget cut inside a known secret is trimmed.
func TestArchiveStreamCutOneByteIntoSecretIsTrimmed(t *testing.T) {
	a := newLimitedArchive(t, config.RequestArchiveConfig{MaxCaptureMB: 1})
	w, err := a.LogStreamingRequest("/v1/chat/completions", "POST", nil, nil, "one-byte-cut")
	if err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat("b", (1<<20)-1)
	w.WriteChunkAsync([]byte(filler + "consumer-secret-123456789"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	log := readArchive(t, a)
	if strings.Contains(log, "consumer-secret") || strings.Contains(log, "c"+archiveTruncatedMarker) {
		t.Fatal("one-byte secret fragment persisted at the cut")
	}
	if !strings.Contains(log, archiveTruncatedMarker) {
		t.Fatal("missing truncation marker")
	}
}
