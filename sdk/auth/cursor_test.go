package auth

import "testing"

func TestParseCursorModelsOutput_JSONArray(t *testing.T) {
	raw := []byte(`["auto","gpt-5.4-medium",{"id":"sonnet-4.5"},"auto"]`)
	got := parseCursorModelsOutput(raw)
	if len(got) != 3 {
		t.Fatalf("expected 3 models, got %d: %#v", len(got), got)
	}
	if got[0] != "auto" || got[1] != "gpt-5.4-medium" || got[2] != "sonnet-4.5" {
		t.Fatalf("unexpected models: %#v", got)
	}
}

func TestParseCursorModelsOutput_JSONObject(t *testing.T) {
	raw := []byte(`{"models":[{"name":"composer-1.5"},{"model":"grok"}]}`)
	got := parseCursorModelsOutput(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %#v", len(got), got)
	}
	if got[0] != "composer-1.5" || got[1] != "grok" {
		t.Fatalf("unexpected models: %#v", got)
	}
}

func TestParseCursorModelsOutput_LineFallback(t *testing.T) {
	raw := []byte("1. auto\n2. sonnet-4.6\n\n")
	got := parseCursorModelsOutput(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %#v", len(got), got)
	}
	if got[0] != "auto" || got[1] != "sonnet-4.6" {
		t.Fatalf("unexpected models: %#v", got)
	}
}
