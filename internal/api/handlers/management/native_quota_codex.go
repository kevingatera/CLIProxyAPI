package management

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/codexusage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var errNativeQuotaCredentialMissing = errors.New("provider access token or account_id not found")

// fetchCodexQuota reads Codex account usage for the unified refresher. The read and the report
// shape are shared with the quota-aware selector through sdk/codexusage.
func (h *Handler) fetchCodexQuota(ctx context.Context, a *coreauth.Auth) (pluginapi.QuotaFetchResponse, bool, error) {
	token, _ := a.Metadata["access_token"].(string)
	accountID, _ := a.Metadata["account_id"].(string)
	if strings.TrimSpace(token) == "" || strings.TrimSpace(accountID) == "" {
		return pluginapi.QuotaFetchResponse{}, true, errNativeQuotaCredentialMissing
	}
	// Config reload replaces h.cfg; proxy selection and the served model list read one snapshot under the lock.
	h.mu.Lock()
	transport := h.apiCallTransport(a, "")
	models := codexUnifiedModels(h.cfg, a)
	h.mu.Unlock()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	headers, err := codexusage.Fetch(ctx, client, token, accountID)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, true, err
	}
	now := time.Now()
	return coreauth.CodexUnifiedReport(codexusage.Read(headers.Get, now), models, now), true, nil
}

// codexUnifiedModels lists the upstream wire models of the unified routes this credential can serve.
// Its windows are account-wide, so the report applies them to exactly these models and to no others.
func codexUnifiedModels(cfg *config.Config, a *coreauth.Auth) []string {
	if cfg == nil || !cfg.Routing.UnifiedModels.Enabled {
		return nil
	}
	var models []string
	for _, unified := range cfg.Routing.UnifiedModels.Models {
		for _, route := range unified.Routes {
			if !coreauth.MatchesUnifiedRoute(route, a) || !registry.GetGlobalRegistry().ClientSupportsModel(a.ID, route.Source) {
				continue
			}
			if !slices.Contains(models, route.Model) {
				models = append(models, route.Model)
			}
		}
	}
	return models
}
