package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// reservingStreamWriter stands in for the archive stream. It admits at most limit
// bytes, like the archive's per-request budget.
type reservingStreamWriter struct {
	logging.StreamingLogWriter
	limit    int
	reserved int
}

func (s *reservingStreamWriter) ReserveChunk(n int) int {
	kept := s.limit - s.reserved
	if kept > n {
		kept = n
	}
	if kept < 0 {
		kept = 0
	}
	s.reserved += kept
	return kept
}

// archiveTestLogger reports lossless streaming, as the request archive does.
type archiveTestLogger struct{ logging.RequestLogger }

func (archiveTestLogger) LosslessStreaming() bool { return true }

func newArchiveReserveWrapper(t *testing.T, limit int) (*ResponseWriterWrapper, *httptest.ResponseRecorder, *reservingStreamWriter) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	stream := &reservingStreamWriter{limit: limit}
	w := &ResponseWriterWrapper{
		ResponseWriter: c.Writer,
		isStreaming:    true,
		chunkChannel:   make(chan []byte, 100),
		streamWriter:   stream,
		logger:         archiveTestLogger{},
	}
	return w, rec, stream
}

func drainArchiveChunks(w *ResponseWriterWrapper) int {
	queued := 0
	for len(w.chunkChannel) > 0 {
		queued += len(<-w.chunkChannel)
	}
	return queued
}

// A refused chunk still reaches the client but is never copied into the queue.
func TestArchiveStreamRefusedChunkIsNotQueued(t *testing.T) {
	w, rec, _ := newArchiveReserveWrapper(t, 0)
	if _, err := w.Write([]byte("client-bytes")); err != nil {
		t.Fatal(err)
	}
	if got := len(w.chunkChannel); got != 0 {
		t.Fatalf("refused chunk was queued: %d chunks", got)
	}
	if rec.Body.String() != "client-bytes" {
		t.Fatalf("client lost bytes: %q", rec.Body.String())
	}
}

// Queued bytes never exceed what the archive reserved, across Write and WriteString.
func TestArchiveStreamQueuedBytesStayWithinReservation(t *testing.T) {
	w, rec, stream := newArchiveReserveWrapper(t, 10)
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte("abcd")); err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString("efgh"); err != nil {
			t.Fatal(err)
		}
	}
	if queued := drainArchiveChunks(w); queued != 10 {
		t.Fatalf("queued %d bytes, want the 10 byte reservation", queued)
	}
	if stream.reserved != 10 {
		t.Fatalf("reserved %d bytes, want 10", stream.reserved)
	}
	if rec.Body.Len() != 24 {
		t.Fatalf("client received %d bytes, want all 24", rec.Body.Len())
	}
}
