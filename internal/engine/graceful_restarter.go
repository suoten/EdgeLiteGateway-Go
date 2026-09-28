package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GracefulRestarter manages graceful restarts of the gateway.
// It uses a marker file to detect if the previous shutdown was clean.
type GracefulRestarter struct {
	markerPath string
}

// RestartMarker represents the marker file content.
type RestartMarker struct {
	OldVersion string    `json:"old_version"`
	NewVersion string    `json:"new_version"`
	Success    bool      `json:"success"`
	Timestamp  time.Time `json:"timestamp"`
	PID        int       `json:"pid"`
}

// NewGracefulRestarter creates a new GracefulRestarter.
func NewGracefulRestarter(dataDir string) *GracefulRestarter {
	return &GracefulRestarter{
		markerPath: filepath.Join(dataDir, "restart_marker.json"),
	}
}

// WriteMarker writes a restart marker file.
func (g *GracefulRestarter) WriteMarker(oldVersion, newVersion string, success bool) error {
	marker := RestartMarker{
		OldVersion: oldVersion,
		NewVersion: newVersion,
		Success:    success,
		Timestamp:  time.Now(),
		PID:        os.Getpid(),
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal restart marker: %w", err)
	}
	return os.WriteFile(g.markerPath, data, 0644)
}

// CheckAndCleanupMarker checks for an existing marker and cleans it up.
// Returns the marker if found, nil otherwise.
func (g *GracefulRestarter) CheckAndCleanupMarker() (*RestartMarker, error) {
	data, err := os.ReadFile(g.markerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read restart marker: %w", err)
	}
	var marker RestartMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		// Corrupted marker, remove it
		_ = os.Remove(g.markerPath)
		return nil, fmt.Errorf("failed to parse restart marker: %w", err)
	}
	// Clean up the marker
	_ = os.Remove(g.markerPath)
	return &marker, nil
}

// GetMarkerPath returns the path to the marker file.
func (g *GracefulRestarter) GetMarkerPath() string {
	return g.markerPath
}
