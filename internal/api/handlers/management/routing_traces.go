package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const maxRoutingTracesPerResponse = 2000

// GetRoutingTraces returns recent routing policy traces, newest first.
// Query: limit (default 100), model, provider, failed (bool).
func (h *Handler) GetRoutingTraces(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, errParse := strconv.Atoi(raw)
		if errParse != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		limit = min(parsed, maxRoutingTracesPerResponse)
	}
	failedOnly := false
	if raw := strings.TrimSpace(c.Query("failed")); raw != "" {
		parsed, errParse := strconv.ParseBool(raw)
		if errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed must be a boolean"})
			return
		}
		failedOnly = parsed
	}
	traces := h.authManager.ListRoutingTraces(limit, c.Query("model"), c.Query("provider"), failedOnly)
	c.JSON(http.StatusOK, gin.H{"traces": traces, "count": len(traces)})
}
