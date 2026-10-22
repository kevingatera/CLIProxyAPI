package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// localModelPatchesJSON holds field-level corrections to upstream catalog entries.
// Each patch names a model and lists only the fields it changes.
//
//go:embed models/models.local.json
var localModelPatchesJSON []byte

// localModelPatchFile is the shape of models.local.json. Sections are provider-scoped:
// a patch under "kimi" only touches the Kimi section.
type localModelPatchFile struct {
	Source  string            `json:"source"`
	Checked string            `json:"checked"`
	Kimi    []localModelPatch `json:"kimi"`
}

// localModelPatch uses pointers so an absent field is distinct from an explicit zero value.
type localModelPatch struct {
	ID            string              `json:"id"`
	ContextLength *int                `json:"context_length,omitempty"`
	Thinking      *localThinkingPatch `json:"thinking,omitempty"`
}

type localThinkingPatch struct {
	ZeroAllowed *bool `json:"zero_allowed,omitempty"`
}

func loadLocalModelPatches() (*localModelPatchFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(localModelPatchesJSON))
	decoder.DisallowUnknownFields()
	var parsed localModelPatchFile
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode local model patches: %w", err)
	}
	return &parsed, nil
}

// applyEmbeddedLocalModelPatches applies models/models.local.json to a freshly loaded
// catalog. It runs on startup and on every remote refresh, because a refresh replaces
// the whole catalog. On failure the catalog keeps its upstream values.
func applyEmbeddedLocalModelPatches(catalog *staticModelsJSON) {
	patches, err := loadLocalModelPatches()
	if err == nil {
		err = applyLocalModelPatches(catalog, patches)
	}
	if err != nil {
		log.Warnf("registry: skipping local model patches: %v", err)
	}
}

// applyLocalModelPatches merges patches onto catalog. It works on a copy and replaces
// catalog only when the merged result passes the upstream validator.
func applyLocalModelPatches(catalog *staticModelsJSON, patches *localModelPatchFile) error {
	kimi, err := patchModelInfos(catalog.Kimi, patches.Kimi)
	if err != nil {
		return fmt.Errorf("kimi: %w", err)
	}
	candidate := *catalog
	candidate.Kimi = kimi
	if err := validateModelsCatalog(&candidate); err != nil {
		return err
	}
	*catalog = candidate
	return nil
}

func patchModelInfos(models []*ModelInfo, patches []localModelPatch) ([]*ModelInfo, error) {
	if len(patches) == 0 {
		return models, nil
	}
	out := make([]*ModelInfo, len(models))
	index := make(map[string]int, len(models))
	for i, model := range models {
		out[i] = cloneModelInfo(model)
		if model != nil {
			index[strings.TrimSpace(model.ID)] = i
		}
	}

	seen := make(map[string]struct{}, len(patches))
	for _, patch := range patches {
		id := strings.TrimSpace(patch.ID)
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("duplicate patch for model %q", id)
		}
		seen[id] = struct{}{}

		i, ok := index[id]
		if !ok {
			log.Warnf("registry: local model patch for %q has no upstream entry, skipping", id)
			continue
		}
		model := out[i]
		if patch.ContextLength != nil {
			if *patch.ContextLength <= 0 {
				return nil, fmt.Errorf("model %q: context_length must be positive", id)
			}
			model.ContextLength = *patch.ContextLength
		}
		if patch.Thinking != nil {
			if model.Thinking == nil {
				return nil, fmt.Errorf("model %q: upstream has no thinking metadata to patch", id)
			}
			if patch.Thinking.ZeroAllowed != nil {
				model.Thinking.ZeroAllowed = *patch.Thinking.ZeroAllowed
			}
		}
	}
	return out, nil
}
