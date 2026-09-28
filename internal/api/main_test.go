package api

// TestMain isolates the config file for the whole package.
//
// Most handlers here persist through config.SaveConfig(cfg, ""), which falls
// back to the packaged configs/config.yaml. Without isolation a plain
// `go test ./internal/api/...` rewrites the shipped config: the YAML reflow
// drops every comment, so the operator's file is silently destroyed. Pointing
// EDGELITE_CONFIG at a throwaway copy before any handler runs keeps the values
// the tests read while redirecting everything they write.

import (
	"os"
	"path/filepath"
	"testing"
)

// shippedConfig returns the repository's own configs/config.yaml.
//
// The path is resolved from the module root rather than the working directory:
// `go test ./internal/api/...` runs with the package directory as cwd, where a
// leftover configs/config.yaml (written by an earlier, unisolated run, with an
// empty secret_key) would otherwise shadow the real one.
func shippedConfig(t *testing.T) ([]byte, string) {
	t.Helper()
	path, err := shippedConfigPath()
	if err != nil {
		return nil, ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ""
	}
	return data, path
}

func shippedConfigPath() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			candidate := filepath.Join(dir, "configs", "config.yaml")
			if _, statErr := os.Stat(candidate); statErr != nil {
				return "", os.ErrNotExist
			}
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "edgelite-api-test-config")
	if err != nil {
		panic("cannot create temp config dir: " + err.Error())
	}
	tmpPath := filepath.Join(dir, "config.yaml")
	if path, err := shippedConfigPath(); err == nil {
		// A copy keeps the config values the tests already depend on.
		if src, rerr := os.ReadFile(path); rerr == nil {
			if werr := os.WriteFile(tmpPath, src, 0600); werr != nil {
				panic("cannot copy test config: " + werr.Error())
			}
		}
	}
	previous, hadPrevious := os.LookupEnv("EDGELITE_CONFIG")
	os.Setenv("EDGELITE_CONFIG", tmpPath)

	code := m.Run()

	if hadPrevious {
		os.Setenv("EDGELITE_CONFIG", previous)
	} else {
		os.Unsetenv("EDGELITE_CONFIG")
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
