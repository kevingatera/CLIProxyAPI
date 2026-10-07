package management

import (
	"strings"
	"testing"
)

func TestPatchUnifiedPresentationPreservesDocument(t *testing.T) {
	before := []byte("# preserve\napi-keys: [synthetic]\nrouting:\n  strategy: quota-aware\n  unified-models:\n    enabled: true # enabled\n    bare-names: false # namespace\n    expose-legacy: false\n    models: []\n")
	value := true
	after, err := patchUnifiedPresentation(before, map[string]*bool{"bare-names": &value})
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Replace(string(before), "bare-names: false", "bare-names: true", 1)
	if string(after) != expected {
		t.Fatalf("unexpected document modification")
	}
}
func TestPatchUnifiedPresentationRequiresExplicitSwitch(t *testing.T) {
	value := true
	_, err := patchUnifiedPresentation([]byte("routing:\n  unified-models:\n    enabled: true\n"), map[string]*bool{"bare-names": &value})
	if err == nil {
		t.Fatal("missing switch accepted")
	}
}
