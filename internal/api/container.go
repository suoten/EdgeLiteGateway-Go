package api

import (
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	"edgelite/internal/drivers"
	"edgelite/internal/engine"
	"edgelite/internal/northbound"
	"edgelite/internal/services"
	"edgelite/internal/storage"
	"edgelite/internal/ws"
)

// ServiceContainer holds references to all services for dependency injection.
// This mirrors the Python ServiceContainer / _app_state pattern.
type ServiceContainer struct {
	Database     *storage.Database
	DeviceRepo   *storage.DeviceRepo
	RuleRepo     *storage.RuleRepo
	AlarmRepo    *storage.AlarmRepo
	TemplateRepo *storage.TemplateRepo
	UserRepo     *storage.UserRepo
	TsStorage    *storage.TimeSeriesStorage
	Cache        *storage.CacheManager

	DeviceService *services.DeviceService
	RuleService   *services.RuleService
	AlarmService  *services.AlarmService
	DataService   *services.DataService
	SystemService *services.SystemService
	NotifyService *services.NotifyService

	// Extended services (1:1 port of Python services/)
	AlarmCorrelation *services.AlarmCorrelationService
	AlarmSilence     *services.AlarmSilenceService
	BackupScheduler  *services.BackupScheduler
	DataImportExport *services.DataImportExportService
	CmdApproval      *services.CommandApprovalService
	ShadowService    *services.ShadowService
	DBMonitor        *services.DBMonitorService
	MCPService       *services.MCPService
	VideoService     *services.VideoService
	I18nService      *services.I18nService
	ServiceManager   *services.ServiceManager
	AuditService     *services.AuditService
	HistDataService  *services.HistoricalDataService

	Scheduler          *engine.CollectScheduler
	EventBus           *engine.EventBus
	CBRegistry         *engine.CircuitBreakerRegistry
	Evaluator          *engine.RuleEvaluator
	AIInference        *engine.AIInferenceEngine
	MqttForward        *engine.MQTTForwarder
	InferenceScheduler *engine.InferenceScheduler
	DriverWatchdog     *engine.DriverWatchdog
	AlarmOutbox        *engine.AlarmOutbox
	StreamCompute      *engine.StreamComputeEngine
	LogAggregator      *engine.LogAggregator
	CascadeManager     *engine.CascadeManager
	ProtocolBridge     *engine.ProtocolBridgeManager
	// LinkageEvaluator actuates target devices when a source point crosses a
	// configured threshold. Distinct from Evaluator, which only raises alarms.
	LinkageEvaluator  *engine.LinkageEvaluator
	OTAEngine         *engine.OTAManager
	MqttServer        *engine.MqttServer
	SerialBridge      *engine.SerialTCPBridge
	ModbusSlaveServer drivers.Driver
	LifecycleMgr      *engine.LifecycleManager

	// Preprocessor filters every batch the collect scheduler gathers, and
	// ExpressionEngine derives additional points from them. Their rule stores are
	// listed with them because a handler needs both halves to keep what it writes
	// and what runs from drifting apart.
	Preprocessor      *engine.Preprocessor
	ExpressionEngine  *engine.PreprocessorExpressionEngine
	PreprocessRules   *storage.PreprocessRuleStore
	ExpressionConfigs *storage.ExpressionConfigStore

	// Northbound platform integration (ThingsBoard / IoTDA / etc.)
	PlatformMgr *northbound.Manager

	ConfigVersionMgr *drivers.ConfigVersionManager
	WSManager        *ws.Manager

	// ServiceEnabledMap tracks user-enabled state for services that are not
	// always-on (e.g. grafana, mqtt_forwarder).  key=service name, val=true|false.
	ServiceEnabledMap map[string]bool
	ServiceEnabledMu  sync.RWMutex
}

// NewServiceContainer creates a new ServiceContainer.
func NewServiceContainer() *ServiceContainer {
	return &ServiceContainer{
		ServiceEnabledMap: make(map[string]bool),
	}
}

// container is the package-level singleton.
var (
	container   *ServiceContainer
	containerMu sync.Mutex
)

// GetContainer returns the singleton ServiceContainer.
func GetContainer() *ServiceContainer {
	containerMu.Lock()
	defer containerMu.Unlock()
	if container == nil {
		logrus.Warn("ServiceContainer not initialized, creating empty container")
		container = NewServiceContainer()
	}
	return container
}

// SetContainer sets the package-level singleton.
func SetContainer(c *ServiceContainer) {
	containerMu.Lock()
	defer containerMu.Unlock()
	container = c
	// The old line claimed "all services" for any container, including one
	// where half the managers were nil -- handlers then answered 503 while the
	// startup log insisted everything was wired up.
	missing := missingServices(c)
	if len(missing) == 0 {
		logrus.Info("ServiceContainer initialized with all services")
		return
	}
	logrus.WithField("missing_services", strings.Join(missing, ",")).
		WithField("count", len(missing)).
		Warn("ServiceContainer initialized with unavailable services")
}

// missingServices lists the names of nil service fields on the container.
func missingServices(c *ServiceContainer) []string {
	if c == nil {
		return []string{"container"}
	}
	var missing []string
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
			if fv.IsNil() {
				missing = append(missing, t.Field(i).Name)
			}
		}
	}
	sort.Strings(missing)
	return missing
}
