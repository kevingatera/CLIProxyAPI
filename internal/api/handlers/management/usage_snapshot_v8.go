package management

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// maxUsageImportBytes bounds one imported snapshot.
const maxUsageImportBytes = 256 << 20

// ExportUsageSnapshotV8 returns the durable usage snapshot. API keys appear only as fingerprints.
func (h *Handler) ExportUsageSnapshotV8(c *gin.Context) {
	data, err := usage.EncodeSnapshot(usage.GetRequestStatistics().Snapshot())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode usage snapshot"})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

// ImportUsageSnapshotV8 merges a version 2 snapshot. Snapshots of any other shape, and
// snapshots whose API key names or sources are not fingerprints, are rejected.
func (h *Handler) ImportUsageSnapshotV8(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxUsageImportBytes))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read usage snapshot body"})
		return
	}
	snapshot, err := usage.DecodeSnapshot(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := usage.GetRequestStatistics().MergeSnapshot(snapshot)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, usage.ErrOverlappingSnapshot) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
