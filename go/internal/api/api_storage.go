package api

import (
	"net/http"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/srcfl/ftw/go/internal/state"
)

const storageInventoryFormat = "ftw-core-storage-inventory-v1"

type storageFilesystem struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

type storageInventoryResponse struct {
	Format        string                `json:"format"`
	GeneratedAtMs int64                 `json:"generated_at_ms"`
	ReadOnly      bool                  `json:"read_only"`
	Databases     state.SQLiteInventory `json:"databases"`
	Filesystem    storageFilesystem     `json:"filesystem"`
}

// handleStorageInventory reads Core's SQLite page use and the data volume's
// free space. It does not scan storage-engine files or run maintenance.
func (s *Server) handleStorageInventory(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil || s.deps.DataDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage inventory unavailable"})
		return
	}

	databases, err := s.deps.State.SQLiteInventory(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage inventory unavailable"})
		return
	}
	usage, err := disk.UsageWithContext(r.Context(), s.deps.DataDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage inventory unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, storageInventoryResponse{
		Format:        storageInventoryFormat,
		GeneratedAtMs: time.Now().UnixMilli(),
		ReadOnly:      true,
		Databases:     databases,
		Filesystem: storageFilesystem{
			TotalBytes: usage.Total, UsedBytes: usage.Used,
			AvailableBytes: usage.Free, UsedPercent: usage.UsedPercent,
		},
	})
}
