package helps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorProviderModelID(t *testing.T) {
	tests := map[string]string{
		"cursor/auto":               "auto",
		"auto":                      "auto",
		"team/cursor/gpt-5.3-codex": "gpt-5.3-codex",
		"  cursor/sonnet-4.6  ":     "sonnet-4.6",
	}
	for in, want := range tests {
		if got := CursorProviderModelID(in); got != want {
			t.Fatalf("CursorProviderModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCursorPromptFromOpenAIPayload(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role":"system","content":"You are concise."},
			{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"text","text":"World"}]},
			{"role":"assistant","content":"Hi there"}
		]
	}`)
	prompt := CursorPromptFromOpenAIPayload(payload)
	for _, want := range []string{"SYSTEM:\nYou are concise.", "USER:\nHello\nWorld", "ASSISTANT:\nHi there"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %q", want, prompt)
		}
	}
}

func TestCursorPromptWithImagesWritesPrivateFilesIntoWorkspace(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"What color is this?"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
			]}
		]
	}`)
	dir := t.TempDir()
	prompt, err := CursorPromptFromOpenAIPayloadWithImages(payload, dir)
	if err != nil {
		t.Fatalf("CursorPromptFromOpenAIPayloadWithImages() error: %v", err)
	}
	if strings.Contains(prompt, "data:image/png") {
		t.Fatalf("prompt leaked image data URL: %q", prompt)
	}
	wantPath := filepath.Join(dir, "image-1.png")
	if !strings.Contains(prompt, "- "+wantPath) {
		t.Fatalf("prompt missing attachment path %s: %q", wantPath, prompt)
	}
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("attachment was not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("attachment mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCursorPromptRejectsNonBase64ImageDataURL(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png,raw"}}]}]}`)
	if _, err := CursorPromptFromOpenAIPayloadWithImages(payload, t.TempDir()); err == nil {
		t.Fatal("expected error for non-base64 image data URL")
	}
}
