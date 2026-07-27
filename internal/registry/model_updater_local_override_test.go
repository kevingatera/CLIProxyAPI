package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withTempModelStore swaps the global catalog store for the duration of fn and
// restores it afterwards, so tests do not leak catalog state.
func withTempModelStore(t *testing.T, fn func()) {
	t.Helper()
	modelsCatalogStore.mu.Lock()
	saved := modelsCatalogStore.data
	modelsCatalogStore.mu.Unlock()
	defer func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = saved
		modelsCatalogStore.mu.Unlock()
	}()
	fn()
}

func findKimiModel(id string) *ModelInfo {
	for _, m := range getModels().Kimi {
		if m != nil && m.ID == id {
			return m
		}
	}
	return nil
}

// TestLoadModelsAppliesLocalOverridesOnRemoteRefresh simulates a remote catalog
// refresh (which lacks homelab-local models) and verifies the local override
// entries are merged in afterwards.
func TestLoadModelsAppliesLocalOverridesOnRemoteRefresh(t *testing.T) {
	fakeRemote := []byte(`{
		"kimi": [
			{"id": "kimi-k3", "object": "model", "owned_by": "moonshot", "type": "kimi",
			 "context_length": 262144, "max_completion_tokens": 65536,
			 "thinking": {"zero_allowed": false, "levels": ["low", "high", "max"]}}
		]
	}`)

	withTempModelStore(t, func() {
		if err := loadModelsFromBytes(fakeRemote, "test-remote"); err != nil {
			t.Fatalf("loadModelsFromBytes() error = %v", err)
		}
		got := findKimiModel("kimi-k3-256k")
		if got == nil {
			t.Fatal("kimi-k3-256k missing after remote catalog load; local override not applied")
		}
		if got.ContextLength != 262144 {
			t.Errorf("kimi-k3-256k context_length = %d, want 262144", got.ContextLength)
		}
		if got.Thinking == nil || len(got.Thinking.Levels) != 3 {
			t.Errorf("kimi-k3-256k thinking = %+v, want 3 levels", got.Thinking)
		}
		// Remote catalog content must be preserved alongside the override.
		if findKimiModel("kimi-k3") == nil {
			t.Error("kimi-k3 from remote catalog missing after override merge")
		}
	})
}

// TestTryRefreshModelsAppliesLocalOverrides exercises the production remote
// refresh path (tryRefreshModels -> fetchModelsFromRemote), which replaces the
// whole catalog store; local overrides must be re-applied there too. This is
// the path that runs at startup and every 3h in production.
func TestTryRefreshModelsAppliesLocalOverrides(t *testing.T) {
	fakeRemote := `{
		"kimi": [
			{"id": "kimi-k3", "object": "model", "owned_by": "moonshot", "type": "kimi",
			 "context_length": 262144, "max_completion_tokens": 65536,
			 "thinking": {"zero_allowed": false, "levels": ["low", "high", "max"]}}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeRemote))
	}))
	t.Cleanup(srv.Close)

	savedURLs := modelsURLs
	modelsURLs = []string{srv.URL}
	t.Cleanup(func() { modelsURLs = savedURLs })

	withTempModelStore(t, func() {
		tryRefreshModels(context.Background(), "test refresh")
		if findKimiModel("kimi-k3-256k") == nil {
			t.Fatal("kimi-k3-256k missing after tryRefreshModels; local override wiped by remote refresh")
		}
		if findKimiModel("kimi-k3") == nil {
			t.Error("kimi-k3 from remote catalog missing after refresh")
		}
	})
}

// TestUpsertModelInfosReplace verifies an override entry replaces an existing
// model with the same ID instead of duplicating it.
func TestUpsertModelInfosReplace(t *testing.T) {
	dst := []*ModelInfo{
		{ID: "kimi-k3", ContextLength: 262144},
		{ID: "kimi-k2", ContextLength: 131072},
	}
	out := upsertModelInfos(dst,
		&ModelInfo{ID: "Kimi-K3", ContextLength: 1048576},
		nil,
		&ModelInfo{ID: "  ", ContextLength: 1},
		&ModelInfo{ID: "kimi-k3-256k", ContextLength: 262144},
	)
	if len(out) != 3 {
		t.Fatalf("upsertModelInfos() len = %d, want 3", len(out))
	}
	byID := make(map[string]int, len(out))
	for _, m := range out {
		byID[m.ID] = m.ContextLength
	}
	if byID["Kimi-K3"] != 1048576 && byID["kimi-k3"] != 1048576 {
		t.Errorf("kimi-k3 context_length = %d, want replaced value 1048576", byID["kimi-k3"])
	}
	if byID["kimi-k2"] != 131072 {
		t.Errorf("kimi-k2 context_length = %d, want untouched 131072", byID["kimi-k2"])
	}
	if _, ok := byID["kimi-k3-256k"]; !ok {
		t.Error("kimi-k3-256k missing, want appended")
	}
}

// TestEmbeddedCatalogIncludesLocalOverrides verifies the override is also
// applied when loading the embedded fallback catalog at startup.
func TestEmbeddedCatalogIncludesLocalOverrides(t *testing.T) {
	withTempModelStore(t, func() {
		if err := loadModelsFromBytes(embeddedModelsJSON, "test-embed"); err != nil {
			t.Fatalf("loadModelsFromBytes(embedded) error = %v", err)
		}
		if findKimiModel("kimi-k3-256k") == nil {
			t.Fatal("kimi-k3-256k missing after embedded catalog load")
		}
	})
}
