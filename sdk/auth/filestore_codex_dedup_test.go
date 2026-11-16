package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
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

// newCodexRecord builds a login record the way the OAuth handler does: the default file
// name comes from the plan, so it changes on a plan change.
func newCodexRecord(t *testing.T, accountID, userID, email, plan string) *cliproxyauth.Auth {
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
	return &cliproxyauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Storage:  storage,
		Metadata: map[string]any{"email": email, "plan_type": plan},
	}
}

// codexLogin runs the production order: resolve the file name, then save with creation intent.
func codexLogin(t *testing.T, store *FileTokenStore, accountID, userID, email, plan string) (string, error) {
	t.Helper()
	auth := newCodexRecord(t, accountID, userID, email, plan)
	if err := ResolveCodexRecordFileName(store, auth); err != nil {
		return "", err
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

// The runtime manager keys credentials by ID. A plan-changing re-login must keep the
// registered ID, bump the epoch of the existing entry, and not add a second entry.
func TestCodexReloginKeepsRegisteredIdentity(t *testing.T) {
	store, dir := newCodexTestStore(t)
	manager := cliproxyauth.NewManager(store, nil, nil)
	ctx := context.Background()

	first := newCodexRecord(t, "acct-1313", "user-a", "a@example.com", "plus")
	if _, err := store.Save(cliproxyauth.WithAuthCreationIntent(ctx), first); err != nil {
		t.Fatalf("first save: %v", err)
	}
	registeredID := first.ID
	if _, err := manager.Register(ctx, first); err != nil {
		t.Fatalf("register first: %v", err)
	}

	second := newCodexRecord(t, "acct-1313", "user-a", "a@example.com", "pro")
	defaultID := second.ID
	if err := ResolveCodexRecordFileName(store, second); err != nil {
		t.Fatalf("resolve second: %v", err)
	}
	if second.ID != registeredID {
		t.Fatalf("resolved ID = %q, want registered ID %q", second.ID, registeredID)
	}
	if _, err := store.Save(cliproxyauth.WithAuthCreationIntent(ctx), second); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if second.ID != registeredID {
		t.Fatalf("Save changed ID to %q after registration lookup", second.ID)
	}
	if _, err := manager.Register(ctx, second); err != nil {
		t.Fatalf("register second: %v", err)
	}

	if files := jsonFilesIn(t, dir); len(files) != 1 {
		t.Fatalf("files = %v, want one", files)
	}
	if _, ok := manager.GetByID(defaultID); ok && defaultID != registeredID {
		t.Fatalf("manager holds a second entry %q", defaultID)
	}
	current, ok := manager.GetByID(registeredID)
	if !ok || current == nil {
		t.Fatalf("registered entry %q missing", registeredID)
	}
	if current.RegistrationEpoch < 2 {
		t.Fatalf("registration epoch = %d, want the existing entry bumped to 2 or more", current.RegistrationEpoch)
	}
	if got := len(manager.List()); got != 1 {
		t.Fatalf("manager entries = %d, want 1", got)
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

// A restarted process has no in-memory state, so the scan alone must find the file.
func TestCodexReloginAfterRestartReusesFile(t *testing.T) {
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

// A file uploaded outside the store for the same identity is reused, not duplicated.
func TestCodexReloginReusesExternallyUploadedFile(t *testing.T) {
	store, dir := newCodexTestStore(t)
	raw, err := json.Marshal(newCodexRecord(t, "acct-1818", "user-a", "a@example.com", "plus").Storage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	doc["type"] = "codex"
	doc["account_id"] = "acct-1818"
	doc["id_token"] = fakeCodexIDToken(t, "acct-1818", "user-a", "a@example.com", "plus")
	uploaded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal upload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "my-upload.json"), uploaded, 0o600); err != nil {
		t.Fatalf("write upload: %v", err)
	}
	path, err := codexLogin(t, store, "acct-1818", "user-a", "a@example.com", "pro")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if filepath.Base(path) != "my-upload.json" {
		t.Fatalf("saved to %q, want the uploaded file", path)
	}
	if files := jsonFilesIn(t, dir); len(files) != 1 {
		t.Fatalf("files = %v, want one", files)
	}
}

// A file deleted outside the store must not be revived or redirected to.
func TestCodexReloginAfterExternalDeleteCreatesFile(t *testing.T) {
	store, dir := newCodexTestStore(t)
	path, err := codexLogin(t, store, "acct-1919", "user-a", "a@example.com", "plus")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("external delete: %v", err)
	}
	if _, err := codexLogin(t, store, "acct-1919", "user-a", "a@example.com", "pro"); err != nil {
		t.Fatalf("login after external delete: %v", err)
	}
	files := jsonFilesIn(t, dir)
	if len(files) != 1 || strings.Contains(files[0], "plus") {
		t.Fatalf("files = %v, want one new pro file", files)
	}
}

// A file at the target name that belongs to another user must never be overwritten.
func TestCodexRefusesToOverwriteForeignFileAtTargetName(t *testing.T) {
	store, dir := newCodexTestStore(t)
	foreign := newCodexRecord(t, "acct-2020", "user-b", "a@example.com", "pro")
	foreignPath := filepath.Join(dir, foreign.FileName)
	if _, err := store.Save(cliproxyauth.WithAuthCreationIntent(context.Background()), foreign); err != nil {
		t.Fatalf("save foreign: %v", err)
	}
	before, err := os.ReadFile(foreignPath)
	if err != nil {
		t.Fatalf("read foreign: %v", err)
	}

	mine := newCodexRecord(t, "acct-2020", "user-a", "a@example.com", "pro")
	if mine.FileName != foreign.FileName {
		t.Fatalf("test setup: names differ: %q vs %q", mine.FileName, foreign.FileName)
	}
	if err := ResolveCodexRecordFileName(store, mine); err == nil {
		t.Fatalf("resolve accepted a name held by another user")
	}
	if _, err := store.Save(cliproxyauth.WithAuthCreationIntent(context.Background()), mine); err == nil {
		t.Fatalf("save overwrote a file held by another user")
	}
	after, err := os.ReadFile(foreignPath)
	if err != nil {
		t.Fatalf("read foreign after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("foreign credential was modified")
	}
}

// A target file without verifiable identity (legacy, no id_token) is not overwritten.
func TestCodexRefusesUnverifiedFileAtTargetName(t *testing.T) {
	store, dir := newCodexTestStore(t)
	mine := newCodexRecord(t, "acct-2121", "user-a", "a@example.com", "pro")
	legacy := []byte(`{"type":"codex","email":"a@example.com","access_token":"old"}`)
	if err := os.WriteFile(filepath.Join(dir, mine.FileName), legacy, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if _, err := store.Save(cliproxyauth.WithAuthCreationIntent(context.Background()), mine); err == nil {
		t.Fatalf("save overwrote an unverified file")
	}
	raw, err := os.ReadFile(filepath.Join(dir, mine.FileName))
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	if string(raw) != string(legacy) {
		t.Fatalf("unverified legacy file was modified")
	}
}

// A scan that fails is reported, and the next attempt scans again instead of reusing
// a cached partial result.
func TestCodexScanErrorIsNotCached(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unreadable directories are readable for root")
	}
	store, dir := newCodexTestStore(t)
	if _, err := codexLogin(t, store, "acct-2222", "user-a", "a@example.com", "plus"); err != nil {
		t.Fatalf("seed login: %v", err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	probe := newCodexRecord(t, "acct-2222", "user-a", "a@example.com", "pro")
	if err := ResolveCodexRecordFileName(store, probe); err == nil {
		t.Fatalf("resolve succeeded with an unreadable credential directory")
	}
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	probe = newCodexRecord(t, "acct-2222", "user-a", "a@example.com", "pro")
	if err := ResolveCodexRecordFileName(store, probe); err != nil {
		t.Fatalf("resolve after repair: %v", err)
	}
	if strings.Contains(probe.FileName, "pro") {
		t.Fatalf("resolved to %q, want the existing plus file", probe.FileName)
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
		if err := ResolveCodexRecordFileName(store, auth); err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if _, err := store.Save(context.Background(), auth); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	if files := jsonFilesIn(t, dir); len(files) != 2 {
		t.Fatalf("files = %v, want two for a non-codex provider", files)
	}
}
