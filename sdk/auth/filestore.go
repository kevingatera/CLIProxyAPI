package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// PluginAuthParser parses auth JSON owned by plugin providers.
type PluginAuthParser interface {
	ParseAuth(context.Context, pluginapi.AuthParseRequest) (*cliproxyauth.Auth, bool, error)
}

// PluginMultiAuthParser expands one auth JSON payload into multiple plugin auth records.
// Returning handled=true with an empty slice means the plugin intentionally suppresses built-in parsing.
type PluginMultiAuthParser interface {
	ParseAuths(context.Context, pluginapi.AuthParseRequest) ([]*cliproxyauth.Auth, bool, error)
}

type pluginAuthParserHolder struct {
	parser PluginAuthParser
}

var pluginAuthParserValue atomic.Value

// RegisterPluginAuthParser registers the current plugin auth parser.
func RegisterPluginAuthParser(parser PluginAuthParser) {
	pluginAuthParserValue.Store(pluginAuthParserHolder{parser: parser})
}

func currentPluginAuthParser() PluginAuthParser {
	value := pluginAuthParserValue.Load()
	if value == nil {
		return nil
	}
	holder, ok := value.(pluginAuthParserHolder)
	if !ok {
		return nil
	}
	return holder.parser
}

// FileTokenStore persists token records and auth metadata using the filesystem as backing storage.
type FileTokenStore struct {
	mu      sync.Mutex
	dirLock sync.RWMutex
	baseDir string
}

// NewFileTokenStore creates a token store that saves credentials to disk through the
// TokenStorage implementation embedded in the token record.
func NewFileTokenStore() *FileTokenStore {
	return &FileTokenStore{}
}

// SetBaseDir updates the default directory used for auth JSON persistence when no explicit path is provided.
func (s *FileTokenStore) SetBaseDir(dir string) {
	s.dirLock.Lock()
	s.baseDir = strings.TrimSpace(dir)
	s.dirLock.Unlock()
}

// Save persists token storage and metadata to the resolved auth file path.
func (s *FileTokenStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}

	// Dedup by account_id: if this is a codex auth whose account already exists
	// under a different filename (e.g. email alias or plan-type changed between
	// logins), reuse the existing filename so we overwrite in place instead of
	// minting a duplicate file. account_id is the true identity; the derived
	// filename is not. See AGENTS.md "duplicate auth" note.
	s.dedupByAccountID(auth)

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("auth filestore: missing file path attribute for %s", auth.ID)
	}

	if auth.Disabled {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return "", nil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("auth filestore: create dir failed: %w", err)
	}

	// metadataSetter is a private interface for TokenStorage implementations that support metadata injection.
	type metadataSetter interface {
		SetMetadata(map[string]any)
	}

	switch {
	case auth.Storage != nil:
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata["disabled"] = auth.Disabled
		if setter, ok := auth.Storage.(metadataSetter); ok {
			setter.SetMetadata(auth.Metadata)
		}
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
	case auth.Metadata != nil:
		auth.Metadata["disabled"] = auth.Disabled
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("auth filestore: marshal metadata failed: %w", errMarshal)
		}
		if existing, errRead := os.ReadFile(path); errRead == nil {
			if jsonEqual(existing, raw) {
				return path, nil
			}
			file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
			if errOpen != nil {
				return "", fmt.Errorf("auth filestore: open existing failed: %w", errOpen)
			}
			if _, errWrite := file.Write(raw); errWrite != nil {
				_ = file.Close()
				return "", fmt.Errorf("auth filestore: write existing failed: %w", errWrite)
			}
			if errClose := file.Close(); errClose != nil {
				return "", fmt.Errorf("auth filestore: close existing failed: %w", errClose)
			}
			return path, nil
		} else if !os.IsNotExist(errRead) {
			return "", fmt.Errorf("auth filestore: read existing failed: %w", errRead)
		}
		if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
			return "", fmt.Errorf("auth filestore: write file failed: %w", errWrite)
		}
	default:
		return "", fmt.Errorf("auth filestore: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[cliproxyauth.AttributePath] = path
	auth.Attributes[cliproxyauth.AttributeSource] = path
	auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourceFile

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	return path, nil
}

// List enumerates all auth JSON files under the configured directory.
func (s *FileTokenStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	dir := s.baseDirSnapshot()
	if dir == "" {
		return nil, fmt.Errorf("auth filestore: directory not configured")
	}
	entries := make([]*cliproxyauth.Auth, 0)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		auths, errReadAuths := s.readAuthFiles(path, dir)
		if errReadAuths != nil {
			return nil
		}
		if len(auths) > 0 {
			entries = append(entries, auths...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Delete removes the auth file.
func (s *FileTokenStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("auth filestore: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("auth filestore: delete failed: %w", err)
	}
	return nil
}

func (s *FileTokenStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, id), nil
}

func (s *FileTokenStore) readAuthFiles(path, baseDir string) ([]*cliproxyauth.Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata := make(map[string]any)
	if err = json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	provider, _ := metadata["type"].(string)
	provider = strings.TrimSpace(provider)
	if strings.EqualFold(provider, "gemini") {
		return nil, nil
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, fmt.Errorf("stat file: %w", errStat)
	}
	if parser := currentPluginAuthParser(); parser != nil {
		auths, handled, errParse := parsePluginAuthFile(parser, pluginapi.AuthParseRequest{
			Provider: provider,
			Path:     path,
			FileName: s.idFor(path, baseDir),
			RawJSON:  data,
		})
		if errParse == nil && handled {
			auths = compactPluginAuths(auths)
			if len(auths) == 0 {
				return nil, nil
			}
			disabled, _ := metadata["disabled"].(bool)
			for index, auth := range auths {
				if auth == nil {
					continue
				}
				if len(auths) > 1 {
					cliproxyauth.MarkPluginVirtualAuth(auth, path, index)
				}
				auth.CreatedAt = info.ModTime()
				auth.UpdatedAt = info.ModTime()
				if auth.Attributes == nil {
					auth.Attributes = make(map[string]string)
				}
				auth.Attributes[cliproxyauth.AttributePath] = path
				auth.Attributes[cliproxyauth.AttributeSource] = path
				auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourceFile
				if disabled {
					auth.Disabled = true
					auth.Status = cliproxyauth.StatusDisabled
					if auth.Metadata == nil {
						auth.Metadata = make(map[string]any)
					}
					auth.Metadata["disabled"] = true
				}
				cliproxyauth.ApplyCustomHeadersFromMetadata(auth)
			}
			return auths, nil
		}
	}
	if provider == "" {
		provider = "unknown"
	}
	if provider == "antigravity" {
		projectID := ""
		if pid, ok := metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
		if projectID == "" {
			accessToken := extractAccessToken(metadata)
			if accessToken != "" {
				fetchedProjectID, errFetch := FetchAntigravityProjectID(context.Background(), accessToken, http.DefaultClient)
				if errFetch == nil && strings.TrimSpace(fetchedProjectID) != "" {
					metadata["project_id"] = strings.TrimSpace(fetchedProjectID)
					if raw, errMarshal := json.Marshal(metadata); errMarshal == nil {
						if file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600); errOpen == nil {
							_, _ = file.Write(raw)
							_ = file.Close()
						}
					}
				}
			}
		}
	}
	info, errStat = os.Stat(path)
	if errStat != nil {
		return nil, fmt.Errorf("stat file: %w", errStat)
	}
	id := s.idFor(path, baseDir)
	disabled, _ := metadata["disabled"].(bool)
	status := cliproxyauth.StatusActive
	if disabled {
		status = cliproxyauth.StatusDisabled
	}
	auth := &cliproxyauth.Auth{
		ID:       id,
		Provider: provider,
		FileName: id,
		Label:    s.labelFor(metadata),
		Status:   status,
		Disabled: disabled,
		Attributes: map[string]string{
			cliproxyauth.AttributePath:          path,
			cliproxyauth.AttributeSource:        path,
			cliproxyauth.AttributeSourceBackend: cliproxyauth.AuthSourceFile,
		},
		Metadata:         metadata,
		CreatedAt:        info.ModTime(),
		UpdatedAt:        info.ModTime(),
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}
	if email, ok := metadata["email"].(string); ok && email != "" {
		auth.Attributes["email"] = email
	}
	cliproxyauth.ApplyCustomHeadersFromMetadata(auth)
	return []*cliproxyauth.Auth{auth}, nil
}

func (s *FileTokenStore) readAuthFile(path, baseDir string) (*cliproxyauth.Auth, error) {
	auths, errReadAuths := s.readAuthFiles(path, baseDir)
	if errReadAuths != nil || len(auths) == 0 {
		return nil, errReadAuths
	}
	return auths[0], nil
}

func parsePluginAuthFile(parser PluginAuthParser, req pluginapi.AuthParseRequest) ([]*cliproxyauth.Auth, bool, error) {
	if parser == nil {
		return nil, false, nil
	}
	if multiParser, ok := parser.(PluginMultiAuthParser); ok {
		return multiParser.ParseAuths(context.Background(), req)
	}
	auth, handled, errParse := parser.ParseAuth(context.Background(), req)
	if errParse != nil || !handled || auth == nil {
		return nil, handled, errParse
	}
	return []*cliproxyauth.Auth{auth}, true, nil
}

func compactPluginAuths(auths []*cliproxyauth.Auth) []*cliproxyauth.Auth {
	if len(auths) == 0 {
		return nil
	}
	out := auths[:0]
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(out, auth)
	}
	return out
}

func (s *FileTokenStore) idFor(path, baseDir string) string {
	id := path
	if baseDir != "" {
		if rel, errRel := filepath.Rel(baseDir, path); errRel == nil && rel != "" {
			id = rel
		}
	}
	// On Windows, normalize ID casing to avoid duplicate auth entries caused by case-insensitive paths.
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}
	return id
}

func (s *FileTokenStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		if dir := s.baseDirSnapshot(); dir != "" {
			return filepath.Join(dir, fileName), nil
		}
		return fileName, nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("auth filestore: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, auth.ID), nil
}

// dedupByAccountID rewrites auth.FileName/auth.ID in place when an existing
// auth file for the same (provider, account_id) is already on disk under a
// different filename. This makes a re-login of the same account overwrite the
// existing file instead of creating a duplicate. It is a no-op when account_id
// is unknown or no existing match is found. Provider-scoped: only dedups within
// the same provider to avoid merging distinct accounts that happen to share an
// identifier across providers.
func (s *FileTokenStore) dedupByAccountID(auth *cliproxyauth.Auth) {
	if auth == nil {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		return
	}
	accountID := strings.TrimSpace(authAccountID(auth.Metadata))
	if accountID == "" {
		// Cannot dedup against an unknown identity; fall back to current behavior.
		return
	}

	dir := s.baseDirSnapshot()
	if dir == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existingName := s.findExistingFileNameByAccountID(dir, provider, accountID)
	if existingName == "" {
		return
	}
	currentName := strings.TrimSpace(auth.FileName)
	if currentName == "" {
		currentName = strings.TrimSpace(auth.ID)
	}
	if existingName == currentName {
		return
	}
	// Overwrite the existing file in place; the about-to-be-written duplicate is
	// never created. Retire the stale name the new login would have used.
	auth.FileName = existingName
	auth.ID = existingName
	if auth.Attributes != nil {
		delete(auth.Attributes, "path")
	}
}

// findExistingFileNameByAccountID scans dir for auth files whose provider and
// account_id match. Returns the matching filename (basename) or "". Caller must
// hold s.mu. Only the small account_id/type fields are parsed per file.
func (s *FileTokenStore) findExistingFileNameByAccountID(dir, provider, accountID string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		raw, errRead := os.ReadFile(filepath.Join(dir, name))
		if errRead != nil {
			continue
		}
		var probe struct {
			Type      string `json:"type"`
			AccountID string `json:"account_id"`
		}
		if errJSON := json.Unmarshal(raw, &probe); errJSON != nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(probe.Type)) != provider {
			continue
		}
		if strings.TrimSpace(probe.AccountID) == accountID {
			return name
		}
	}
	return ""
}

// authAccountID extracts the canonical account identifier from a codex auth's
// metadata, tolerating the two key variants written by different login paths.
func authAccountID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	for _, key := range []string{"account_id", "chatgpt_account_id"} {
		if v, ok := metadata[key]; ok {
			if s, okStr := v.(string); okStr && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func (s *FileTokenStore) labelFor(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata["label"].(string); ok && v != "" {
		return v
	}
	if v, ok := metadata["email"].(string); ok && v != "" {
		return v
	}
	if project, ok := metadata["project_id"].(string); ok && project != "" {
		return project
	}
	return ""
}

func (s *FileTokenStore) baseDirSnapshot() string {
	s.dirLock.RLock()
	defer s.dirLock.RUnlock()
	return s.baseDir
}

func extractAccessToken(metadata map[string]any) string {
	if at, ok := metadata["access_token"].(string); ok {
		if v := strings.TrimSpace(at); v != "" {
			return v
		}
	}
	if tokenMap, ok := metadata["token"].(map[string]any); ok {
		if at, ok := tokenMap["access_token"].(string); ok {
			if v := strings.TrimSpace(at); v != "" {
				return v
			}
		}
	}
	return ""
}

// jsonEqual compares two JSON blobs by parsing them into Go objects and deep comparing.
func jsonEqual(a, b []byte) bool {
	var objA any
	var objB any
	if err := json.Unmarshal(a, &objA); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &objB); err != nil {
		return false
	}
	return deepEqualJSON(objA, objB)
}

func deepEqualJSON(a, b any) bool {
	switch valA := a.(type) {
	case map[string]any:
		valB, ok := b.(map[string]any)
		if !ok || len(valA) != len(valB) {
			return false
		}
		for key, subA := range valA {
			subB, ok1 := valB[key]
			if !ok1 || !deepEqualJSON(subA, subB) {
				return false
			}
		}
		return true
	case []any:
		sliceB, ok := b.([]any)
		if !ok || len(valA) != len(sliceB) {
			return false
		}
		for i := range valA {
			if !deepEqualJSON(valA[i], sliceB[i]) {
				return false
			}
		}
		return true
	case float64:
		valB, ok := b.(float64)
		if !ok {
			return false
		}
		return valA == valB
	case string:
		valB, ok := b.(string)
		if !ok {
			return false
		}
		return valA == valB
	case bool:
		valB, ok := b.(bool)
		if !ok {
			return false
		}
		return valA == valB
	case nil:
		return b == nil
	default:
		return false
	}
}
