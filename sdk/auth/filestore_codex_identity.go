package auth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// codexIdentity returns the key for one Codex login: the workspace account plus the
// user inside it. A workspace account_id alone is shared by every member, so it is
// never enough. Returns "" when either part is unknown, which disables dedup.
func codexIdentity(accountID, idToken string) string {
	accountID = strings.TrimSpace(accountID)
	var claims *codex.JWTClaims
	if token := strings.TrimSpace(idToken); token != "" {
		if parsed, err := codex.ParseJWTToken(token); err == nil {
			claims = parsed
		}
	}
	user := ""
	if claims != nil {
		if accountID == "" {
			accountID = strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID)
		}
		user = firstNonEmptyString(claims.CodexAuthInfo.ChatgptUserID, claims.CodexAuthInfo.UserID, claims.Sub)
	}
	if accountID == "" || user == "" {
		return ""
	}
	return "codex\x1f" + accountID + "\x1f" + user
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// codexAuthIdentity derives the identity of an in-memory Codex credential.
func codexAuthIdentity(auth *cliproxyauth.Auth) string {
	if !isCodexAuth(auth) {
		return ""
	}
	accountID, idToken := "", ""
	if storage, ok := auth.Storage.(*codex.CodexTokenStorage); ok && storage != nil {
		accountID, idToken = storage.AccountID, storage.IDToken
	}
	if auth.Metadata != nil {
		accountID = firstNonEmptyString(accountID, metadataString(auth.Metadata, "account_id"))
		idToken = firstNonEmptyString(idToken, metadataString(auth.Metadata, "id_token"))
	}
	return codexIdentity(accountID, idToken)
}

func isCodexAuth(auth *cliproxyauth.Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}

// readCodexIdentity reads one JSON file. isCodex is false for files that are not
// Codex credentials or do not parse, so they are ignored. A returned error means the
// file could not be read and the caller must not decide from it.
func readCodexIdentity(path string) (identity string, isCodex bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var probe struct {
		Type      string `json:"type"`
		AccountID string `json:"account_id"`
		IDToken   string `json:"id_token"`
	}
	if errJSON := json.Unmarshal(raw, &probe); errJSON != nil {
		return "", false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(probe.Type), "codex") {
		return "", false, nil
	}
	return codexIdentity(probe.AccountID, probe.IDToken), true, nil
}

// scanCodexIdentities reads every Codex credential under dir on each call. Nothing is
// cached, so files added, replaced, or deleted outside this store are seen immediately.
// Any read error other than a file that vanished during the scan fails the whole scan.
func scanCodexIdentities(dir string) (map[string]string, error) {
	out := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) && path == dir {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		identity, isCodex, errRead := readCodexIdentity(path)
		if errors.Is(errRead, fs.ErrNotExist) {
			return nil
		}
		if errRead != nil {
			return errRead
		}
		if !isCodex {
			return nil
		}
		rel, errRel := filepath.Rel(dir, path)
		if errRel != nil {
			return errRel
		}
		out[rel] = identity
		return nil
	})
	return out, err
}

// codexDirLocks serializes Codex credential resolution and writes per directory within
// this process. Writers in other processes are not covered.
var codexDirLocks sync.Map

func lockCodexDir(dir string) func() {
	key := filepath.Clean(dir)
	if abs, errAbs := filepath.Abs(key); errAbs == nil {
		key = abs
	}
	value, _ := codexDirLocks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// codexRecordDir returns the directory used for the identity lock and scan.
func (s *FileTokenStore) codexRecordDir(path string) string {
	if dir := s.baseDirSnapshot(); dir != "" {
		return dir
	}
	return filepath.Dir(path)
}

// ResolveCodexFileName returns the file name a Codex credential should be saved under.
// Call it before the record is registered with the runtime manager. It returns the one
// existing file for the account and user when exactly one matches. With several matches
// it returns a new, unused name, so no existing file is overwritten. It fails when the
// default name holds a different or unverifiable credential and nothing matches.
func (s *FileTokenStore) ResolveCodexFileName(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = strings.TrimSpace(auth.ID)
	}
	identity := codexAuthIdentity(auth)
	dir := s.baseDirSnapshot()
	if !isCodexAuth(auth) || identity == "" || dir == "" || name == "" || filepath.Base(name) != name {
		return name, nil
	}

	unlock := lockCodexDir(dir)
	defer unlock()

	known, err := scanCodexIdentities(dir)
	if err != nil {
		return "", fmt.Errorf("auth filestore: scan codex credentials failed: %w", err)
	}
	var matches []string
	for candidate, candidateIdentity := range known {
		if candidateIdentity == identity {
			matches = append(matches, candidate)
		}
	}
	sort.Strings(matches)

	switch len(matches) {
	case 0:
		if _, errStat := os.Stat(filepath.Join(dir, name)); errStat == nil {
			return "", fmt.Errorf("auth filestore: %s already exists and is not verified as this Codex account; remove or rename it and log in again", name)
		} else if !errors.Is(errStat, fs.ErrNotExist) {
			return "", fmt.Errorf("auth filestore: stat %s failed: %w", name, errStat)
		}
		return name, nil
	case 1:
		return matches[0], nil
	default:
		log.Warnf("codex: %d credential files share one account and user; saving a new file without overwriting any of them", len(matches))
		return unusedCodexFileName(dir, name, known)
	}
}

// unusedCodexFileName returns name, or name with a numeric suffix, that neither the scan
// nor the disk already uses.
func unusedCodexFileName(dir, name string, known map[string]string) (string, error) {
	base := strings.TrimSuffix(name, ".json")
	for i := 1; i <= 1000; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d.json", base, i)
		}
		if _, taken := known[candidate]; taken {
			continue
		}
		_, errStat := os.Stat(filepath.Join(dir, candidate))
		if errors.Is(errStat, fs.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("auth filestore: no unused Codex file name near %s", name)
}

// beforeCodexPersist runs between the Codex save start and the final identity check. Tests
// use it to simulate another writer replacing the file in that window.
var beforeCodexPersist = func(string) {}

// verifyCodexTargetLocked refuses a creation save over an existing file that is not
// verified as the same Codex identity. Runtime refreshes do not carry creation intent
// and are not checked. Caller holds s.mu and the directory lock.
func (s *FileTokenStore) verifyCodexTargetLocked(ctx context.Context, auth *cliproxyauth.Auth, path string) error {
	if !isCodexAuth(auth) || !cliproxyauth.HasAuthCreationIntent(ctx) {
		return nil
	}
	if _, errStat := os.Stat(path); errStat != nil {
		if errors.Is(errStat, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("auth filestore: stat %s failed: %w", filepath.Base(path), errStat)
	}
	identity, isCodex, errRead := readCodexIdentity(path)
	if errRead != nil {
		return fmt.Errorf("auth filestore: read %s failed: %w", filepath.Base(path), errRead)
	}
	want := codexAuthIdentity(auth)
	if want == "" {
		// A login without derivable identity keeps the upstream explicit-replacement behavior.
		return nil
	}
	if !isCodex || identity != want {
		return fmt.Errorf("auth filestore: refusing to overwrite %s: it is not verified as this Codex account", filepath.Base(path))
	}
	return nil
}

// codexFileNameResolver is implemented by stores that can pick a Codex file name.
type codexFileNameResolver interface {
	ResolveCodexFileName(*cliproxyauth.Auth) (string, error)
}

// ResolveCodexRecordFileName points a new Codex record at the file it should be saved
// to. It is a no-op for other providers and for stores without a file name resolver.
// Callers invoke it before registration so the record ID never changes after Save.
func ResolveCodexRecordFileName(store any, record *cliproxyauth.Auth) error {
	resolver, ok := store.(codexFileNameResolver)
	if !ok || !isCodexAuth(record) {
		return nil
	}
	name, err := resolver.ResolveCodexFileName(record)
	if err != nil {
		return err
	}
	if name != "" {
		record.FileName = name
		record.ID = name
	}
	return nil
}
