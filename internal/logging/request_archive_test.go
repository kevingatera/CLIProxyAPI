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
