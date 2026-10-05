package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestRequestArchiveProviderIndependentCapturePreservesWireTraffic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses", "/v1beta/models/gemini:streamGenerateContent", "/api/provider/plugin/completion"} {
		t.Run(path, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{RequestArchive: config.RequestArchiveConfig{Enabled: true, Directory: dir}}
			cfg.RequestLog = true
			cfg.APIKeys = []string{"test-consumer-secret"}
			archive := logging.NewRequestArchive(cfg, "", dir)
			engine := gin.New()
			engine.Use(RequestLoggingMiddleware(archive))
			body := `{"prompt":"provider prompt marker","tool_input":"test-consumer-secret"}`
			wire := ""
			for i := 0; i < 350; i++ {
				wire += fmt.Sprintf("data: {\"delta\":\"provider-chunk-%d\"}\n\n", i)
			}
			wire += "data: {\"token\":\"test-consumer-secret\"}\n\n"
			engine.POST(path, func(c *gin.Context) {
				got, _ := io.ReadAll(c.Request.Body)
				if string(got) != body {
					t.Errorf("request modified: %s", got)
				}
				c.Header("Content-Type", "text/event-stream")
				c.Status(200)
				for _, b := range []byte(wire) {
					_, _ = c.Writer.Write([]byte{b})
				}
			})
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer test-consumer-secret")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, req)
			if recorder.Body.String() != wire {
				t.Fatal("client response changed")
			}
			var archiveText strings.Builder
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".log") {
					b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
					archiveText.Write(b)
				}
			}
			log := archiveText.String()
			if strings.Contains(log, "test-consumer-secret") {
				t.Fatal("secret persisted")
			}
			for _, marker := range []string{"provider prompt marker", "provider-chunk-0", "provider-chunk-349", "[REDACTED]"} {
				if !strings.Contains(log, marker) {
					t.Errorf("missing %s", marker)
				}
			}
		})
	}
}
