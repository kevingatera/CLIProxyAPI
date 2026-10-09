package config

import (
	"fmt"
	"strings"
)

// UnifiedModels presents verified equivalents under one client-facing model ID.
type UnifiedModels struct {
	Enabled      bool           `yaml:"enabled" json:"enabled"`
	BareNames    bool           `yaml:"bare-names" json:"bare-names"`
	ExposeLegacy bool           `yaml:"expose-legacy" json:"expose-legacy"`
	Models       []UnifiedModel `yaml:"models,omitempty" json:"models,omitempty"`
}
type UnifiedModel struct {
	ContextLength int                 `yaml:"context-length,omitempty" json:"context-length,omitempty"`
	ID            string              `yaml:"id" json:"id"`
	Routes        []UnifiedModelRoute `yaml:"routes" json:"routes"`
}
type UnifiedModelRoute struct {
	Priority         int    `yaml:"priority,omitempty" json:"priority,omitempty"`
	SubscriptionOnly bool   `yaml:"subscription-only,omitempty" json:"subscription-only,omitempty"`
	AuthKind         string `yaml:"auth-kind,omitempty" json:"auth-kind,omitempty"`
	Provider         string `yaml:"provider" json:"provider"`
	// Source is the already registered model ID, including its provider prefix.
	Source string `yaml:"source" json:"source"`
	Model  string `yaml:"model" json:"model"`
	// Plan is included or metered. It describes the operator's plan, not model identity.
	Plan string `yaml:"plan" json:"plan"`
}

func (u UnifiedModels) PublicID(id string) string {
	if u.BareNames {
		return id
	}
	return "cliproxy/" + id
}
func (u UnifiedModels) Find(id string) *UnifiedModel {
	if !u.Enabled {
		return nil
	}
	for i := range u.Models {
		if u.PublicID(u.Models[i].ID) == id {
			return &u.Models[i]
		}
	}
	return nil
}
func (r UnifiedModelRoute) Matches(provider, compatName string) bool {
	return strings.EqualFold(r.Provider, provider) || (compatName != "" && strings.EqualFold(r.Provider, compatName))
}

func (u UnifiedModels) Validate() error {
	if u.Enabled && len(u.Models) == 0 {
		return fmt.Errorf("unified-models requires at least one model")
	}
	seen := map[string]bool{}
	for _, model := range u.Models {
		if model.ID == "" || strings.ContainsAny(model.ID, " /\t\n()") || seen[model.ID] {
			return fmt.Errorf("unified-models requires unique canonical IDs without spaces, slashes or thinking suffixes")
		}
		seen[model.ID] = true
		if len(model.Routes) == 0 {
			return fmt.Errorf("unified model %s has no routes", model.ID)
		}
		providers := map[string]bool{}
		for _, route := range model.Routes {
			if route.Provider == "" || route.Source == "" || route.Model == "" || providers[strings.ToLower(route.Provider)] {
				return fmt.Errorf("unified model %s requires one verified source and upstream model per provider", model.ID)
			}
			providers[strings.ToLower(route.Provider)] = true
			if route.AuthKind != "" && route.AuthKind != "oauth" && route.AuthKind != "apikey" {
				return fmt.Errorf("unified model %s auth-kind must be oauth or apikey", model.ID)
			}
			if route.SubscriptionOnly && route.AuthKind != "oauth" {
				return fmt.Errorf("unified model %s subscription-only requires auth-kind oauth", model.ID)
			}
			if route.Plan != "included" && route.Plan != "metered" {
				return fmt.Errorf("unified model %s plan must be included or metered", model.ID)
			}
		}
	}
	return nil
}
