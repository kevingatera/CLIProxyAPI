package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestFileTokenStoreDedupByAccountID verifies that saving a codex auth whose
// account_id already exists on disk (under a different filename, e.g. because
// the email alias or plan type changed between logins) overwrites the existing
// file in place rather than creating a duplicate.
func TestFileTokenStoreDedupByAccountID(t *testing.T) {
	const accountID = "8bb28d34-5585-4df0-be2b-b85d8408b326"
	baseDir := t.TempDir()

	// Seed an existing codex auth file for the account (the "plus" variant).
	existingName := "codex-kevingatera@gmail.com.json"
	existingPath := filepath.Join(baseDir, existingName)
	if err := os.WriteFile(existingPath, []byte(`{"type":"codex","account_id":"`+accountID+`","email":"kevingatera@gmail.com"}`), 0o600); err != nil {
		t.Fatalf("seed existing auth: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	// Now save a NEW record for the SAME account_id but a different filename
	// (as would happen if the plan type changed to "prolite"). Metadata carries
	// the account_id; the dedup must rewrite FileName to the existing one.
	dupAuth := &cliproxyauth.Auth{
		ID:       "codex-kevingatera@gmail.com-prolite.json",
		Provider: "codex",
		FileName: "codex-kevingatera@gmail.com-prolite.json",
		Metadata: map[string]any{
			"type":       "codex",
			"email":      "kevingatera@gmail.com",
			"account_id": accountID,
		},
	}

	path, err := store.Save(context.Background(), dupAuth)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// The saved path must be the EXISTING file, not a new duplicate.
	if got := filepath.Base(path); got != existingName {
		t.Fatalf("Save() wrote to %q, want to overwrite existing %q (no duplicate)", got, existingName)
	}

	// The duplicate filename must NOT have been created.
	if _, err := os.Stat(filepath.Join(baseDir, "codex-kevingatera@gmail.com-prolite.json")); !os.IsNotExist(err) {
		t.Fatalf("duplicate file was created; expected it to be suppressed by dedup")
	}

	// The auth record's FileName/ID must have been rewritten to the existing one.
	if dupAuth.FileName != existingName {
		t.Fatalf("auth.FileName = %q, want %q", dupAuth.FileName, existingName)
	}
	if dupAuth.ID != existingName {
		t.Fatalf("auth.ID = %q, want %q", dupAuth.ID, existingName)
	}
}

// TestFileTokenStoreNoDedupWhenAccountIDUnknown ensures the dedup is skipped
// when account_id is absent, so two genuinely-distinct accounts (or accounts
// whose JWT failed to parse) are not wrongly merged.
func TestFileTokenStoreNoDedupWhenAccountIDUnknown(t *testing.T) {
	baseDir := t.TempDir()

	// Seed a codex file with no account_id.
	existingName := "codex-unknown@example.com.json"
	if err := os.WriteFile(filepath.Join(baseDir, existingName), []byte(`{"type":"codex","email":"unknown@example.com"}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	// Save a different codex auth also with NO account_id - must not dedup.
	newAuth := &cliproxyauth.Auth{
		ID:       "codex-other@example.com.json",
		Provider: "codex",
		FileName: "codex-other@example.com.json",
		Metadata: map[string]any{"type": "codex", "email": "other@example.com"},
	}
	path, err := store.Save(context.Background(), newAuth)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got := filepath.Base(path); got != "codex-other@example.com.json" {
		t.Fatalf("Save() wrote to %q, want the new file (no dedup without account_id)", got)
	}
}

// TestFileTokenStoreNoDedupAcrossProviders ensures the dedup is scoped to the
// same provider: a codex and a claude auth sharing an account_id-ish value must
// both be kept.
func TestFileTokenStoreNoDedupAcrossProviders(t *testing.T) {
	const sharedID = "abc-123"
	baseDir := t.TempDir()

	// Seed a codex file.
	if err := os.WriteFile(filepath.Join(baseDir, "codex-x.json"), []byte(`{"type":"codex","account_id":"`+sharedID+`"}`), 0o600); err != nil {
		t.Fatalf("seed codex: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	// Save a CLAUDE auth with the same account_id - must NOT be deduped to codex.
	claudeAuth := &cliproxyauth.Auth{
		ID:       "claude-x.json",
		Provider: "claude",
		FileName: "claude-x.json",
		Metadata: map[string]any{"type": "claude", "account_id": sharedID},
	}
	path, err := store.Save(context.Background(), claudeAuth)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got := filepath.Base(path); got != "claude-x.json" {
		t.Fatalf("Save() wrote to %q, want claude-x.json (no cross-provider dedup)", got)
	}
}
