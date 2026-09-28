package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"sync"

	"github.com/sirupsen/logrus"
)

// PluginManager manages dynamically loaded Go plugins.
// This is a Go port of the Python edgelite/engine/plugin_manager.py.
//
// In Go, plugins are loaded via the standard `plugin` package.
// This requires CGO and only works on Linux/macOS (not Windows).
// On Windows, plugins are simulated (registered but not loaded).
//
// Plugin interface:
//   - Each plugin must export a `NewPlugin` function returning a Plugin interface
//   - Plugins are initialized with a config map

// Plugin is the interface that all plugins must implement.
type Plugin interface {
	Name() string
	Version() string
	Init(config map[string]interface{}) error
	Start() error
	Stop() error
}

// PluginInfo holds metadata about a registered plugin.
type PluginInfo struct {
	Name     string                 `json:"name"`
	Version  string                 `json:"version"`
	FilePath string                 `json:"file_path"`
	Enabled  bool                   `json:"enabled"`
	Status   string                 `json:"status"`
	Config   map[string]interface{} `json:"config,omitempty"`
}

// PluginManager manages dynamic plugins.
type PluginManager struct {
	mu      sync.RWMutex
	plugins map[string]*PluginInfo
	loaded  map[string]Plugin
	dir     string
}

// NewPluginManager creates a new PluginManager.
func NewPluginManager(dir string) *PluginManager {
	return &PluginManager{
		plugins: make(map[string]*PluginInfo),
		loaded:  make(map[string]Plugin),
		dir:     dir,
	}
}

// DiscoverPlugins scans the plugin directory for .so files.
func (pm *PluginManager) DiscoverPlugins() ([]PluginInfo, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(pm.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read plugin dir: %w", err)
	}

	var result []PluginInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".so" && ext != ".dll" {
			continue
		}

		name := entry.Name()[:len(entry.Name())-len(ext)]
		info := &PluginInfo{
			Name:     name,
			FilePath: filepath.Join(pm.dir, entry.Name()),
			Enabled:  true,
			Status:   "discovered",
		}
		pm.plugins[name] = info
		result = append(result, *info)
	}

	logrus.WithField("count", len(result)).Info("Plugins discovered")
	return result, nil
}

// LoadPlugin loads a plugin by name.
func (pm *PluginManager) LoadPlugin(name string, config map[string]interface{}) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	info, ok := pm.plugins[name]
	if !ok {
		return fmt.Errorf("plugin not found: %s", name)
	}

	if !info.Enabled {
		return fmt.Errorf("plugin disabled: %s", name)
	}

	// On Windows, skip actual plugin loading
	if filepath.Separator == '\\' {
		info.Status = "loaded (simulated)"
		logrus.WithField("plugin", name).Info("Plugin loaded (simulated on Windows)")
		return nil
	}

	// Load the Go plugin
	p, err := plugin.Open(info.FilePath)
	if err != nil {
		info.Status = "load_error"
		return fmt.Errorf("failed to load plugin %s: %w", name, err)
	}

	// Look up the NewPlugin symbol
	sym, err := p.Lookup("NewPlugin")
	if err != nil {
		info.Status = "symbol_error"
		return fmt.Errorf("NewPlugin symbol not found in %s: %w", name, err)
	}

	newPluginFunc, ok := sym.(func() Plugin)
	if !ok {
		info.Status = "type_error"
		return fmt.Errorf("NewPlugin in %s does not return Plugin interface", name)
	}

	pluginInstance := newPluginFunc()
	if err := pluginInstance.Init(config); err != nil {
		info.Status = "init_error"
		return fmt.Errorf("plugin %s init failed: %w", name, err)
	}

	pm.loaded[name] = pluginInstance
	info.Status = "loaded"
	info.Version = pluginInstance.Version()
	info.Config = config

	logrus.WithFields(logrus.Fields{
		"plugin":  name,
		"version": info.Version,
	}).Info("Plugin loaded successfully")
	return nil
}

// StartPlugin starts a loaded plugin.
func (pm *PluginManager) StartPlugin(name string) error {
	pm.mu.RLock()
	pluginInstance, ok := pm.loaded[name]
	info := pm.plugins[name]
	pm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("plugin not loaded: %s", name)
	}

	if err := pluginInstance.Start(); err != nil {
		if info != nil {
			info.Status = "start_error"
		}
		return fmt.Errorf("plugin %s start failed: %w", name, err)
	}

	pm.mu.Lock()
	if info != nil {
		info.Status = "running"
	}
	pm.mu.Unlock()

	logrus.WithField("plugin", name).Info("Plugin started")
	return nil
}

// StopPlugin stops a running plugin.
func (pm *PluginManager) StopPlugin(name string) error {
	pm.mu.RLock()
	pluginInstance, ok := pm.loaded[name]
	info := pm.plugins[name]
	pm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("plugin not loaded: %s", name)
	}

	if err := pluginInstance.Stop(); err != nil {
		return fmt.Errorf("plugin %s stop failed: %w", name, err)
	}

	pm.mu.Lock()
	if info != nil {
		info.Status = "stopped"
	}
	pm.mu.Unlock()

	logrus.WithField("plugin", name).Info("Plugin stopped")
	return nil
}

// UnloadPlugin unloads a plugin.
func (pm *PluginManager) UnloadPlugin(name string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, ok := pm.loaded[name]; !ok {
		return fmt.Errorf("plugin not loaded: %s", name)
	}

	delete(pm.loaded, name)
	if info, ok := pm.plugins[name]; ok {
		info.Status = "unloaded"
	}

	logrus.WithField("plugin", name).Info("Plugin unloaded")
	return nil
}

// ListPlugins returns all registered plugins.
func (pm *PluginManager) ListPlugins() []PluginInfo {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	result := make([]PluginInfo, 0, len(pm.plugins))
	for _, info := range pm.plugins {
		result = append(result, *info)
	}
	return result
}

// GetPluginInfo returns info about a specific plugin.
func (pm *PluginManager) GetPluginInfo(name string) (*PluginInfo, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	info, ok := pm.plugins[name]
	return info, ok
}

// StopAll stops and unloads all plugins.
func (pm *PluginManager) StopAll() {
	pm.mu.RLock()
	loaded := make(map[string]Plugin, len(pm.loaded))
	for k, v := range pm.loaded {
		loaded[k] = v
	}
	pm.mu.RUnlock()

	for name, p := range loaded {
		if err := p.Stop(); err != nil {
			logrus.WithError(err).WithField("plugin", name).Warn("Failed to stop plugin")
		}
	}

	pm.mu.Lock()
	for _, info := range pm.plugins {
		info.Status = "stopped"
	}
	pm.loaded = make(map[string]Plugin)
	pm.mu.Unlock()

	logrus.Info("All plugins stopped")
}
