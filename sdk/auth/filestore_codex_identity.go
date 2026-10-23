package auth

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
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

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}

// codexIdentityIndex maps identity to the credential files that carry it. It is built
// once per base directory and kept current by Save and Delete. Guarded by FileTokenStore.mu.
type codexIdentityIndex struct {
	dir    string
	built  bool
	byKey  map[string]map[string]struct{}
	byName map[string]string
}

func (idx *codexIdentityIndex) reset(dir string) {
	idx.dir = dir
	idx.built = true
	idx.byKey = make(map[string]map[string]struct{})
	idx.byName = make(map[string]string)
}

// set records that file name carries identity. An empty identity removes the entry.
func (idx *codexIdentityIndex) set(name, identity string) {
	if !idx.built || name == "" {
		return
	}
	if old, ok := idx.byName[name]; ok {
		if names := idx.byKey[old]; names != nil {
			delete(names, name)
			if len(names) == 0 {
				delete(idx.byKey, old)
			}
		}
		delete(idx.byName, name)
	}
	if identity == "" {
		return
	}
	idx.byName[name] = identity
	names := idx.byKey[identity]
	if names == nil {
		names = make(map[string]struct{})
		idx.byKey[identity] = names
	}
	names[name] = struct{}{}
}

// relIndexName returns the index key for path inside dir, or "" when path is outside dir.
func relIndexName(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return rel
}

func (s *FileTokenStore) ensureCodexIndexLocked(dir string) {
	if s.codexIndex.built && s.codexIndex.dir == dir {
		return
	}
	s.codexIndex.reset(dir)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		name := relIndexName(dir, path)
		raw, errRead := os.ReadFile(path)
		if name == "" || errRead != nil {
			return nil
		}
		var probe struct {
			Type      string `json:"type"`
			AccountID string `json:"account_id"`
			IDToken   string `json:"id_token"`
		}
		if errJSON := json.Unmarshal(raw, &probe); errJSON != nil {
			return nil
		}
		if strings.EqualFold(strings.TrimSpace(probe.Type), "codex") {
			s.codexIndex.set(name, codexIdentity(probe.AccountID, probe.IDToken))
		}
		return nil
	})
}

// redirectCodexDuplicateLocked returns the file a Codex credential should be written to.
// A save that would create a new file is redirected to the single existing file with the
// same identity. An existing target file is never redirected. When several files share an
// identity, none is overwritten and the new file keeps its own path. Caller holds s.mu.
func (s *FileTokenStore) redirectCodexDuplicateLocked(auth *cliproxyauth.Auth, path string) string {
	identity := codexAuthIdentity(auth)
	dir := s.baseDirSnapshot()
	if identity == "" || dir == "" {
		return path
	}
	s.ensureCodexIndexLocked(dir)
	if _, errStat := os.Stat(path); errStat == nil {
		return path
	}
	matches := s.codexIndex.byKey[identity]
	if len(matches) == 0 {
		return path
	}
	if len(matches) > 1 {
		log.Warnf("codex: %d credential files share one account and user; saving without merging", len(matches))
		return path
	}
	for name := range matches {
		target := filepath.Join(dir, name)
		if _, errStat := os.Stat(target); errStat != nil {
			// Stale index entry: the file was removed outside this store.
			s.codexIndex.set(name, "")
			return path
		}
		auth.FileName = name
		auth.ID = name
		return target
	}
	return path
}

// indexSavedCodexLocked refreshes the index entry for a file this store just wrote.
func (s *FileTokenStore) indexSavedCodexLocked(auth *cliproxyauth.Auth, path string) {
	dir := s.baseDirSnapshot()
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || dir == "" {
		return
	}
	if !s.codexIndex.built || s.codexIndex.dir != dir {
		return
	}
	name := relIndexName(dir, path)
	raw, errRead := os.ReadFile(path)
	if name == "" || errRead != nil {
		s.codexIndex.set(name, "")
		return
	}
	var probe struct {
		AccountID string `json:"account_id"`
		IDToken   string `json:"id_token"`
	}
	if errJSON := json.Unmarshal(raw, &probe); errJSON != nil {
		s.codexIndex.set(name, "")
		return
	}
	s.codexIndex.set(name, codexIdentity(probe.AccountID, probe.IDToken))
}

// forgetCodexFileLocked drops a deleted file from the index. Caller holds s.mu.
func (s *FileTokenStore) forgetCodexFileLocked(path string) {
	if dir := s.baseDirSnapshot(); dir != "" {
		s.codexIndex.set(relIndexName(dir, path), "")
	}
}
