package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// GetRoutingPolicy returns the full routing policy object.
func (h *Handler) GetRoutingPolicy(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusOK, gin.H{"policy": config.RoutingPolicy{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"policy": h.cfg.Routing.Policy})
}

func decodeRoutingPolicyPayload(c *gin.Context) (config.RoutingPolicy, error) {
	var raw map[string]any
	if err := c.ShouldBindJSON(&raw); err != nil {
		return config.RoutingPolicy{}, err
	}
	var candidate any = raw
	if nested, ok := raw["policy"]; ok {
		candidate = nested
	} else if nested, ok := raw["value"]; ok {
		candidate = nested
	}
	data, errMarshal := json.Marshal(candidate)
	if errMarshal != nil {
		return config.RoutingPolicy{}, errMarshal
	}
	var policy config.RoutingPolicy
	if errUnmarshal := json.Unmarshal(data, &policy); errUnmarshal != nil {
		return config.RoutingPolicy{}, errUnmarshal
	}
	return policy, nil
}

func (h *Handler) validateRoutingPolicy(policy config.RoutingPolicy) error {
	allowedTriggers := map[string]struct{}{
		"exhausted":       {},
		"rate_limited":    {},
		"server_error":    {},
		"transport_error": {},
	}
	for _, trigger := range policy.Fallback.On {
		if _, ok := allowedTriggers[strings.ToLower(strings.TrimSpace(trigger))]; !ok {
			return fmt.Errorf("invalid fallback trigger: %s", trigger)
		}
	}

	if policy.Observability.TraceLimit < 0 || policy.Observability.TraceLimit > 2000 {
		return fmt.Errorf("trace-limit must be within [0,2000]")
	}

	authProviderByID := make(map[string]string)
	if h != nil && h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil {
				continue
			}
			authID := strings.TrimSpace(auth.ID)
			if authID == "" {
				continue
			}
			authProviderByID[authID] = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
	}

	validateRule := func(scope string, rule config.RoutingPolicyRule) error {
		for _, route := range rule.Route {
			provider := strings.ToLower(strings.TrimSpace(route.Provider))
			if provider == "" {
				return fmt.Errorf("%s route provider is required", scope)
			}
			for _, authID := range route.AuthOrder {
				trimmedID := strings.TrimSpace(authID)
				if trimmedID == "" {
					continue
				}
				if len(authProviderByID) == 0 {
					continue
				}
				authProvider, ok := authProviderByID[trimmedID]
				if !ok {
					return fmt.Errorf("%s references unknown auth id: %s", scope, trimmedID)
				}
				if authProvider != provider {
					return fmt.Errorf("%s auth %s belongs to provider %s, not %s", scope, trimmedID, authProvider, provider)
				}
			}
		}
		return nil
	}

	if err := validateRule("defaults", policy.Defaults); err != nil {
		return err
	}
	for modelID, rule := range policy.ModelOverrides {
		modelKey := strings.TrimSpace(modelID)
		if modelKey == "" {
			return fmt.Errorf("model-overrides contains empty model key")
		}
		if err := validateRule(fmt.Sprintf("model-overrides[%s]", modelKey), rule); err != nil {
			return err
		}
	}

	return nil
}

// PutRoutingPolicy updates the full routing policy object.
func (h *Handler) PutRoutingPolicy(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	policy, errDecode := decodeRoutingPolicyPayload(c)
	if errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	scratch := *h.cfg
	scratch.Routing.Policy = policy
	scratch.SanitizeRoutingPolicy()

	if errValidate := h.validateRoutingPolicy(scratch.Routing.Policy); errValidate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
		return
	}

	h.cfg.Routing.Policy = scratch.Routing.Policy
	h.persist(c)
}

// PreviewRoutingPolicy previews resolved routing order and auth candidates for a model.
func (h *Handler) PreviewRoutingPolicy(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var body struct {
		Model        string   `json:"model"`
		Providers    []string `json:"providers"`
		PinnedAuthID string   `json:"pinned_auth_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	providers := make([]string, 0, len(body.Providers))
	for _, provider := range body.Providers {
		trimmedProvider := strings.TrimSpace(provider)
		if trimmedProvider != "" {
			providers = append(providers, trimmedProvider)
		}
	}
	if len(providers) == 0 {
		baseModel := strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
		if baseModel == "" {
			baseModel = model
		}
		providers = util.GetProviderName(baseModel)
	}
	if len(providers) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no providers available for model"})
		return
	}

	opts := cliproxyexecutor.Options{}
	if strings.TrimSpace(body.PinnedAuthID) != "" {
		opts.Metadata = map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey: strings.TrimSpace(body.PinnedAuthID),
		}
	}
	preview := h.authManager.PreviewRouting(model, providers, opts)
	c.JSON(http.StatusOK, gin.H{"preview": preview})
}

// GetRoutingTraces returns recent in-memory routing traces.
func (h *Handler) GetRoutingTraces(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	limit := 100
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		if parsedLimit, err := strconv.Atoi(rawLimit); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}
	model := strings.TrimSpace(c.Query("model"))
	provider := strings.TrimSpace(c.Query("provider"))
	failedOnly := false
	if rawFailed := strings.TrimSpace(c.Query("failed")); rawFailed != "" {
		if parsed, err := strconv.ParseBool(rawFailed); err == nil {
			failedOnly = parsed
		}
	}

	traces := h.authManager.ListRoutingTraces(limit, model, provider, failedOnly)
	c.JSON(http.StatusOK, gin.H{
		"traces": traces,
		"count":  len(traces),
	})
}
