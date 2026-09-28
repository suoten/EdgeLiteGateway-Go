// Package main is the CLI entry point for EdgeLite Gateway (Go Edition).
//
// This is a 1:1 port of the Python edgelite/__main__.py + app.py create_app().
// It parses CLI args, loads config, bootstraps all services, registers routes,
// mounts the frontend SPA, and starts the Echo HTTP server with graceful shutdown.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/sirupsen/logrus"

	"edgelite/internal/api"
	"edgelite/internal/config"
	"edgelite/internal/drivers"
	"edgelite/internal/engine"
	elware "edgelite/internal/middleware"
	"edgelite/internal/northbound"
	"edgelite/internal/security"
	"edgelite/internal/services"
	"edgelite/internal/storage"
	"edgelite/internal/ws"
)

// Version is the application version, injected at build time or defaulted.
var Version = "1.0.0-go"

func main() {
	// Parse CLI arguments
	host := getEnvOrDefault("EDGELITE_SERVER__HOST", "")
	port := getEnvOrDefault("EDGELITE_SERVER__PORT", "")
	configPath := "configs/config.yaml"

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			if i+1 < len(args) {
				host = args[i+1]
				i++
			}
		case "--port":
			if i+1 < len(args) {
				port = args[i+1]
				i++
			}
		case "--config":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			}
		case "--help", "-h":
			printUsage()
			os.Exit(0)
		case "--version", "-v":
			fmt.Printf("EdgeLite Gateway (Go Edition) v%s\n", Version)
			os.Exit(0)
		}
	}

	// Set config path in env so config.GetConfig() can find it
	os.Setenv("EDGELITE_CONFIG", configPath)

	// /ota/check compares the release feed against this, so it has to be the version
	// the binary was linked with rather than a string the api package copied.
	api.SetGatewayVersion(Version)

	// Load configuration
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Failed to load config: %v\n", err)
		os.Exit(1)
	}
	config.SetGlobalConfig(cfg)

	// Listen address precedence: --host/--port > EDGELITE_SERVER__* > config.yaml > default.
	if host == "" {
		host = cfg.Server.Host
	}
	if port == "" {
		port = fmt.Sprintf("%d", cfg.Server.Port)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" || port == "0" {
		port = "8080"
	}

	// Configure logging
	configureLogging(cfg.Logging)

	logrus.WithFields(logrus.Fields{
		"version": Version,
		"host":    host,
		"port":    port,
		"config":  configPath,
	}).Info("Starting EdgeLite Gateway (Go Edition)")

	// Create data directories
	ensureDirs(cfg)

	// A restore can only be applied while no connection owns the database file,
	// so POST /system/backup/restore stages a marker and it lands here.
	applied, snapshot, err := storage.ApplyPendingRestore(cfg.Database.SQLitePath, cfg.BackupDirectory())
	if err != nil {
		logrus.WithError(err).Fatal("Staged database restore could not be applied; the gateway did not start")
	}
	if applied {
		fields := logrus.Fields{"database": cfg.Database.SQLitePath}
		if snapshot != "" {
			fields["pre_restore_snapshot"] = snapshot
		}
		logrus.WithFields(fields).Warn("Staged database restore applied")
	}

	// Bootstrap all services
	container, err := bootstrap(cfg)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to bootstrap services")
	}
	api.SetContainer(container)

	// Create Echo instance
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// A connection that is accepted but never sends a request header stays in
	// StateNew, and http.Server.Shutdown only force-closes idle connections. Without
	// a header budget one silent socket - an LB TCP probe, a port scan, a browser's
	// speculative connect - holds graceful shutdown until the grace period burns out
	// and the process is killed with the databases still open.
	e.Server.ReadHeaderTimeout = 15 * time.Second

	// Configure IP extractor based on trusted proxies configuration.
	// If trusted proxies are configured, use ExtractIPFromXFFHeader to
	// correctly resolve client IPs behind reverse proxies. Otherwise,
	// use DirectIPExtractor to prevent IP spoofing via X-Forwarded-For.
	if len(cfg.Server.TrustedProxies) > 0 {
		e.IPExtractor = echo.ExtractIPFromXFFHeader()
	} else {
		e.IPExtractor = echo.ExtractIPDirect()
	}

	// Custom HTTP error handler — returns standardized JSON error responses
	// and prevents internal error details from leaking to clients.
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		code := http.StatusInternalServerError
		message := "Internal Server Error"
		if he, ok := err.(*echo.HTTPError); ok {
			code = he.Code
			if msg, ok := he.Message.(string); ok {
				message = msg
			} else if msg, ok := he.Message.(fmt.Stringer); ok {
				message = msg.String()
			}
		}
		// Log internal errors for debugging, but don't expose details to client
		if code >= 500 {
			logrus.WithError(err).WithField("path", c.Request().URL.Path).Error("Internal server error")
		}
		_ = c.JSON(code, map[string]interface{}{
			"code":    code,
			"message": message,
			"data":    nil,
		})
	}

	// Configure middleware (order matters — outermost first)
	configureMiddleware(e, cfg)

	// Register API routes
	registerRoutes(e, cfg)

	// Mount frontend SPA
	mountFrontend(e, cfg)

	// Start engine components in background
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := startEngine(ctx, container, cfg); err != nil {
		logrus.WithError(err).Warn("Failed to start engine components (continuing anyway)")
	}

	// Start server
	addr := fmt.Sprintf("%s:%s", host, port)
	go func() {
		logrus.WithField("addr", addr).Info("HTTP server listening")
		if err := e.Start(addr); err != nil && err != http.ErrServerClosed {
			logrus.WithError(err).Fatal("Server failed")
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Ignore SIGHUP to prevent SSH disconnect from killing the process
	signal.Ignore(syscall.SIGHUP)

	sig := <-quit
	logrus.WithField("signal", sig.String()).Info("Shutdown signal received")

	// Give background components 30 seconds to shut down
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Stop engine components
	stopEngine(shutdownCtx, container)

	// Stop HTTP server, with its own slice of the grace period: stopEngine above may
	// already have spent most of it, and the engine is quiesced at this point, so a
	// client that refuses to finish is dropped rather than waited out - the databases
	// are closed next and must not be closed under a live request.
	httpCtx, httpCancel := context.WithTimeout(shutdownCtx, 10*time.Second)
	defer httpCancel()
	if err := e.Shutdown(httpCtx); err != nil {
		logrus.WithError(err).Error("HTTP server shutdown timed out, forcing connections closed")
		if cerr := e.Close(); cerr != nil {
			logrus.WithError(cerr).Error("HTTP server force close failed")
		}
	}

	// Close database
	if container.Database != nil {
		if err := container.Database.Close(); err != nil {
			logrus.WithError(err).Error("Database close error")
		}
	}
	if container.TsStorage != nil {
		if err := container.TsStorage.Close(); err != nil {
			logrus.WithError(err).Error("TS storage close error")
		}
	}

	logrus.Info("Application shutdown complete")
}

// printUsage prints CLI usage information.
func printUsage() {
	fmt.Print(`EdgeLite Gateway (Go Edition)

Usage:
  edgelite [options]

Options:
  --host string       Listen address (falls back to config server.host, then 127.0.0.1)
  --port string       Listen port (falls back to EDGELITE_SERVER__PORT, then config server.port, then 8080)
  --config string     Config file path (default "configs/config.yaml")
  --version, -v       Print version and exit
  --help, -h          Print this help and exit
`)
}

// getEnvOrDefault returns the env var value or the default if not set.
func getEnvOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// configureLogging sets up logrus based on config.
func configureLogging(cfg config.LoggingConfig) {
	level, err := logrus.ParseLevel(strings.ToLower(cfg.Level))
	if err != nil {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)

	if cfg.JSONFormat {
		logrus.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: time.RFC3339,
		})
	} else {
		logrus.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02 15:04:05",
		})
	}
}

// ensureDirs creates all required data directories.
func ensureDirs(cfg *config.AppConfig) {
	offlineDir := ""
	if cfg.MQTT.OfflineDBPath != "" {
		offlineDir = filepath.Dir(cfg.MQTT.OfflineDBPath)
	}
	dirs := []string{
		filepath.Dir(cfg.Database.SQLitePath),
		cfg.Database.BackupDir,
		cfg.BackupDirectory(),
		filepath.Dir(cfg.InfluxDB.SQLiteTSPath),
		cfg.Logging.LogDir,
		offlineDir,
	}
	for _, d := range dirs {
		if d != "" && d != "." {
			if err := os.MkdirAll(d, 0755); err != nil {
				logrus.WithError(err).WithField("dir", d).Warn("Failed to create directory")
			}
		}
	}
}

// bootstrap initializes all services and returns a ServiceContainer.
func bootstrap(cfg *config.AppConfig) (*api.ServiceContainer, error) {
	// Register all protocol drivers with infrastructure integration
	drivers.RegisterAll()

	// Database
	db, err := storage.NewDatabase(cfg)
	if err != nil {
		return nil, fmt.Errorf("database init: %w", err)
	}
	logrus.Info("Database initialized")

	// Repositories
	deviceRepo := storage.NewDeviceRepo(db)
	ruleRepo := storage.NewRuleRepo(db)
	alarmRepo := storage.NewAlarmRepo(db)
	userRepo := storage.NewUserRepo(db)
	templateRepo := storage.NewTemplateRepo(db)

	// Ensure a default admin user exists for first-time login.
	// Default credentials: admin / admin123 — should be changed after first login.
	defaultPasswordHash, err := security.HashPassword("admin123")
	if err != nil {
		return nil, fmt.Errorf("failed to hash default admin password: %w", err)
	}
	if err := storage.EnsureAdminUser(userRepo, "admin", defaultPasswordHash); err != nil {
		logrus.WithError(err).Warn("Failed to ensure admin user exists")
	} else {
		logrus.Info("Default admin user ensured (admin / admin123)")
	}

	// Time-series storage
	tsStorage, err := storage.NewTimeSeriesStorage(cfg)
	if err != nil {
		return nil, fmt.Errorf("TS storage init: %w", err)
	}
	logrus.Info("Time-series storage initialized")

	// Cache manager
	cacheCapacity := cfg.Cache.RingBufferCapacity
	if cacheCapacity <= 0 {
		cacheCapacity = 100000
	}
	cache := storage.NewCacheManager(cacheCapacity)

	// Offline queue for MQTT
	offlineQueue, err := storage.NewOfflineQueue(cfg.MQTT.OfflineDBPath)
	if err != nil {
		logrus.WithError(err).Warn("Offline queue init failed, continuing without it")
	}

	// Pre-create notifyService so engine components (e.g. AlarmOutbox) can reference it
	notifyService := services.NewNotifyService()
	notifyService.SetConfig(&cfg.Notify)

	// Engine components
	eventBus := engine.NewEventBus(10000)
	scheduler := engine.NewCollectScheduler(eventBus, tsStorage, cache, &cfg.Scheduler)
	cbRegistry := engine.NewCircuitBreakerRegistry(eventBus)
	evaluator := engine.NewRuleEvaluator(eventBus, ruleRepo, alarmRepo)
	aiInference := engine.NewAIInferenceEngine(&cfg.AiInference, eventBus)

	var mqttForwarder *engine.MQTTForwarder
	if offlineQueue != nil {
		mqttForwarder = engine.NewMQTTForwarder(&cfg.MQTT, eventBus, offlineQueue, cache)
	}

	// InferenceScheduler — manages concurrent AI inference tasks
	inferenceScheduler := engine.NewInferenceScheduler(cfg.AiInference.MaxConcurrentInferences)

	// DriverWatchdog — monitors driver health and auto-restarts stalled drivers
	driverWatchdog := engine.NewDriverWatchdog(func(deviceID string) error {
		logrus.WithField("device_id", deviceID).Warn("Driver watchdog triggered restart")
		// Stop and restart the collector for this device
		scheduler.StopCollector(deviceID)
		time.Sleep(2 * time.Second) // Brief cooldown before restart
		scheduler.StartCollector(deviceID)
		return nil
	})

	// AlarmOutbox — reliable alarm delivery with retry and dead-letter queue
	alarmOutbox := engine.NewAlarmOutbox(func(msg *engine.OutboxMessage) error {
		return notifyService.SendNotification(msg.Channels, msg.Title, msg.Message, msg.Severity)
	})

	// StreamComputeEngine — real-time stream processing
	streamCompute := engine.NewStreamComputeEngine(eventBus, tsStorage)

	// LogAggregator — aggregates logs from multiple sources
	logAggregator := engine.NewLogAggregator(cfg.Logging.LogDir)
	logrus.AddHook(engine.NewLogrusHook(logAggregator))

	// CascadeManager — multi-gateway cascade topology
	cascadeManager := engine.NewCascadeManager("", "", 0, "")

	// ProtocolBridgeManager — bridges data between protocols
	protocolBridge := engine.NewProtocolBridgeManager()

	// LinkageEvaluator — actuates target devices when a source point crosses a
	// configured threshold (see api.WireDeviceLinkage).
	linkageEvaluator := engine.NewLinkageEvaluator()

	// OTAManager — over-the-air firmware update management
	otaManager := engine.NewOTAManager(filepath.Join(filepath.Dir(cfg.Database.SQLitePath), "firmware"))

	// MqttServer — built-in MQTT broker
	mqttServer := engine.NewMqttServer()

	// SerialTCPBridge — bridges serial devices over TCP
	serialBridge := engine.NewSerialTCPBridge()

	// LifecycleManager — manages device lifecycle states
	lifecycleMgr := engine.NewLifecycleManager()

	// Edge preprocessing and derived points. The Preprocessor and its expression
	// engine used to be built by nobody, so config.preprocess and the whole
	// preprocessing page filtered nothing while reporting success. They are
	// created here and handed to the collect scheduler before it starts; the
	// rules and expressions they run come from the store in WirePreprocessing.
	preprocessRuleStore := storage.NewPreprocessRuleStore(db)
	expressionConfigStore := storage.NewExpressionConfigStore(db)
	preprocessor := engine.NewPreprocessor(&cfg.Preprocess)
	preprocessorExpressions := engine.NewPreprocessorExpressionEngine()
	scheduler.SetPreprocessor(preprocessor)
	scheduler.SetExpressionEngine(preprocessorExpressions)

	// WebSocket manager
	wsManager := ws.NewManager()

	// Bridge EventBus events to WebSocket for real-time push
	if eventBus != nil {
		eventBus.SubscribeAll(func(event engine.Event) {
			wsManager.BroadcastEvent(event)
		})
		logrus.Info("EventBus → WebSocket bridge established")
	}

	// Services
	deviceService := services.NewDeviceService(deviceRepo, templateRepo, scheduler, cbRegistry)
	ruleService := services.NewRuleService(ruleRepo, evaluator)
	alarmService := services.NewAlarmService(alarmRepo, evaluator)
	dataService := services.NewDataService(tsStorage, cache)
	systemService := services.NewSystemService(cfg)

	// Wire alarm service to notification service
	alarmService.SetNotifyService(notifyService)

	// Extended services (1:1 port of Python services/)
	alarmCorrelation := services.NewAlarmCorrelationService(alarmRepo)
	alarmSilence := services.NewAlarmSilenceService()
	backupCfg := cfg.Backup
	backupCfg.BackupDir = cfg.BackupDirectory()
	backupScheduler := services.NewBackupScheduler(backupCfg, cfg.Database.SQLitePath)
	dataImportExport := services.NewDataImportExportService(tsStorage)
	cmdApproval := services.NewCommandApprovalService()
	shadowService := services.NewShadowService()
	dbMonitor := services.NewDBMonitorService(cfg.Database.SQLitePath)
	mcpService := services.NewMCPService()
	videoService := services.NewVideoService()
	i18nService := services.NewI18nService()
	serviceManager := services.NewServiceManager()
	auditService := services.NewAuditService(db)
	histDataService := services.NewHistoricalDataService(tsStorage)

	// Config version manager
	configVersionMgr := drivers.NewConfigVersionManager(filepath.Join(filepath.Dir(cfg.Database.SQLitePath), "config_versions"))

	container := &api.ServiceContainer{
		Database:     db,
		DeviceRepo:   deviceRepo,
		RuleRepo:     ruleRepo,
		AlarmRepo:    alarmRepo,
		TemplateRepo: templateRepo,
		UserRepo:     userRepo,
		TsStorage:    tsStorage,
		Cache:        cache,

		ServiceEnabledMap: make(map[string]bool),

		DeviceService: deviceService,
		RuleService:   ruleService,
		AlarmService:  alarmService,
		DataService:   dataService,
		SystemService: systemService,
		NotifyService: notifyService,

		// Extended services
		AlarmCorrelation: alarmCorrelation,
		AlarmSilence:     alarmSilence,
		BackupScheduler:  backupScheduler,
		DataImportExport: dataImportExport,
		CmdApproval:      cmdApproval,
		ShadowService:    shadowService,
		DBMonitor:        dbMonitor,
		MCPService:       mcpService,
		VideoService:     videoService,
		I18nService:      i18nService,
		ServiceManager:   serviceManager,
		AuditService:     auditService,
		HistDataService:  histDataService,

		Scheduler:          scheduler,
		EventBus:           eventBus,
		CBRegistry:         cbRegistry,
		Evaluator:          evaluator,
		AIInference:        aiInference,
		MqttForward:        mqttForwarder,
		InferenceScheduler: inferenceScheduler,
		DriverWatchdog:     driverWatchdog,
		AlarmOutbox:        alarmOutbox,
		StreamCompute:      streamCompute,
		LogAggregator:      logAggregator,
		CascadeManager:     cascadeManager,
		ProtocolBridge:     protocolBridge,
		LinkageEvaluator:   linkageEvaluator,
		OTAEngine:          otaManager,
		MqttServer:         mqttServer,
		SerialBridge:       serialBridge,
		LifecycleMgr:       lifecycleMgr,

		ConfigVersionMgr: configVersionMgr,
		WSManager:        wsManager,

		Preprocessor:      preprocessor,
		ExpressionEngine:  preprocessorExpressions,
		PreprocessRules:   preprocessRuleStore,
		ExpressionConfigs: expressionConfigStore,
	}

	// The MCP tool registry was never populated outside the tests, which left
	// /mcp/tools, /mcp/call and the page built on them answering from an empty
	// map. The tools read through the container, so they must be registered after
	// it is assembled.
	api.RegisterMCPTools(container)

	// Load the saved preprocessing rules and derived-point expressions into the
	// engines, which happens before the scheduler starts collecting so the first
	// batch of a restarted gateway is already filtered the way the operator left
	// it.
	api.WirePreprocessing(container)

	// Restore user-toggled preset model states (default: all enabled).
	for _, pid := range aiInference.PresetModelIDs() {
		if v, err := db.GetSetting("ai_preset_enabled_" + pid); err == nil && v == "0" {
			aiInference.SetPresetEnabled(pid, false)
		}
	}

	logrus.Info("All services bootstrapped successfully")
	return container, nil
}

// startEngine starts all engine components in background goroutines.
func startEngine(ctx context.Context, c *api.ServiceContainer, cfg *config.AppConfig) error {
	// Start event bus
	if c.EventBus != nil {
		c.EventBus.Start(ctx)
		logrus.Info("EventBus started")
	}

	// Start northbound platform manager: register all platform handlers,
	// subscribe to collected-data events, and auto-connect platforms marked
	// enabled in the config (mirrors Python bootstrap.py platform connect).
	platformMgr := northbound.NewManager(c.EventBus)
	platformMgr.Start()
	platformMgr.AutoConnect(cfg.Platforms)
	c.PlatformMgr = platformMgr

	// Preload existing rules from database into RuleEvaluator
	if c.Evaluator != nil && c.RuleRepo != nil {
		rules, _, err := c.RuleRepo.List(1, 10000, "")
		if err != nil {
			logrus.WithError(err).Warn("Failed to load rules from database")
		} else {
			loaded := 0
			for i := range rules {
				if rules[i].Enabled {
					c.Evaluator.LoadRule(&rules[i])
					loaded++
				}
			}
			logrus.WithField("loaded_rules", loaded).Info("Rules preloaded into evaluator")
		}
	}

	// Start collection scheduler
	if c.Scheduler != nil {
		c.Scheduler.Start(ctx)
		logrus.Info("CollectScheduler started")
	}

	// Restore device collectors from database on startup.
	// This ensures that devices created in a previous session are still
	// collected after a backend restart.
	if c.DeviceService != nil && c.DeviceRepo != nil {
		go func() {
			time.Sleep(2 * time.Second) // Wait for scheduler to fully start
			devices, err := c.DeviceRepo.ListAll()
			if err != nil {
				logrus.WithError(err).Warn("Failed to load devices from database for collector restoration")
				return
			}
			restored := 0
			for _, dev := range devices {
				if err := c.DeviceService.SetupDriver(&dev); err != nil {
					logrus.WithField("device_id", dev.DeviceID).
						WithError(err).Warn("Failed to restore driver for device")
				} else {
					restored++
					logrus.WithField("device_id", dev.DeviceID).
						Info("Restored collector for device")
				}
			}
			if restored > 0 {
				logrus.WithField("count", restored).Info("Device collectors restored from database")
			}
		}()
	}

	// Start MQTT forwarder
	if c.MqttForward != nil {
		if err := c.MqttForward.Start(ctx); err != nil {
			logrus.WithError(err).Warn("MQTT forwarder start failed")
		} else {
			logrus.Info("MQTT forwarder started")
		}
	}

	// Start InferenceScheduler
	if c.InferenceScheduler != nil {
		if err := c.InferenceScheduler.Start(ctx); err != nil {
			logrus.WithError(err).Warn("InferenceScheduler start failed")
		} else {
			logrus.Info("InferenceScheduler started")
		}
	}

	// Start DriverWatchdog
	if c.DriverWatchdog != nil {
		c.DriverWatchdog.Start(ctx)
		logrus.Info("DriverWatchdog started")
	}

	// Start AlarmOutbox
	if c.AlarmOutbox != nil {
		c.AlarmOutbox.Start(ctx)
		logrus.Info("AlarmOutbox started")
	}

	// Start BackupScheduler. The loop itself honours backup.enabled, so starting
	// it while disabled is what lets PUT /system/backup/schedule turn automatic
	// backups on without a restart.
	if c.BackupScheduler != nil {
		c.BackupScheduler.Start(ctx)
	}

	// Start StreamComputeEngine
	if c.StreamCompute != nil {
		c.StreamCompute.Start(ctx)
		logrus.Info("StreamComputeEngine started")
	}

	// Start LogAggregator
	if c.LogAggregator != nil {
		if err := c.LogAggregator.Start(ctx); err != nil {
			logrus.WithError(err).Warn("LogAggregator start failed")
		} else {
			logrus.Info("LogAggregator started")
		}
	}

	// Start CascadeManager
	if c.CascadeManager != nil {
		if err := c.CascadeManager.Start(ctx); err != nil {
			logrus.WithError(err).Warn("CascadeManager start failed")
		} else {
			logrus.Info("CascadeManager started")
		}
	}

	// Start ProtocolBridgeManager
	if c.ProtocolBridge != nil {
		if err := c.ProtocolBridge.Start(ctx); err != nil {
			logrus.WithError(err).Warn("ProtocolBridgeManager start failed")
		} else {
			logrus.Info("ProtocolBridgeManager started")
		}
		// Loads persisted bridges, installs the device-write sink and subscribes to
		// collected data. Without this the manager was started, never fed and never
		// able to write, so /bridge CRUD answers 201 about a link that does nothing.
		api.WireProtocolBridge(c)
	}

	// Device linkage: rules live in the database, but only this call makes them
	// fire. Without it a rule could be created, listed and toggled forever while
	// nothing ever wrote to the target device.
	if c.LinkageEvaluator != nil {
		api.WireDeviceLinkage(c)
	}

	// Start OTAManager
	if c.OTAEngine != nil {
		if err := c.OTAEngine.Start(ctx); err != nil {
			logrus.WithError(err).Warn("OTAManager start failed")
		} else {
			logrus.Info("OTAManager started")
		}
	}

	// Start MqttServer (if enabled in config)
	if c.MqttServer != nil && cfg.MqttServer.Enabled {
		// The broker speaks MQTT over a raw TCP stream only; saying nothing here
		// let operators configure a WS port and believe browsers could connect.
		if cfg.MqttServer.WSPort != nil && *cfg.MqttServer.WSPort != 0 {
			logrus.WithField("ws_port", *cfg.MqttServer.WSPort).
				Warn("mqtt_server.ws_port is not supported by the embedded broker (MQTT over WebSocket is not implemented); the value is ignored")
		}
		mqttSrvCfg := engine.MqttServerConfig{
			Enabled:  cfg.MqttServer.Enabled,
			Host:     cfg.MqttServer.Host,
			Port:     cfg.MqttServer.Port,
			Username: cfg.MqttServer.Username,
			Password: cfg.MqttServer.Password,
			// Without this the operator's allow_no_auth: false was ignored and
			// the broker's auth mode was decided only by the empty/non-empty
			// shape of the credentials.
			AllowNoAuth:  cfg.MqttServer.AllowNoAuth,
			MaxClients:   100,
			MaxQueueSize: 1000,
		}
		if err := c.MqttServer.Start(ctx, mqttSrvCfg); err != nil {
			logrus.WithError(err).Warn("MQTT server start failed")
		} else {
			logrus.Info("MQTT server started")
		}
	}

	// Start SerialTCPBridge (if enabled in config)
	if c.SerialBridge != nil && cfg.SerialBridge.Enabled {
		serialCfg := engine.SerialBridgeConfig{
			SerialPort: cfg.SerialBridge.SerialPort,
			BaudRate:   cfg.SerialBridge.BaudRate,
			DataBits:   8,
			Parity:     "N",
			StopBits:   1,
			ListenAddr: fmt.Sprintf("0.0.0.0:%d", cfg.SerialBridge.TCPPort),
		}
		if err := c.SerialBridge.Start(ctx, serialCfg); err != nil {
			logrus.WithError(err).Warn("Serial bridge start failed")
		} else {
			logrus.Info("SerialTCPBridge started")
		}
	}

	// Start embedded Modbus Slave TCP server (if enabled in config)
	if c.ModbusSlaveServer == nil && cfg.ModbusSlave.Enabled {
		slave, err := drivers.NewModbusSlaveDriver("embedded-modbus-slave", map[string]interface{}{
			"host":          cfg.ModbusSlave.Host,
			"port":          cfg.ModbusSlave.Port,
			"holding_size":  cfg.ModbusSlave.HoldingSize,
			"input_size":    cfg.ModbusSlave.InputSize,
			"coil_size":     cfg.ModbusSlave.CoilSize,
			"discrete_size": cfg.ModbusSlave.DiscreteSize,
		})
		if err != nil {
			logrus.WithError(err).Warn("Modbus Slave init failed")
		} else if err := slave.Connect(ctx); err != nil {
			logrus.WithError(err).Warn("Modbus Slave start failed")
		} else {
			c.ModbusSlaveServer = slave
			logrus.Infof("Modbus Slave server started on %s:%d", cfg.ModbusSlave.Host, cfg.ModbusSlave.Port)
		}
	}

	// Start LifecycleManager
	if c.LifecycleMgr != nil {
		if err := c.LifecycleMgr.Start(ctx); err != nil {
			logrus.WithError(err).Warn("LifecycleManager start failed")
		} else {
			logrus.Info("LifecycleManager started")
		}
	}

	return nil
}

// stopEngine stops all engine components gracefully.
func stopEngine(ctx context.Context, c *api.ServiceContainer) {
	// Stop in reverse order of start

	if c.LifecycleMgr != nil {
		c.LifecycleMgr.Stop()
		logrus.Info("LifecycleManager stopped")
	}

	if c.SerialBridge != nil {
		if err := c.SerialBridge.Stop(); err != nil {
			logrus.WithError(err).Warn("Serial bridge stop failed")
		}
	}

	if c.MqttServer != nil {
		if err := c.MqttServer.Stop(); err != nil {
			logrus.WithError(err).Warn("MQTT server stop failed")
		}
	}

	if c.ModbusSlaveServer != nil {
		if err := c.ModbusSlaveServer.Disconnect(); err != nil {
			logrus.WithError(err).Warn("Modbus Slave stop failed")
		}
	}

	if c.OTAEngine != nil {
		c.OTAEngine.Stop()
		logrus.Info("OTAManager stopped")
	}

	if c.ProtocolBridge != nil {
		c.ProtocolBridge.Stop()
		logrus.Info("ProtocolBridgeManager stopped")
	}

	if c.CascadeManager != nil {
		c.CascadeManager.Stop()
		logrus.Info("CascadeManager stopped")
	}

	if c.LogAggregator != nil {
		c.LogAggregator.Stop()
		logrus.Info("LogAggregator stopped")
	}

	if c.StreamCompute != nil {
		c.StreamCompute.Stop()
		logrus.Info("StreamComputeEngine stopped")
	}

	if c.AlarmOutbox != nil {
		c.AlarmOutbox.Stop()
		logrus.Info("AlarmOutbox stopped")
	}

	if c.DriverWatchdog != nil {
		c.DriverWatchdog.Stop()
		logrus.Info("DriverWatchdog stopped")
	}

	if c.InferenceScheduler != nil {
		c.InferenceScheduler.Stop()
		logrus.Info("InferenceScheduler stopped")
	}

	if c.MqttForward != nil {
		if err := c.MqttForward.Stop(); err != nil {
			logrus.WithError(err).Warn("MQTT forwarder stop failed")
		}
	}

	if c.PlatformMgr != nil {
		c.PlatformMgr.Stop()
		logrus.Info("Northbound platform manager stopped")
	}

	if c.BackupScheduler != nil {
		c.BackupScheduler.Stop()
		logrus.Info("BackupScheduler stopped")
	}

	if c.Scheduler != nil {
		c.Scheduler.Stop()
		logrus.Info("CollectScheduler stopped")
	}

	// Stop WebSocket manager before EventBus to ensure
	// no events are broadcast after clients are disconnected.
	if c.WSManager != nil {
		c.WSManager.Stop()
		logrus.Info("WebSocket manager stopped")
	}

	if c.EventBus != nil {
		c.EventBus.Stop()
		logrus.Info("EventBus stopped")
	}
}

// configureMiddleware sets up the Echo middleware chain.
func configureMiddleware(e *echo.Echo, cfg *config.AppConfig) {
	// Request ID
	e.Use(elware.RequestIDMiddleware())

	// Recover from panics to prevent process crash
	e.Use(echomw.RecoverWithConfig(echomw.RecoverConfig{
		StackSize: 1 << 20, // 1MB
		Skipper:   nil,
		LogLevel:  0, // logrus handles level; use Error
	}))

	// Logger
	e.Use(elware.LoggerMiddleware())

	// Security headers
	e.Use(elware.SecurityHeadersMiddleware())

	// CORS
	corsOrigins := cfg.Server.CORSAllowedOrigins
	if len(corsOrigins) == 0 {
		corsOrigins = cfg.Server.CORSOrigins
	}
	if len(corsOrigins) == 0 {
		// Dev mode: allow localhost
		corsOrigins = []string{"http://localhost:3000", "http://127.0.0.1:3000",
			"http://localhost:5173", "http://127.0.0.1:5173",
			"http://localhost:8080", "http://127.0.0.1:8080"}
	}
	e.Use(elware.CORSMiddleware(corsOrigins))

	// CSRF protection: enforced only for cookie-authenticated state-changing
	// requests (Bearer-token and public requests are skipped inside the middleware)
	e.Use(elware.CSRFMiddleware())

	// Rate limiting
	rateLimit := cfg.Security.RateLimitRequestsPerMinute
	if rateLimit <= 0 {
		rateLimit = 600
	}
	limiter := elware.NewRateLimiter(rateLimit)
	e.Use(elware.RateLimitMiddleware(limiter))

	// Body limit (10MB)
	e.Use(echomw.BodyLimit("10M"))

	// Request timeout (30s for normal requests, skip WebSocket and SSE)
	e.Use(echomw.TimeoutWithConfig(echomw.TimeoutConfig{
		Timeout: 30 * time.Second,
		Skipper: func(c echo.Context) bool {
			path := c.Path()
			return strings.HasPrefix(path, "/ws/") || strings.HasPrefix(path, "/api/v1/system/stream")
		},
	}))

	// Gzip compression
	e.Use(echomw.GzipWithConfig(echomw.GzipConfig{
		Level: 5,
		Skipper: func(c echo.Context) bool {
			// Don't gzip WebSocket or SSE
			path := c.Path()
			return strings.HasPrefix(path, "/ws/")
		},
	}))
}

// registerRoutes registers all API route groups.
func registerRoutes(e *echo.Echo, cfg *config.AppConfig) {
	// API v1 group
	v1 := e.Group("/api/v1")

	// Auth routes (public — no auth middleware at group level)
	authGroup := v1.Group("/auth")
	api.RegisterAuthRoutes(authGroup)

	// Device routes (auth required — handled per-route by requirePermission)
	deviceGroup := v1.Group("/devices", elware.AuthMiddleware())
	api.RegisterDeviceRoutes(deviceGroup)

	// Rule routes
	ruleGroup := v1.Group("/rules", elware.AuthMiddleware())
	api.RegisterRuleRoutes(ruleGroup)

	// Alarm routes
	alarmGroup := v1.Group("/alarms", elware.AuthMiddleware())
	api.RegisterAlarmRoutes(alarmGroup)

	// Data routes
	dataGroup := v1.Group("/data", elware.AuthMiddleware())
	api.RegisterDataRoutes(dataGroup)

	// System routes (some endpoints are public)
	systemGroup := v1.Group("/system")
	api.RegisterSystemRoutes(systemGroup)
	api.RegisterSystemExtendedRoutes(systemGroup)

	// User routes
	userGroup := v1.Group("/users", elware.AuthMiddleware())
	api.RegisterUserRoutes(userGroup)

	// Notify routes
	notifyGroup := v1.Group("/notify", elware.AuthMiddleware())
	api.RegisterNotifyRoutes(notifyGroup)
	api.RegisterNotifyExtendedRoutes(notifyGroup)

	// Video routes
	videoGroup := v1.Group("/video", elware.AuthMiddleware())
	api.RegisterVideoRoutes(videoGroup)
	api.RegisterVideoExtendedRoutes(videoGroup)

	// Driver routes
	driverGroup := v1.Group("/drivers", elware.AuthMiddleware())
	api.RegisterDriverRoutes(driverGroup)
	api.RegisterDriverExtendedRoutes(driverGroup)

	// Platform routes
	platformGroup := v1.Group("/platforms", elware.AuthMiddleware())
	api.RegisterPlatformRoutes(platformGroup)
	api.RegisterPlatformExtendedRoutes(platformGroup)

	// Audit routes
	auditGroup := v1.Group("/audit", elware.AuthMiddleware())
	api.RegisterAuditRoutes(auditGroup)
	api.RegisterAuditExtendedRoutes(auditGroup)

	// SCADA routes (prefix=/api/v1/scada, aligned with Python)
	scadaGroup := v1.Group("/scada", elware.AuthMiddleware())
	api.RegisterSCADARoutes(scadaGroup)
	api.RegisterSCADAExtendedRoutes(scadaGroup)

	// Shadow routes (prefix=/api/v1/shadows, aligned with Python and frontend)
	shadowGroup := v1.Group("/shadows", elware.AuthMiddleware())
	api.RegisterShadowRoutes(shadowGroup)
	api.RegisterShadowExtendedRoutes(shadowGroup)

	// OTA routes
	otaGroup := v1.Group("/ota", elware.AuthMiddleware())
	// RegisterOTARoutes and RegisterOTAExtendedRoutes are replaced, not mounted: the
	// first echoed POST /ota/tasks back and listed nothing it had stored, the second
	// printed two hardcoded versions for /check and answered "applying" and
	// "rolled_back" without touching anything, which made OtaUpdate.vue tell the
	// operator the gateway had upgraded. /ota/* now reads the live OTA manager, the
	// real backup directory and the configured release feed, and refuses what this
	// build cannot do.
	api.RegisterOTARoutesReal(otaGroup)

	// Preprocess routes
	preprocessGroup := v1.Group("/preprocess", elware.AuthMiddleware())
	api.RegisterPreprocessRoutes(preprocessGroup)
	// RegisterPreprocessExtendedRoutes is replaced, not mounted: its GET answered
	// {rules: [], global: {}} and its PUT echoed the body with updated:true, so the
	// preprocessing page could add a deadband, report success and collect raw
	// values. The real routes read the config section and the rule store, and push
	// both into the running Preprocessor.
	api.RegisterPreprocessConfigRoutes(preprocessGroup)

	// Expression routes
	expressionGroup := v1.Group("/expressions", elware.AuthMiddleware())
	api.RegisterExpressionRoutes(expressionGroup)
	// Same for the expression workbench: evaluate answered result null for every
	// input and validate answered valid true. The real routes run the same parser
	// the collect path uses for derived points.
	api.RegisterExpressionTestRoutes(expressionGroup)

	// AI Model routes (prefix=/api/v1/ai, aligned with Python and frontend)
	aiGroup := v1.Group("/ai", elware.AuthMiddleware())
	api.RegisterAIModelRoutes(aiGroup)

	// MQTT Server routes
	mqttServerGroup := v1.Group("/mqtt-server", elware.AuthMiddleware())
	api.RegisterMQTTServerRoutes(mqttServerGroup)

	// Modbus Slave routes
	modbusSlaveGroup := v1.Group("/modbus-slave", elware.AuthMiddleware())
	api.RegisterModbusSlaveRoutes(modbusSlaveGroup)
	api.RegisterModbusSlaveExtendedRoutes(modbusSlaveGroup)

	// Serial Bridge routes
	serialBridgeGroup := v1.Group("/serial-bridge", elware.AuthMiddleware())
	api.RegisterSerialBridgeRoutes(serialBridgeGroup)

	// Integration routes
	integrationGroup := v1.Group("/integration", elware.AuthMiddleware())
	api.RegisterIntegrationRoutes(integrationGroup)
	api.RegisterIntegrationExtendedRoutes(integrationGroup)

	// MCP routes
	mcpGroup := v1.Group("/mcp", elware.AuthMiddleware())
	api.RegisterMCPRoutes(mcpGroup)
	// RegisterMCPExtendedRoutes is replaced rather than mounted: its /call echoed
	// an empty result for any tool name, /auth-keys answered 201 for a credential
	// nothing stores or checks, and /sse wrote one line and closed.
	api.RegisterMCPRealRoutes(mcpGroup)

	// Service routes
	serviceGroup := v1.Group("/services", elware.AuthMiddleware())
	api.RegisterServiceRoutes(serviceGroup)
	api.RegisterServiceExtendedRoutes(serviceGroup)

	// Grafana routes
	grafanaGroup := v1.Group("/grafana", elware.AuthMiddleware())
	api.RegisterGrafanaRoutes(grafanaGroup)
	api.RegisterGrafanaExtendedRoutes(grafanaGroup)

	// Simulation routes
	simulationGroup := v1.Group("/simulation", elware.AuthMiddleware())
	api.RegisterSimulationRoutes(simulationGroup)
	api.RegisterSimulationExtendedRoutes(simulationGroup)

	// MQTT Forwarder routes
	mqttForwarderGroup := v1.Group("/mqtt-forwarder", elware.AuthMiddleware())
	api.RegisterMQTTForwarderRoutes(mqttForwarderGroup)
	api.RegisterMQTTForwarderExtendedRoutes(mqttForwarderGroup)

	// Metrics route (public, no auth)
	e.GET("/metrics", func(c echo.Context) error {
		cont := api.GetContainer()
		metrics := map[string]interface{}{
			"status":  "ok",
			"version": Version,
		}
		if cont.Scheduler != nil {
			metrics["scheduler"] = cont.Scheduler.Stats()
		}
		if cont.EventBus != nil {
			metrics["event_bus"] = cont.EventBus.Metrics()
		}
		return c.JSON(http.StatusOK, metrics)
	})
	metricsGroup := v1.Group("/metrics")
	api.RegisterMetricsRoutes(metricsGroup)

	// Data Quality routes
	dataQualityGroup := v1.Group("/data-quality", elware.AuthMiddleware())
	api.RegisterDataQualityRoutes(dataQualityGroup)
	api.RegisterDataQualityExtendedRoutes(dataQualityGroup)

	// DB Monitor routes
	dbMonitorGroup := v1.Group("/db-monitor", elware.AuthMiddleware())
	api.RegisterDBMonitorRoutes(dbMonitorGroup)
	api.RegisterDBMonitorExtendedRoutes(dbMonitorGroup)

	// Log Aggregation routes (prefix=/api/v1/logs, aligned with Python and frontend)
	logAggGroup := v1.Group("/logs", elware.AuthMiddleware())
	api.RegisterLogAggregationRoutes(logAggGroup)
	api.RegisterLogAggregationExtendedRoutes(logAggGroup)

	// Resource Share routes
	resourceShareGroup := v1.Group("/resource-shares", elware.AuthMiddleware())
	api.RegisterResourceShareRoutes(resourceShareGroup)
	api.RegisterResourceShareExtendedRoutes(resourceShareGroup)

	// Config Version routes (prefix=/api/v1/config, aligned with Python)
	configVersionGroup := v1.Group("/config", elware.AuthMiddleware())
	api.RegisterConfigVersionRoutes(configVersionGroup)

	// Protocol Bridge routes (prefix=/api/v1/bridge, aligned with Python and frontend)
	protocolBridgeGroup := v1.Group("/bridge", elware.AuthMiddleware())
	api.RegisterProtocolBridgeRoutes(protocolBridgeGroup)
	// RegisterProtocolBridgeExtendedRoutes is not mounted: /bridge/list answered an
	// empty page and /bridge/create a 201 echo while the persisted, manager-backed
	// API is /bridge/bridges (above). Keeping both meant a client could create a
	// bridge, get an id back, and never see it in the list again.
	// The /bridge/list|create|/:name aliases are not mounted: they answered with an
	// echo of the request while /bridge/bridges is the persisted, manager-backed API,
	// so a client could POST a bridge, get 201 and never see it again.

	// Observability routes
	observabilityGroup := v1.Group("/observability", elware.AuthMiddleware())
	api.RegisterObservabilityRoutes(observabilityGroup)
	api.RegisterObservabilityExtendedRoutes(observabilityGroup)

	// Scripts routes
	scriptsGroup := v1.Group("/scripts", elware.AuthMiddleware())
	api.RegisterScriptsRoutes(scriptsGroup)
	api.RegisterScriptsExtendedRoutes(scriptsGroup)

	// Firmware Signature routes (prefix=/api/v1/firmware, aligned with Python and frontend)
	firmwareGroup := v1.Group("/firmware", elware.AuthMiddleware())
	api.RegisterFirmwareSignatureRoutes(firmwareGroup)
	// RegisterFirmwareSignatureExtendedRoutes is replaced rather than mounted: its
	// /verify/signature and /verify/hash returned {"valid":true} without reading a
	// body, a key or a file. RegisterFirmwareIntegrityRoutes serves the same paths
	// from the real signature store.
	api.RegisterFirmwareIntegrityRoutes(firmwareGroup)

	// Device Linkage routes (prefix=/api/v1/linkage, aligned with Python and frontend)
	deviceLinkageGroup := v1.Group("/linkage", elware.AuthMiddleware())
	api.RegisterDeviceLinkageRoutes(deviceLinkageGroup)
	// RegisterDeviceLinkageExtendedRoutes is deliberately not mounted: /linkage/rules
	// and /linkage/executions answered with an echo of the request body and a hard
	// coded empty list, so a client that POSTed a rule got 201 for a rule that was
	// never stored. /linkage (above) is the persisted, evaluator-backed API.

	// Anomaly Learner routes
	anomalyLearnerGroup := v1.Group("/anomaly-learner", elware.AuthMiddleware())
	api.RegisterAnomalyLearnerRoutes(anomalyLearnerGroup)
	// The extended learner aliases are not mounted for all three learners: there is
	// no model store behind them, yet /initialize answered initialized:true, /infer
	// answered is_anomaly:false score:0 and /dashboard answered zeros. A caller
	// could not tell "trained, nothing anomalous" from "nothing was ever trained".
	// /stats (above) is the honest endpoint: it reports implemented:false.

	// Trend Learner routes
	trendLearnerGroup := v1.Group("/trend-learner", elware.AuthMiddleware())
	api.RegisterTrendLearnerRoutes(trendLearnerGroup)

	// Threshold Learner routes
	thresholdLearnerGroup := v1.Group("/threshold-learner", elware.AuthMiddleware())
	api.RegisterThresholdLearnerRoutes(thresholdLearnerGroup)

	// Profiler routes (prefix=/api/v1/profiler, aligned with Python)
	profilerGroup := v1.Group("/profiler", elware.AuthMiddleware())
	api.RegisterProfilerRoutes(profilerGroup)

	// Debug routes (prefix=/api/v1/debug, aligned with Python)
	debugGroup := v1.Group("/debug", elware.AuthMiddleware())
	api.RegisterDebugRoutes(debugGroup)

	// WebSocket routes
	wsGroup := e.Group("/ws/v1")
	registerWSRoutes(wsGroup, cfg)

	// Health check endpoints (public)
	e.GET("/health/live", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "alive"})
	})
	e.GET("/health/ready", func(c echo.Context) error {
		cont := api.GetContainer()
		if cont.Database != nil && cont.Database.IsHealthy() {
			return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
		}
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
	})

	logrus.Info("API routes registered")
}

// registerWSRoutes registers WebSocket endpoints with token-based authentication.
// WebSocket clients authenticate via the "token" query parameter or the
// "Authorization" header (Bearer token). This is necessary because browsers
// cannot set custom headers on WebSocket upgrade requests.
func registerWSRoutes(g *echo.Group, cfg *config.AppConfig) {
	wsManager := api.GetContainer().WSManager
	if wsManager == nil {
		logrus.Warn("WSManager not initialized, skipping WebSocket routes")
		return
	}

	// wsAuthMiddleware authenticates WebSocket connections via query parameter,
	// Authorization header, or HttpOnly Cookie. This prevents unauthenticated access to
	// real-time data streams. Cookie support is essential for browser clients that
	// rely on HttpOnly Cookie auth after page refresh (token not in JS memory).
	wsAuthMiddleware := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Try Authorization header first
			token := ""
			authHeader := c.Request().Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}
			// Fall back to query parameter (for browser WebSocket clients with in-memory token)
			if token == "" {
				token = c.QueryParam("token")
			}
			// Fall back to HttpOnly Cookie (for browser clients after page refresh).
			// Cookie name must match the one set at login ("edgelite_access").
			if token == "" {
				if cookie, err := c.Cookie("edgelite_access"); err == nil && cookie.Value != "" {
					token = cookie.Value
				}
			}
			if token == "" {
				return c.JSON(http.StatusUnauthorized, map[string]string{"detail": "Authentication required"})
			}
			jwtMgr := security.GetJWTManager()
			claims, err := jwtMgr.VerifyToken(token)
			if err != nil || (claims.ID != "" && security.IsRevoked(claims.ID)) {
				return c.JSON(http.StatusUnauthorized, map[string]string{"detail": "Invalid or expired token"})
			}
			c.Set("user_id", claims.UserID)
			c.Set("username", claims.Username)
			c.Set("role", claims.Role)
			return next(c)
		}
	}

	wsHandler := func(c echo.Context) error {
		wsManager.HandleWS(c.Response().Writer, c.Request())
		return nil
	}

	g.GET("/realtime", wsHandler, wsAuthMiddleware)
	g.GET("/alarm", wsHandler, wsAuthMiddleware)
	g.GET("/device", wsHandler, wsAuthMiddleware)
	g.GET("/ai", wsHandler, wsAuthMiddleware)
	g.GET("/integration", wsHandler, wsAuthMiddleware)

	logrus.Info("WebSocket routes registered with authentication")
}

// mountFrontend mounts the frontend SPA static files.
func mountFrontend(e *echo.Echo, cfg *config.AppConfig) {
	frontendDist := os.Getenv("EDGELITE_FRONTEND_DIST")
	if frontendDist == "" {
		// Try local frontend/dist (built by `npm run build`)
		frontendDist = "frontend/dist"
	}

	// Check if the frontend directory exists
	if _, err := os.Stat(frontendDist); os.IsNotExist(err) {
		logrus.WithField("path", frontendDist).Warn("Frontend dist not found, serving fallback page")

		// Serve a fallback guide page
		e.GET("/", func(c echo.Context) error {
			html := `<!DOCTYPE html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>EdgeLite Gateway - 前端未构建</title>
<style>
body{font-family:system-ui,-apple-system,sans-serif;max-width:680px;margin:60px auto;padding:0 20px;color:#333}
h1{color:#009688} code{background:#f5f5f5;padding:2px 6px;border-radius:3px;font-size:14px}
pre{background:#f5f5f5;padding:16px;border-radius:8px;overflow-x:auto;font-size:13px;line-height:1.6}
.tip{background:#e8f5e9;border-left:4px solid #009688;padding:12px 16px;border-radius:4px;margin:16px 0}
</style></head>
<body>
<h1>🔧 EdgeLite Gateway 后端已启动</h1>
<p>当前访问的是后端 API 服务，前端界面尚未构建。</p>
<div class="tip">
<b>选择以下任一方式查看完整界面：</b>
</div>
<h3>方式一：Docker 部署（推荐）</h3>
<pre>cd docker
cp .env.example .env
docker compose build edgelite
docker compose up -d</pre>
<h3>方式二：本地开发模式</h3>
<pre># 终端1：后端已运行（当前窗口）
# 终端2：启动前端开发服务器
cd frontend
npm install
npm run dev</pre>
<p>然后浏览器打开 <code>http://localhost:5173</code></p>
<h3>方式三：构建前端后由后端提供服务</h3>
<pre>cd frontend
npm install
npm run build</pre>
<hr>
<p style="color:#999;font-size:13px">
健康检查：<a href="/health/live">/health/live</a>
</p>
</body></html>`
			return c.HTML(http.StatusOK, html)
		})
		return
	}

	// Serve static assets
	assetsDir := filepath.Join(frontendDist, "assets")
	if _, err := os.Stat(assetsDir); err == nil {
		e.Static("/assets", assetsDir)
	}

	// Serve index.html for all non-API routes (SPA fallback)
	indexFile := filepath.Join(frontendDist, "index.html")
	e.GET("/*", func(c echo.Context) error {
		path := c.Param("*")
		// Don't intercept API, WS, or health routes
		if strings.HasPrefix(path, "api/") || strings.HasPrefix(path, "ws/") || strings.HasPrefix(path, "health") {
			return c.JSON(http.StatusNotFound, map[string]string{"detail": "Not Found"})
		}

		// Try to serve a static file
		filePath := filepath.Join(frontendDist, path)
		if path != "" {
			if _, err := os.Stat(filePath); err == nil {
				// Security: prevent path traversal — use filepath.Rel for robust
				// containment check instead of string prefix, which can be bypassed
				// when the prefix is a substring of another directory name.
				absPath, err := filepath.Abs(filePath)
				if err != nil {
					return c.JSON(http.StatusNotFound, map[string]string{"detail": "Not Found"})
				}
				absDist, err := filepath.Abs(frontendDist)
				if err != nil {
					return c.JSON(http.StatusNotFound, map[string]string{"detail": "Not Found"})
				}
				relPath, err := filepath.Rel(absDist, absPath)
				if err != nil || strings.HasPrefix(relPath, "..") || relPath == ".." {
					logrus.WithField("path", path).Warn("Frontend path traversal attempt blocked")
					return c.JSON(http.StatusNotFound, map[string]string{"detail": "Not Found"})
				}
				return c.File(filePath)
			}
		}

		// Fallback to index.html (SPA routing)
		return c.File(indexFile)
	})

	logrus.WithField("path", frontendDist).Info("Frontend static files mounted")
}
