package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// GetUsageStatistics serves GET /v0/management/usage for the management UI. Upstream
// removed this read; the fork restores it. It returns the persistent aggregate, in which
// API keys and upstream key sources appear only as fingerprints.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	snapshot := usage.GetRequestStatistics().Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}
