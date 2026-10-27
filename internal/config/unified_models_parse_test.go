package config

import "testing"

// Management writes parse through ParseConfigBytes; they must reject what file loading rejects,
// or an API-accepted config fails at the next restart.
func TestParseConfigBytesRejectsInvalidUnifiedModels(t *testing.T) {
	if _, err := ParseConfigBytes([]byte("routing:\n  unified-models:\n    enabled: true\n")); err == nil {
		t.Fatal("enabled unified models without models were accepted")
	}
}
