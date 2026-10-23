package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// fakeCodexIDToken builds an unsigned JWT. ParseJWTToken does not verify signatures.
func fakeCodexIDToken(t *testing.T, accountID, userID, email, plan string) string {
	t.Helper()
	claims := map[string]any{
		"email": email,
		"sub":   userID,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
			"chatgpt_plan_type":  plan,
		},
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// codexLogin saves a Codex credential the way the device login does: through the
// token storage with creation intent. The file name follows the upstream naming scheme.
func codexLogin(t *testing.T, store *FileTokenStore, accountID, userID, email, plan string) (string, error) {
	t.Helper()
	digest := sha256.Sum256([]byte(accountID))
	fileName := codex.CredentialFileName(email, plan, hex.EncodeToString(digest[:])[:8], true)
	storage := &codex.CodexTokenStorage{
		IDToken:      fakeCodexIDToken(t, accountID, userID, email, plan),
		AccessToken:  "access-" + userID,
		RefreshToken: "refresh-" + userID,
		AccountID:    accountID,
		Email:        email,
		PlanType:     plan,
	}
	auth := &cliproxyauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Storage:  storage,
		Metadata: map[string]any{"email": email, "plan_type": plan},
	}
	ctx := cliproxyauth.WithAuthCreationIntent(context.Background())
	path, err := store.Save(ctx, auth)
	if err == nil && path != "" && auth.FileName != filepath.Base(path) {
		t.Fatalf("auth.FileName %q does not match saved file %q", auth.FileName, path)
	}
	return path, err
}

func newCodexTestStore(t *testing.T) (*FileTokenStore, string) {
	t.Helper()
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	return store, dir
}

func jsonFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	return names
}

func readStoredPlan(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		PlanType string `json:"plan_type"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return doc.PlanType
}

func TestCodexReloginWithNewPlanOverwritesSameIdentity(t *testing.T) {
	store, dir := newCodexTestStore(t)
	first, err := codexLogin(t, store, "acct-1111", "user-a", "a@example.com", "plus")
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	second, err := codexLogin(t, store, "acct-1111", "user-a", "a@example.com", "pro")
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if first != second {
		t.Fatalf("plan change wrote %q, want the existing file %q", second, first)
	}
	if files := jsonFilesIn(t, dir); len(files) != 1 {
		t.Fatalf("files = %v, want one", files)
	}
	if got := readStoredPlan(t, second); got != "pro" {
		t.Fatalf("plan_type = %q, want pro", got)
	}
}

func TestCodexSameWorkspaceDifferentUsersStaySeparate(t *testing.T) {
	store, dir := newCodexTestStore(t)
	if _, err := codexLogin(t, store, "acct-2222", "user-a", "a@example.com", "team"); err != nil {
		t.Fatalf("user a: %v", err)
	}
	if _, err := codexLogin(t, store, "acct-2222", "user-b", "b@example.com", "team"); err != nil {
		t.Fatalf("user b: %v", err)
	}
	if files := jsonFilesIn(t, dir); len(files) != 2 {
		t.Fatalf("files = %v, want two separate users", files)
	}
}

func TestCodexSameUserAcrossWorkspacesStaySeparate(t *testing.T) {
	store, dir := newCodexTestStore(t)
	if _, err := codexLogin(t, store, "acct-3333", "user-a", "a@example.com", "plus"); err != nil {
		t.Fatalf("workspace one: %v", err)
	}
	if _, err := codexLogin(t, store, "acct-4444", "user-a", "a@example.com", "plus"); err != nil {
		t.Fatalf("workspace two: %v", err)
	}
	if files := jsonFilesIn(t, dir); len(files) != 2 {
		t.Fatalf("files = %v, want two workspaces", files)
	}
}

func TestCodexUnknownIdentityIsNotMerged(t *testing.T) {
	store, dir := newCodexTestStore(t)
	for _, plan := range []string{"plus", "pro"} {
		name := "codex-noid-" + plan + ".json"
		auth := &cliproxyauth.Auth{
			ID:       name,
			FileName: name,
			Provider: "codex",
			Metadata: map[string]any{"email": "a@example.com", "plan_type": plan, "type": "codex"},
		}
		if _, err := store.Save(context.Background(), auth); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	if files := jsonFilesIn(t, dir); len(files) != 2 {
		t.Fatalf("files = %v, want two when identity is unknown", files)
	}
}

func TestCodexAmbiguousLegacyDuplicatesAreNotOverwritten(t *testing.T) {
	store, dir := newCodexTestStore(t)
	token := fakeCodexIDToken(t, "acct-5555", "user-a", "a@example.com", "plus")
	for _, name := range []string{"legacy-one.json", "legacy-two.json"} {
		raw, _ := json.Marshal(map[string]any{"type": "codex", "account_id": "acct-5555", "id_token": token, "plan_type": "plus", "email": "a@example.com", "access_token": name})
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatalf("write legacy: %v", err)
		}
	}
	before := map[string]string{}
	for _, name := range []string{"legacy-one.json", "legacy-two.json"} {
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		before[name] = string(raw)
	}

	if _, err := codexLogin(t, store, "acct-5555", "user-a", "a@example.com", "pro"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if files := jsonFilesIn(t, dir); len(files) != 3 {
		t.Fatalf("files = %v, want the two legacy files kept plus the new one", files)
	}
	for name, content := range before {
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		if string(raw) != content {
			t.Fatalf("legacy duplicate %s was overwritten", name)
		}
	}
}

func TestCodexConcurrentLoginsCreateOneFile(t *testing.T) {
	store, dir := newCodexTestStore(t)
	plans := []string{"plus", "pro", "team", "plus", "pro", "team", "plus", "pro"}
	var wg sync.WaitGroup
	errs := make(chan error, len(plans))
	for _, plan := range plans {
		wg.Add(1)
		go func(plan string) {
			defer wg.Done()
			_, err := codexLogin(t, store, "acct-6666", "user-a", "a@example.com", plan)
			errs <- err
		}(plan)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent login: %v", err)
		}
	}
	if files := jsonFilesIn(t, dir); len(files) != 1 {
		t.Fatalf("files = %v, want one", files)
	}
}

func TestCodexIdentityIndexSurvivesRestart(t *testing.T) {
	first, dir := newCodexTestStore(t)
	if _, err := codexLogin(t, first, "acct-7777", "user-a", "a@example.com", "plus"); err != nil {
		t.Fatalf("login before restart: %v", err)
	}
	restarted := NewFileTokenStore()
	restarted.SetBaseDir(dir)
	if _, err := codexLogin(t, restarted, "acct-7777", "user-a", "a@example.com", "pro"); err != nil {
		t.Fatalf("login after restart: %v", err)
	}
	if files := jsonFilesIn(t, dir); len(files) != 1 {
		t.Fatalf("files after restart = %v, want one", files)
	}
}

func TestCodexSaveInPlaceKeepsFileName(t *testing.T) {
	store, dir := newCodexTestStore(t)
	path, err := codexLogin(t, store, "acct-8888", "user-a", "a@example.com", "plus")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	// A runtime refresh of the same credential must not rename or duplicate it.
	name := filepath.Base(path)
	auth := &cliproxyauth.Auth{ID: name, FileName: name, Provider: "codex", Metadata: map[string]any{"email": "a@example.com", "plan_type": "plus", "type": "codex", "account_id": "acct-8888", "id_token": fakeCodexIDToken(t, "acct-8888", "user-a", "a@example.com", "plus")}}
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatalf("refresh save: %v", err)
	}
	if auth.FileName != name || auth.ID != name {
		t.Fatalf("refresh changed identity: FileName=%q ID=%q", auth.FileName, auth.ID)
	}
	if files := jsonFilesIn(t, dir); len(files) != 1 {
		t.Fatalf("files = %v, want one", files)
	}
}

func TestCodexDeletedFileIsNotRevived(t *testing.T) {
	store, dir := newCodexTestStore(t)
	path, err := codexLogin(t, store, "acct-9999", "user-a", "a@example.com", "plus")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := store.Delete(context.Background(), filepath.Base(path)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := codexLogin(t, store, "acct-9999", "user-a", "a@example.com", "pro"); err != nil {
		t.Fatalf("login after delete: %v", err)
	}
	files := jsonFilesIn(t, dir)
	if len(files) != 1 {
		t.Fatalf("files = %v, want one new file", files)
	}
	if strings.Contains(files[0], "plus") {
		t.Fatalf("deleted plus file was revived: %v", files)
	}
}

func TestNonCodexProviderIsNotMerged(t *testing.T) {
	store, dir := newCodexTestStore(t)
	token := fakeCodexIDToken(t, "acct-1212", "user-a", "a@example.com", "plus")
	for _, name := range []string{"claude-one.json", "claude-two.json"} {
		auth := &cliproxyauth.Auth{
			ID:       name,
			FileName: name,
			Provider: "claude",
			Metadata: map[string]any{"type": "claude", "account_id": "acct-1212", "id_token": token, "email": "a@example.com"},
		}
		if _, err := store.Save(context.Background(), auth); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	if files := jsonFilesIn(t, dir); len(files) != 2 {
		t.Fatalf("files = %v, want two for a non-codex provider", files)
	}
}
