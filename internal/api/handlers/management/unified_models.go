package management

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

// PatchUnifiedModels updates presentation switches without rewriting the YAML document.
func (h *Handler) PatchUnifiedModels(c *gin.Context) {
	var body struct {
		Enabled      *bool `json:"enabled"`
		BareNames    *bool `json:"bare_names"`
		ExposeLegacy *bool `json:"expose_legacy"`
	}
	if c.ShouldBindJSON(&body) != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	before, err := os.ReadFile(h.configFilePath)
	if err != nil {
		c.JSON(500, gin.H{"error": "config read failed"})
		return
	}
	updates := map[string]*bool{"enabled": body.Enabled, "bare-names": body.BareNames, "expose-legacy": body.ExposeLegacy}
	after, err := patchUnifiedPresentation(before, updates)
	if err != nil {
		c.JSON(409, gin.H{"error": err.Error()})
		return
	}
	next, err := config.ParseConfigBytes(after)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid unified model configuration"})
		return
	}
	if !bytes.Equal(before, after) {
		backup := h.configFilePath + ".bak." + time.Now().UTC().Format("20060102-150405.000000000")
		if err = os.WriteFile(backup, before, 0600); err != nil {
			c.JSON(500, gin.H{"error": "config backup failed"})
			return
		}
		if err = WriteConfig(h.configFilePath, after); err != nil {
			c.JSON(500, gin.H{"error": "config write failed"})
			return
		}
	}
	next.Home = h.cfg.Home
	h.cfg = next
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), h.reloadSnapshotConfigLocked())
	c.JSON(200, gin.H{"status": "ok"})
}
func patchUnifiedPresentation(data []byte, updates map[string]*bool) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid config")
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty config")
	}
	node := configV8Node(doc.Content[0], []string{"routing", "unified-models"})
	if node == nil {
		return nil, fmt.Errorf("configure routing.unified-models before changing its presentation")
	}
	lines := strings.Split(string(data), "\n")
	for key, value := range updates {
		if value == nil {
			continue
		}
		scalar := configV8Node(node, []string{key})
		if scalar == nil || scalar.Kind != yaml.ScalarNode || scalar.Tag != "!!bool" {
			return nil, fmt.Errorf("unified-models.%s must be an explicit boolean", key)
		}
		line := scalar.Line - 1
		expression := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(key) + `:\s*)(true|false)(\s*(?:#.*)?)$`)
		matches := expression.FindStringSubmatch(lines[line])
		if matches == nil {
			return nil, fmt.Errorf("unified-models.%s needs a block-style boolean", key)
		}
		lines[line] = matches[1] + strconv.FormatBool(*value) + matches[3]
	}
	return []byte(strings.Join(lines, "\n")), nil
}
