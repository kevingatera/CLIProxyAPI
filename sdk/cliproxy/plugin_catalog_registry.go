package cliproxy

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// pluginCatalogRegistry is the registry surface used to publish static plugin
// catalogs. It matches the methods the pluginhost uses.
type pluginCatalogRegistry interface {
	RegisterClient(clientID, clientProvider string, models []*registry.ModelInfo)
	UnregisterClient(clientID string)
	GetModelProviders(modelID string) []string
}

// globalExclusionCatalogRegistry applies the live global exclusion list to static
// plugin catalogs. It reads the list under cfgMu while the registry write runs, so
// a config commit cannot interleave with a publication that uses an older list.
// A config reload re-runs plugin model registration, which republishes catalogs.
type globalExclusionCatalogRegistry struct {
	service *Service
	next    pluginCatalogRegistry
}

// pluginCatalogRegistry returns the registry that static plugin catalogs publish through.
func (s *Service) pluginCatalogRegistry() pluginCatalogRegistry {
	return globalExclusionCatalogRegistry{service: s, next: registry.GetGlobalRegistry()}
}

func (r globalExclusionCatalogRegistry) RegisterClient(clientID, clientProvider string, models []*registry.ModelInfo) {
	if r.service == nil {
		r.next.RegisterClient(clientID, clientProvider, models)
		return
	}
	r.service.cfgMu.RLock()
	defer r.service.cfgMu.RUnlock()
	var patterns []string
	if r.service.cfg != nil {
		patterns = r.service.cfg.GlobalExcludedModels
	}
	r.next.RegisterClient(clientID, clientProvider, filterGlobalExcludedModels(models, patterns))
}

func (r globalExclusionCatalogRegistry) UnregisterClient(clientID string) {
	r.next.UnregisterClient(clientID)
}

func (r globalExclusionCatalogRegistry) GetModelProviders(modelID string) []string {
	return r.next.GetModelProviders(modelID)
}
