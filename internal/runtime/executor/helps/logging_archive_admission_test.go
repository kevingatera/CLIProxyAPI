package helps

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// archiveCaptureContext returns a request context whose Gin context carries an
// archive capture budget, as the logging middleware sets it up.
func archiveCaptureContext(t *testing.T, rc config.RequestArchiveConfig) (context.Context, *gin.Context, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	rc.Enabled = true
	rc.Directory = dir
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}, RequestArchive: rc}
	archive := logging.NewRequestArchive(cfg, "", t.TempDir())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	logging.AttachArchiveCapture(c, archive.BeginCapture("admission-"+t.Name()))
	return context.WithValue(context.Background(), "gin", c), c, cfg
}

// Finding 3: upstream response bodies are capped at the source, before they
// accumulate in the Gin-context builders.
func TestAppendAPIResponseChunkBoundedByArchiveCapture(t *testing.T) {
	ctx, c, cfg := archiveCaptureContext(t, config.RequestArchiveConfig{MaxCaptureMB: 1})
	chunk := bytes.Repeat([]byte("a"), 64<<10)
	for i := 0; i < 64; i++ {
		AppendAPIResponseChunk(ctx, cfg, chunk)
	}
	attempts := getAttempts(c)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	if got := attempts[0].response.Len(); got > 1<<20+256 {
		t.Fatalf("source buffered %d bytes of a 4 MiB stream with a 1 MiB cap", got)
	}
	if !strings.Contains(attempts[0].response.String(), "[ARCHIVE TRUNCATED") {
		t.Fatal("capped source did not record a truncation marker")
	}
}

// Finding 3 with metadata-only: no upstream body reaches the source buffer.
func TestAppendAPIResponseChunkMetadataOnlyBuffersNoBody(t *testing.T) {
	ctx, c, cfg := archiveCaptureContext(t, config.RequestArchiveConfig{MetadataOnly: true})
	AppendAPIResponseChunk(ctx, cfg, []byte("upstream-body-marker"))
	attempts := getAttempts(c)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	got := attempts[0].response.String()
	if strings.Contains(got, "upstream-body-marker") {
		t.Fatal("metadata-only source buffered the upstream body")
	}
	if !strings.Contains(got, "[ARCHIVE OMITTED: metadata-only mode]") {
		t.Fatal("metadata-only source did not record an omission marker")
	}
}

// Without an archive budget on the context the helpers behave as before.
func TestAppendAPIResponseChunkWithoutArchiveKeepsBody(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", c)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
	AppendAPIResponseChunk(ctx, cfg, []byte("plain-body-marker"))
	attempts := getAttempts(c)
	if len(attempts) != 1 || !strings.Contains(attempts[0].response.String(), "plain-body-marker") {
		t.Fatal("unbudgeted capture changed the body")
	}
}
