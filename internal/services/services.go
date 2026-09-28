// Package services provides business logic services for EdgeLite Gateway.
//
// This package is a 1:1 port of the Python edgelite/services/ package.
// It includes services for devices, rules, alarms, data, notifications,
// system, video, audit, platforms, shadow, and AI.
package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"edgelite/internal/config"
	"edgelite/internal/constants"
	"edgelite/internal/drivers"
	"edgelite/internal/engine"
	"edgelite/internal/models"
	"edgelite/internal/security"
	"edgelite/internal/storage"
)

// DeviceService handles device management operations.
type DeviceService struct {
	deviceRepo   *storage.DeviceRepo
	templateRepo *storage.TemplateRepo
	scheduler   *engine.CollectScheduler
	cbRegistry   *engine.CircuitBreakerRegistry
	driverRegistry *drivers.Registry
	mu           sync.RWMutex
	drivers      map[string]drivers.Driver // deviceID -> driver instance

	// writeMu guards lastWrite, the per-point timestamps that make
	// config.write_rate_limit real. It is separate from mu so a throttled write
	// never blocks the driver map.
	writeMu   sync.Mutex
	lastWrite map[string]time.Time
}

// NewDeviceService creates a new DeviceService.
func NewDeviceService(
	deviceRepo *storage.DeviceRepo,
	templateRepo *storage.TemplateRepo,
	scheduler *engine.CollectScheduler,
	cbRegistry *engine.CircuitBreakerRegistry,
) *DeviceService {
	return &DeviceService{
		deviceRepo:     deviceRepo,
		templateRepo:   templateRepo,
		scheduler:      scheduler,
		cbRegistry:     cbRegistry,
		driverRegistry: drivers.GetRegistry(),
		drivers:        make(map[string]drivers.Driver),
		lastWrite:      make(map[string]time.Time),
	}
}

// Create creates a new device.
func (s *DeviceService) Create(req *models.DeviceCreate, createdBy string) (*models.DeviceResponse, error) {
	deviceID := req.DeviceID
	if deviceID == "" {
		deviceID = fmt.Sprintf("dev-%s", uuid.New().String()[:8])
	}

	// Normalize protocol
	protocol := constants.NormalizeProtocol(req.Protocol)
	if protocol == "" {
		return nil, fmt.Errorf("unsupported protocol: %s", req.Protocol)
	}

	// Report a taken id as "already exists": callers translate that wording into
	// 409, while the UNIQUE constraint error from SQLite translated to 400, so
	// creating a device that exists read as a malformed request.
	if existing, err := s.deviceRepo.Get(deviceID); err == nil && existing != nil {
		return nil, fmt.Errorf("device already exists: %s", deviceID)
	}

	// Create device record
	device := &models.DeviceResponse{
		DeviceID:       deviceID,
		Name:           req.Name,
		Protocol:       protocol,
		Status:         "offline",
		Config:         req.Config,
		Points:         req.Points,
		CollectInterval: req.CollectInterval,
		CreatedBy:      createdBy,
		CreatedAt:      time.Now().Format(time.RFC3339),
		UpdatedAt:      time.Now().Format(time.RFC3339),
		Version:        1,
	}

	if err := s.deviceRepo.Create(device, createdBy); err != nil {
		return nil, fmt.Errorf("failed to create device: %w", err)
	}

	// Create and register driver
	if err := s.setupDriver(device); err != nil {
		// A device whose protocol driver refuses its config can never collect or
		// be written to. Saving it anyway left a permanently offline row whose
		// only explanation was a server-side log line.
		_ = s.deviceRepo.Delete(deviceID)
		return nil, fmt.Errorf("%w: %s", ErrDriverInit, err)
	}

	logrus.WithField("device_id", deviceID).Info("Device created")
	return device, nil
}

// SetupDriver creates a driver instance and registers it with the scheduler.
// This is called on device creation and on backend restart to restore collectors.
func (s *DeviceService) SetupDriver(device *models.DeviceResponse) error {
	return s.setupDriver(device)
}

// setupDriver creates a driver instance and registers it with the scheduler.
func (s *DeviceService) setupDriver(device *models.DeviceResponse) error {
	driver, err := s.driverRegistry.CreateDriver(device.Protocol, device.DeviceID, device.Config)
	if err != nil {
		return fmt.Errorf("failed to create driver: %w", err)
	}

	s.mu.Lock()
	s.drivers[device.DeviceID] = driver
	s.mu.Unlock()

	// Register collector with scheduler
	interval := device.CollectInterval
	if interval <= 0 {
		interval = 5
	}

	s.scheduler.RegisterCollector(device.DeviceID, interval, func(ctx context.Context, devID string) ([]storage.PointData, error) {
		// Check circuit breaker
		cb := s.cbRegistry.Get(devID)
		if !cb.AllowRequest() {
			return nil, fmt.Errorf("circuit breaker open for device %s", devID)
		}

		s.mu.RLock()
		drv, ok := s.drivers[devID]
		s.mu.RUnlock()
		if !ok {
			cb.RecordFailure()
			return nil, fmt.Errorf("driver not found for device %s", devID)
		}

		if !drv.IsConnected() {
			if err := drv.Connect(ctx); err != nil {
				cb.RecordFailure()
				return nil, fmt.Errorf("connect failed: %w", err)
			}
		}

		points, err := drv.ReadPoints(ctx, device.Points)
		if err != nil {
			cb.RecordFailure()
			s.deviceRepo.UpdateStatus(device.DeviceID, "error")
			return nil, err
		}

		cb.RecordSuccess()
		// Driver values are raw protocol counts; the value that gets stored, alerted
		// on and shown is the engineering one the point declares (raw*scale+offset).
		// Applying it where every driver's result converges is what makes scale and
		// offset mean the same thing for Modbus, S7 and MQTT as they do for the
		// simulator, instead of being silently dropped by every real driver.
		applyPointScaling(device.Points, points)
		// Update device status to online when collection succeeds
		s.deviceRepo.UpdateStatus(device.DeviceID, "online")
		return points, nil
	})

	return nil
}

// applyPointScaling rewrites the collected values in place using each point's
// declared scale and offset. Non-numeric values (strings, booleans, binaries) are
// left alone: there is no meaningful unit transform for them.
func applyPointScaling(defs []models.PointDef, points []storage.PointData) {
	if len(defs) == 0 || len(points) == 0 {
		return
	}
	var scaled map[string]*models.PointDef
	for i := range defs {
		p := &defs[i]
		if p.Scale == nil && p.Offset == nil {
			continue
		}
		if scaled == nil {
			scaled = make(map[string]*models.PointDef, 4)
		}
		scaled[p.Name] = p
	}
	if scaled == nil {
		return
	}
	for i := range points {
		p, ok := scaled[points[i].PointName]
		if !ok || points[i].Value == nil {
			continue
		}
		f, ok := pointValueAsFloat(points[i].Value)
		if !ok {
			continue
		}
		v := f
		if p.Scale != nil {
			v *= *p.Scale
		}
		if p.Offset != nil {
			v += *p.Offset
		}
		if isIntegralDataType(p.DataType) {
			v = math.Round(v)
		}
		points[i].Value = v
	}
}

// inversePointScaling undoes applyPointScaling for a value the operator wrote.
// Writes arrive in engineering units and the device register holds the raw count,
// so without this a written 21.5 would land as 21.5 counts and read back as a
// different number than the one that was sent.
func inversePointScaling(p *models.PointDef, value interface{}) (interface{}, error) {
	if p == nil || (p.Scale == nil && p.Offset == nil) {
		return value, nil
	}
	f, ok := pointValueAsFloat(value)
	if !ok {
		// A bool/string setpoint has no unit transform; pass it through untouched.
		return value, nil
	}
	v := f
	if p.Offset != nil {
		v -= *p.Offset
	}
	if p.Scale != nil {
		if *p.Scale == 0 {
			return nil, fmt.Errorf("point %s declares scale 0, so its value cannot be converted back to register units", p.Name)
		}
		v /= *p.Scale
	}
	if isIntegralDataType(p.DataType) {
		v = math.Round(v)
	}
	return v, nil
}

func isIntegralDataType(dataType string) bool {
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "int", "int8", "int16", "int32", "int64", "long",
		"uint", "uint8", "uint16", "uint32", "uint64", "word", "dword", "short", "byte":
		return true
	}
	return false
}

// pointValueAsFloat reports the numeric value of a collected sample.
//
// bool and text are deliberately NOT converted here, unlike
// constants.NumericAsFloat: scaling a boolean or a text setpoint has no
// meaning, and callers such as applyPointScaling/inversePointScaling rely on
// the false answer to pass those values through untouched.
func pointValueAsFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// Get retrieves a device by ID.
func (s *DeviceService) Get(deviceID string) (*models.DeviceResponse, error) {
	return s.deviceRepo.Get(deviceID)
}

// List returns all devices with pagination.
func (s *DeviceService) List(page, size int) ([]models.DeviceResponse, int, error) {
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > constants.MaxQuerySize {
		size = constants.DefaultPageSize
	}
	return s.deviceRepo.List(page, size)
}

// Update updates a device.
func (s *DeviceService) Update(deviceID string, req *models.DeviceUpdate) (*models.DeviceResponse, error) {
	// Validate the protocol config before writing it: driver constructors only
	// parse, so this is cheap, and it is what keeps a rejected config from being
	// saved as a working-looking device.
	if req.Config != nil || req.Points != nil {
		if err := s.checkDriverConfig(deviceID, req); err != nil {
			return nil, err
		}
	}
	if err := s.deviceRepo.Update(deviceID, req); err != nil {
		return nil, fmt.Errorf("failed to update device: %w", err)
	}

	// Re-setup driver if config changed
	if req.Config != nil || req.Points != nil {
		device, _ := s.deviceRepo.Get(deviceID)
		if device != nil {
			s.scheduler.UnregisterCollector(deviceID)
			s.mu.Lock()
			delete(s.drivers, deviceID)
			s.mu.Unlock()
			if err := s.setupDriver(device); err != nil {
				// The old collector is gone at this point, so the device has to be
				// shown as broken even though the row itself saved correctly.
				logrus.WithField("device_id", deviceID).
					WithError(err).
					Error("Failed to re-create driver after device update; device has no collector")
				s.deviceRepo.UpdateStatus(deviceID, "error")
			}
		}
	}

	device, err := s.deviceRepo.Get(deviceID)
	if err != nil {
		return nil, err
	}
	return device, nil
}

// ErrDriverInit means the protocol driver rejected a device's configuration.
var ErrDriverInit = errors.New("invalid device config for protocol driver")

// checkDriverConfig builds a throwaway driver from the config the update is
// about to store. Driver constructors only parse, so nothing is opened here; the
// point is to refuse an unusable config before it replaces a working one.
func (s *DeviceService) checkDriverConfig(deviceID string, req *models.DeviceUpdate) error {
	device, err := s.deviceRepo.Get(deviceID)
	if err != nil || device == nil {
		// Let the repo report an unknown device; nothing to validate against here.
		return nil
	}
	config := device.Config
	if req.Config != nil {
		config = req.Config
	}
	if _, err := s.driverRegistry.CreateDriver(device.Protocol, deviceID, config); err != nil {
		return fmt.Errorf("%w: %s", ErrDriverInit, err)
	}
	return nil
}

// Delete deletes a device.
func (s *DeviceService) Delete(deviceID string) error {
	s.scheduler.UnregisterCollector(deviceID)
	s.cbRegistry.Remove(deviceID)

	s.mu.Lock()
	if drv, ok := s.drivers[deviceID]; ok {
		drv.Disconnect()
		delete(s.drivers, deviceID)
	}
	s.mu.Unlock()

	// The three infrastructure managers are keyed by device ID and outlive the
	// service, so deleting a device used to leave its health counters, circuit
	// state and reconnect attempt count behind forever: the maps grew on every
	// delete, and re-creating a device under the same ID (an import, or a
	// delete-then-retry) started out inheriting the old device's failures --
	// a device that had never collected could look degraded, or have its breaker
	// already open.
	drivers.GetHealthStatsManager().ResetHealthStats(deviceID)
	drivers.GetCircuitBreaker().Reset(deviceID)
	drivers.GetReconnectManager().ResetReconnectState(deviceID)

	return s.deviceRepo.Delete(deviceID)
}

// ErrReadOnlyPoint is returned when a write targets a point that declares
// read-only access. The UI hid such points, which made a direct API call the
// only way to discover the restriction was client-side.
var ErrReadOnlyPoint = errors.New("point is read-only")

// ErrWriteRateLimited is returned when config.write_rate_limit (milliseconds)
// has not elapsed since the previous write to the same point. The flag used to
// be stored by PUT /devices/:id/write-policy and read by nobody, so an operator
// who throttled writes still hammered the device and saw no error.
var ErrWriteRateLimited = errors.New("write rate limit exceeded")

// ErrWriteVerifyFailed is returned when config.write_verify is set and the
// value read back from the device is not the value just written. It is the
// difference between "the gateway sent the frame" and "the device holds it".
var ErrWriteVerifyFailed = errors.New("write verification failed")

// ErrWriteNotAllowed is returned when the device declares a write_whitelist that
// does not include the caller.
var ErrWriteNotAllowed = errors.New("caller is not allowed to write this device")

const (
	// writeVerifyReadTimeout bounds the read-back so a wedged device cannot hang
	// the write request, which usually arrives without a deadline of its own.
	writeVerifyReadTimeout = 5 * time.Second
	// writeVerifyRetryDelay gives controllers that commit a register after the
	// write transaction a chance before the mismatch is reported.
	writeVerifyRetryDelay = 200 * time.Millisecond
	// writeVerifyFloatTolerance absorbs the precision a wire format loses, e.g.
	// 20.1 in a float32 register pair reads back as 20.099998.
	writeVerifyFloatTolerance = 1e-3
	// writeTimesPruneThreshold caps the throttle map; past it, stale entries go.
	writeTimesPruneThreshold = 1024
)

// WriteActor identifies the user behind a device write. The write whitelist is
// only enforceable when the caller says who is asking; a write with no actor is
// system-originated (rule output, northbound downlink) and is not filtered.
type WriteActor struct {
	UserID   string
	Username string
	Role     string
}

type writeActorKey struct{}

// WithWriteActor returns ctx carrying the user who is performing the write.
func WithWriteActor(ctx context.Context, actor WriteActor) context.Context {
	return context.WithValue(ctx, writeActorKey{}, actor)
}

// WriteActorFromContext returns the user a write was performed as, if any. A
// blank identity is not one: with a whitelist in place it must be refused, not
// matched against an empty string.
func WriteActorFromContext(ctx context.Context) (WriteActor, bool) {
	actor, ok := ctx.Value(writeActorKey{}).(WriteActor)
	if !ok {
		return actor, false
	}
	return actor, strings.TrimSpace(actor.UserID) != "" || strings.TrimSpace(actor.Username) != ""
}

// readOnlyAccessModes are the spellings that mean "cannot be written".
// 'r'/'rw'/'w' are the canonical values the device editor offers, but
// access_mode has never been validated on write, so stored devices also carry
// the long forms the protocol templates use ('read').
var readOnlyAccessModes = map[string]bool{
	"r":         true,
	"read":      true,
	"readonly":  true,
	"read_only": true,
}

// WritePoint writes a value to a device point.
func (s *DeviceService) WritePoint(ctx context.Context, deviceID string, req *models.WritePointRequest) error {
	device, err := s.deviceRepo.Get(deviceID)
	if err != nil {
		device = nil
	}

	// Reject here, where every device write path (point write, RPC command)
	// converges on the driver, rather than trusting callers to filter.
	if err := checkPointWritable(device, req.Point); err != nil {
		return err
	}
	var config map[string]interface{}
	if device != nil {
		config = device.Config
	}
	if err := checkWriteWhitelist(ctx, config); err != nil {
		return err
	}
	if err := s.enforceWriteRateLimit(deviceID, req.Point, config); err != nil {
		return err
	}

	s.mu.RLock()
	drv, ok := s.drivers[deviceID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("driver not found for device %s", deviceID)
	}

	if !drv.IsConnected() {
		if err := drv.Connect(ctx); err != nil {
			return fmt.Errorf("connect failed: %w", err)
		}
	}

	point := pointDefOf(device, req.Point)
	value, err := inversePointScaling(point, req.Value)
	if err != nil {
		return err
	}
	dataType := ""
	if point != nil {
		dataType = point.DataType
	}
	// Drivers whose wire format depends on the point's data type (a Modbus
	// register pair holding int32 vs float32) get the type; the rest keep the
	// value exactly as before.
	if err := writeDriverPoint(ctx, drv, req.Point, value, dataType, pointAddress(point)); err != nil {
		return err
	}
	if configBool(config, "write_verify") {
		return verifyDeviceWrite(ctx, drv, point, req.Point, value)
	}
	return nil
}

// pointAddress is the wire address of a point definition, or "" when the device
// carries no definition for the name the caller used.
func pointAddress(point *models.PointDef) string {
	if point == nil {
		return ""
	}
	return point.Address
}

// writeDriverPoint dispatches to the typed write interface when the driver has
// one, so the data type reaches the wire format that needs it.
func writeDriverPoint(ctx context.Context, drv drivers.Driver, point string, value interface{}, dataType, address string) error {
	// Drivers that address the wire (Modbus registers, S7 areas, OPC UA NodeIDs)
	// are handed the address, because that is the string their own parser accepts
	// and the operator's point name carries no address in it. Drivers that key
	// their state by name (the simulator's held values, an MQTT command field)
	// do not implement this and keep receiving the name.
	if aw, ok := drv.(drivers.WireAddressWriter); ok && address != "" {
		return aw.WritePointAtAddress(ctx, address, value, dataType)
	}
	if tw, ok := drv.(drivers.TypedWritePointer); ok {
		return tw.WritePointTyped(ctx, point, value, dataType)
	}
	return drv.WritePoint(ctx, point, value)
}

// verifyDeviceWrite reads the point back through the driver that just wrote it
// and compares. A device that answers with a different value, an error or a bad
// quality is a failed write, not a successful one with a footnote.
func verifyDeviceWrite(ctx context.Context, drv drivers.Driver, point *models.PointDef, name string, want interface{}) error {
	def := models.PointDef{Name: name}
	if point != nil {
		def = *point
	}
	readCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		readCtx, cancel = context.WithTimeout(ctx, writeVerifyReadTimeout)
		defer cancel()
	}

	got, err := readBackPoint(readCtx, drv, &def)
	if err == nil && writeValuesMatch(def.DataType, want, got) {
		return nil
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: cannot confirm write of %s: %v", ErrWriteVerifyFailed, name, ctx.Err())
	case <-time.After(writeVerifyRetryDelay):
	}
	got, err = readBackPoint(readCtx, drv, &def)
	if err == nil && writeValuesMatch(def.DataType, want, got) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: wrote %v to %s but reading it back failed (%v)", ErrWriteVerifyFailed, want, name, err)
	}
	return fmt.Errorf("%w: wrote %v to %s but the device reports %v", ErrWriteVerifyFailed, want, name, got)
}

// readBackPoint returns the device's own answer for one point.
func readBackPoint(ctx context.Context, drv drivers.Driver, pt *models.PointDef) (interface{}, error) {
	data, err := drv.ReadPoints(ctx, []models.PointDef{*pt})
	if err != nil {
		return nil, err
	}
	for _, pd := range data {
		if pd.PointName != pt.Name {
			continue
		}
		if pd.Quality != "" && pd.Quality != "good" {
			return nil, fmt.Errorf("device reported quality %q", pd.Quality)
		}
		if pd.Value == nil {
			return nil, errors.New("device returned no value")
		}
		return pd.Value, nil
	}
	return nil, errors.New("device did not answer for this point")
}

// writeValuesMatch compares the written wire value with the read-back value.
// Integers must match exactly; floats get writeVerifyFloatTolerance because the
// register itself cannot hold the requested decimal.
func writeValuesMatch(dataType string, want, got interface{}) bool {
	if wf, ok := pointValueAsFloat(want); ok {
		gf, ok := pointValueAsFloat(got)
		if !ok {
			return false
		}
		if isIntegralDataType(dataType) {
			return math.Round(wf) == math.Round(gf)
		}
		tolerance := math.Abs(wf) * writeVerifyFloatTolerance
		if tolerance < 1e-6 {
			tolerance = 1e-6
		}
		return math.Abs(wf-gf) <= tolerance
	}
	if wb, ok := want.(bool); ok {
		gb, ok := got.(bool)
		return ok && wb == gb
	}
	return fmt.Sprintf("%v", want) == fmt.Sprintf("%v", got)
}

// enforceWriteRateLimit throttles writes to one point at the interval the device
// declares. Admitting the write stamps the timestamp, so a caller that retries a
// rejected write cannot slip through.
func (s *DeviceService) enforceWriteRateLimit(deviceID, point string, config map[string]interface{}) error {
	limit := writeRateLimit(config)
	if limit <= 0 {
		return nil
	}
	key := deviceID + "\x00" + point

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now()
	if last, ok := s.lastWrite[key]; ok {
		if wait := last.Add(limit).Sub(now); wait > 0 {
			return fmt.Errorf("%w: next write to %s allowed in %s",
				ErrWriteRateLimited, point, wait.Round(time.Millisecond))
		}
	}
	if len(s.lastWrite) >= writeTimesPruneThreshold {
		s.pruneWriteTimesLocked(now, limit)
	}
	s.lastWrite[key] = now
	return nil
}

// pruneWriteTimesLocked drops entries older than the current limit, which can
// never gate a write again. If nothing is old enough to drop the map is simply
// allowed to grow by one; the throttle is best effort, not a quota ledger.
func (s *DeviceService) pruneWriteTimesLocked(now time.Time, limit time.Duration) {
	for key, at := range s.lastWrite {
		if now.Sub(at) > limit {
			delete(s.lastWrite, key)
		}
	}
}

// writeRateLimit reads config.write_rate_limit as milliseconds. A missing, zero
// or negative value disables throttling.
func writeRateLimit(config map[string]interface{}) time.Duration {
	if config == nil {
		return 0
	}
	f, ok := pointValueAsFloat(config["write_rate_limit"])
	if !ok || f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Millisecond))
}

// configBool reads a device config flag. Absent means the default the UI offers.
func configBool(config map[string]interface{}, key string) bool {
	if config == nil {
		return false
	}
	v, ok := config[key].(bool)
	return ok && v
}

// checkWriteWhitelist rejects the write when the device lists the users allowed
// to write it and the caller is not one of them.
func checkWriteWhitelist(ctx context.Context, config map[string]interface{}) error {
	allowed := configStringList(config, "write_whitelist")
	if len(allowed) == 0 {
		return nil
	}
	actor, ok := WriteActorFromContext(ctx)
	if !ok {
		return fmt.Errorf("%w: the device carries a write whitelist and this request has no user identity",
			ErrWriteNotAllowed)
	}
	if strings.EqualFold(actor.Role, security.RoleAdmin) {
		return nil
	}
	for _, entry := range allowed {
		if entry == actor.UserID || strings.EqualFold(entry, actor.Username) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not in the device write whitelist", ErrWriteNotAllowed, actor.Username)
}

// configStringList reads a []string, []interface{} or comma separated string.
func configStringList(config map[string]interface{}, key string) []string {
	if config == nil {
		return nil
	}
	switch v := config[key].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		for _, part := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n'
		}) {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// pointDefOf returns the stored definition of a point, or nil when the device
// does not declare it.
func pointDefOf(device *models.DeviceResponse, name string) *models.PointDef {
	if device == nil {
		return nil
	}
	for i := range device.Points {
		if device.Points[i].Name == name {
			return &device.Points[i]
		}
	}
	return nil
}

// checkPointWritable wraps ErrReadOnlyPoint when the named point declares
// read-only access. Unknown point names and empty access modes pass through:
// the driver is the authority on whether the address exists, and devices
// created before access_mode existed carry none.
func checkPointWritable(device *models.DeviceResponse, pointName string) error {
	if device == nil {
		return nil
	}
	for _, p := range device.Points {
		if p.Name != pointName {
			continue
		}
		if readOnlyAccessModes[strings.ToLower(strings.TrimSpace(p.AccessMode))] {
			return fmt.Errorf("%w: %s", ErrReadOnlyPoint, pointName)
		}
	}
	return nil
}

// PushData pushes data from external sources (webhook/MQTT) to a device.
func (s *DeviceService) PushData(deviceID string, req *models.PushDeviceDataRequest) error {
	s.mu.RLock()
	drv, ok := s.drivers[deviceID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("driver not found for device %s", deviceID)
	}

	// If it's an HTTP webhook driver, forward the data
	if wh, ok := drv.(*drivers.HTTPWebhookDriver); ok {
		payload, err := json.Marshal(req.Data)
		if err != nil {
			return fmt.Errorf("encode push payload: %w", err)
		}
		return wh.HandleWebhook(payload)
	}

	return fmt.Errorf("push data not supported for protocol: %s", drv.Name())
}

// Discover discovers devices on the network.
func (s *DeviceService) Discover(ctx context.Context, req *models.DiscoverRequest) ([]map[string]interface{}, error) {
	protocol := constants.NormalizeProtocol(req.Protocol)
	if protocol == "" {
		return nil, fmt.Errorf("unsupported protocol: %s", req.Protocol)
	}

	// Create a temporary driver for discovery
	driver, err := s.driverRegistry.CreateDriver(protocol, "discovery", req.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery driver: %w", err)
	}

	return driver.Discover(ctx, req.Config)
}

// ErrBrowseUnsupported reports that a device's protocol has no address-space
// browse service. Without it the API had to answer "no nodes" for a question it
// cannot ask the device, which is indistinguishable from an empty server.
var ErrBrowseUnsupported = errors.New("protocol does not support node browsing")

// BrowseNodes lists the children of a node in a live device's address space.
// It connects the device's driver on demand, because browsing is an interactive
// step a user takes while configuring that device.
func (s *DeviceService) BrowseNodes(ctx context.Context, deviceID, nodeID string) ([]drivers.BrowseEntry, error) {
	s.mu.RLock()
	drv, ok := s.drivers[deviceID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("driver not found for device %s", deviceID)
	}
	browser, canBrowse := drv.(drivers.NodeBrowser)
	if !canBrowse {
		return nil, fmt.Errorf("%w: %s", ErrBrowseUnsupported, drv.Name())
	}
	if !drv.IsConnected() {
		if err := drv.Connect(ctx); err != nil {
			return nil, fmt.Errorf("connect failed: %w", err)
		}
	}
	return browser.BrowseChildren(ctx, nodeID)
}

// CreateTemplate creates a device template from an existing device.
func (s *DeviceService) CreateTemplate(req *models.TemplateCreate) (*models.TemplateResponse, error) {
	device, err := s.deviceRepo.Get(req.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("device not found: %w", err)
	}
	if device == nil {
		return nil, fmt.Errorf("device not found: %s", req.DeviceID)
	}

	template := &models.TemplateResponse{
		Name:           req.TemplateName,
		Protocol:       device.Protocol,
		ConfigTemplate: device.Config,
		PointTemplates: device.Points,
		CreatedAt:      time.Now().Format(time.RFC3339),
	}

	if err := s.templateRepo.Create(template); err != nil {
		return nil, fmt.Errorf("failed to create template: %w", err)
	}

	return template, nil
}

// ListTemplates returns all templates.
func (s *DeviceService) ListTemplates() ([]models.TemplateResponse, error) {
	return s.templateRepo.List()
}

// DeleteTemplate deletes a template.
func (s *DeviceService) DeleteTemplate(name string) error {
	return s.templateRepo.Delete(name)
}

// CreateFromTemplate creates a device from a template.
func (s *DeviceService) CreateFromTemplate(req *models.CreateFromTemplateRequest, createdBy string) (*models.DeviceResponse, error) {
	template, err := s.templateRepo.Get(req.TemplateName)
	if err != nil {
		return nil, fmt.Errorf("template not found: %w", err)
	}
	if template == nil {
		return nil, fmt.Errorf("template not found: %s", req.TemplateName)
	}

	// Merge provided config with template config
	config := template.ConfigTemplate
	for k, v := range req.Config {
		config[k] = v
	}

	createReq := &models.DeviceCreate{
		DeviceID:       req.DeviceID,
		Name:           req.Name,
		Protocol:       template.Protocol,
		Config:         config,
		Points:         template.PointTemplates,
		CollectInterval: req.CollectInterval,
	}

	return s.Create(createReq, createdBy)
}

// ExportDevices exports device configurations.
func (s *DeviceService) ExportDevices(req *models.ExportDevicesRequest) ([]map[string]interface{}, error) {
	var devices []models.DeviceResponse
	var err error
	if len(req.DeviceIDs) == 0 {
		devices, _, err = s.deviceRepo.List(1, constants.ExportQuerySize)
	} else {
		devices = make([]models.DeviceResponse, 0, len(req.DeviceIDs))
		for _, id := range req.DeviceIDs {
			d, e := s.deviceRepo.Get(id)
			if e != nil {
				err = e
				break
			}
			if d != nil {
				devices = append(devices, *d)
			}
		}
	}
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, len(devices))
	for i, d := range devices {
		data, err := json.Marshal(d)
		if err != nil {
			logrus.WithError(err).WithField("device_id", d.DeviceID).Warn("Failed to marshal device for export")
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			logrus.WithError(err).WithField("device_id", d.DeviceID).Warn("Failed to unmarshal device for export")
			continue
		}
		result[i] = m
	}
	return result, nil
}

// ImportDevices imports device configurations.
// The number of devices per import is capped at 1000 to prevent abuse.
func (s *DeviceService) ImportDevices(req *models.ImportDevicesRequest, createdBy string) (int, int, error) {
	// Cap the number of devices per import to prevent DoS
	const maxImportBatch = 1000
	if len(req.Data) > maxImportBatch {
		return 0, 0, fmt.Errorf("import batch exceeds limit of %d devices", maxImportBatch)
	}

	imported := 0
	failed := 0
	for _, data := range req.Data {
		deviceID, _ := data["device_id"].(string)
		name, _ := data["name"].(string)
		protocol, _ := data["protocol"].(string)
		collectInterval := 0
		if ci, ok := data["collect_interval"].(float64); ok {
			collectInterval = int(ci)
		}
		var config map[string]interface{}
		if c, ok := data["config"].(map[string]interface{}); ok {
			config = c
		}
		var points []models.PointDef
		if p, ok := data["points"].([]interface{}); ok {
			pData, _ := json.Marshal(p)
			if err := json.Unmarshal(pData, &points); err != nil {
				failed++
				logrus.WithField("device_id", deviceID).
					WithField("error", err.Error()).
					Warn("Import device failed: invalid points data")
				continue
			}
		}

		createReq := &models.DeviceCreate{
			DeviceID:       deviceID,
			Name:           name,
			Protocol:       protocol,
			Config:         config,
			Points:         points,
			CollectInterval: collectInterval,
		}

		_, err := s.Create(createReq, createdBy)
		if err != nil {
			failed++
			logrus.WithField("device_id", deviceID).
				WithField("error", err.Error()).
				Warn("Import device failed")
		} else {
			imported++
		}
	}
	return imported, failed, nil
}

// --- Rule Service ---

// RuleService handles rule management operations.
type RuleService struct {
	ruleRepo     *storage.RuleRepo
	evaluator    *engine.RuleEvaluator
}

// NewRuleService creates a new RuleService.
func NewRuleService(ruleRepo *storage.RuleRepo, evaluator *engine.RuleEvaluator) *RuleService {
	return &RuleService{ruleRepo: ruleRepo, evaluator: evaluator}
}

// Create creates a new rule.
func (s *RuleService) Create(req *models.RuleCreate, createdBy string) (*models.RuleResponse, error) {
	ruleID := fmt.Sprintf("rule-%s", uuid.New().String()[:8])

	logic := req.Logic
	if logic == "" {
		logic = "AND"
	}
	severity := req.Severity
	if severity == "" {
		severity = "warning"
	}
	ruleType := req.RuleType
	if ruleType == "" {
		ruleType = "threshold"
	}

	rule := &models.RuleResponse{
		RuleID:         ruleID,
		Name:           req.Name,
		DeviceID:       req.DeviceID,
		Conditions:     req.Conditions,
		Logic:          logic,
		Duration:       req.Duration,
		Severity:       severity,
		Enabled:        true,
		NotifyChannels: req.NotifyChannels,
		Script:         req.Script,
		RuleType:       ruleType,
		CreatedAt:      time.Now().Format(time.RFC3339),
		UpdatedAt:      time.Now().Format(time.RFC3339),
		CreatedBy:      createdBy,
		Version:        1,
	}

	if err := s.ruleRepo.Create(rule, createdBy); err != nil {
		return nil, fmt.Errorf("failed to create rule: %w", err)
	}

	// Load into evaluator
	if s.evaluator != nil {
		s.evaluator.LoadRule(rule)
	}

	logrus.WithField("rule_id", ruleID).Info("Rule created")
	return rule, nil
}

// Get retrieves a rule by ID.
func (s *RuleService) Get(ruleID string) (*models.RuleResponse, error) {
	return s.ruleRepo.Get(ruleID)
}

// List returns rules with pagination.
func (s *RuleService) List(page, size int, deviceID string) ([]models.RuleResponse, int, error) {
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > constants.MaxQuerySize {
		size = constants.DefaultPageSize
	}
	return s.ruleRepo.List(page, size, deviceID)
}

// Update updates a rule.
func (s *RuleService) Update(ruleID string, req *models.RuleUpdate) (*models.RuleResponse, error) {
	if err := s.ruleRepo.Update(ruleID, req); err != nil {
		return nil, fmt.Errorf("failed to update rule: %w", err)
	}

	rule, err := s.ruleRepo.Get(ruleID)
	if err != nil {
		return nil, err
	}
	if rule == nil {
		return nil, fmt.Errorf("rule not found: %s", ruleID)
	}

	// Reload into evaluator
	if s.evaluator != nil {
		s.evaluator.UnloadRule(ruleID)
		s.evaluator.LoadRule(rule)
	}

	return rule, nil
}

// Delete deletes a rule.
func (s *RuleService) Delete(ruleID string) error {
	// Close what the rule still holds open before it stops being evaluated;
	// UnloadRule drops the evaluator's rule→alarm mapping, and without this the
	// stored alarm stayed "firing" under a rule that no longer exists.
	if s.evaluator != nil {
		if rule, _ := s.ruleRepo.Get(ruleID); rule != nil {
			if err := s.evaluator.CloseRuleAlarms(rule); err != nil {
				return err
			}
		}
		s.evaluator.UnloadRule(ruleID)
	}
	return s.ruleRepo.Delete(ruleID)
}

// SetEnabled enables or disables a rule.
func (s *RuleService) SetEnabled(ruleID string, enabled bool) error {
	if err := s.ruleRepo.SetEnabled(ruleID, enabled); err != nil {
		return err
	}
	if s.evaluator != nil {
		rule, _ := s.ruleRepo.Get(ruleID)
		if rule != nil {
			if enabled {
				s.evaluator.LoadRule(rule)
			} else {
				// A disabled rule is never evaluated again, so it can no longer
				// recover the alarm it raised. Close it here instead of leaving the
				// alarm firing with nothing behind it but a toggle nobody could undo.
				if err := s.evaluator.CloseRuleAlarms(rule); err != nil {
					return err
				}
				s.evaluator.UnloadRule(ruleID)
			}
		}
	}
	return nil
}

// TestRule tests a rule against sample values.
func (s *RuleService) TestRule(req *models.RuleTestRequest, conditions []models.RuleCondition, logic string) (bool, map[string]interface{}, error) {
	results := make([]bool, len(conditions))
	triggerValues := make(map[string]interface{})

	for i, cond := range conditions {
		val, ok := req.PointValues[cond.Point]
		if !ok {
			results[i] = false
			continue
		}
		matched := engine.EvaluateConditionPublic(cond.Operator, val, cond.Threshold)
		results[i] = matched
		if matched {
			triggerValues[cond.Point] = val
		}
	}

	var triggered bool
	switch logic {
	case "AND":
		triggered = true
		for _, r := range results {
			if !r {
				triggered = false
				break
			}
		}
	case "OR":
		for _, r := range results {
			if r {
				triggered = true
				break
			}
		}
	case "NOT":
		triggered = true
		for _, r := range results {
			if r {
				triggered = false
				break
			}
		}
	default:
		triggered = true
		for _, r := range results {
			if !r {
				triggered = false
				break
			}
		}
	}

	return triggered, triggerValues, nil
}

// --- Alarm Service ---

// AlarmSeverity levels
const (
	SeverityCritical = "critical"
	SeverityMajor    = "major"
	SeverityMinor    = "minor"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// AlarmEscalationConfig defines escalation policy per severity.
type AlarmEscalationConfig struct {
	Severity       string
	ThresholdSecs  int
	EscalateTo     string
	NotifyChannels []string
}

// AlarmStatistics holds aggregate alarm statistics.
type AlarmStatistics struct {
	TotalCount      int            `json:"total_count"`
	FiringCount     int            `json:"firing_count"`
	AckedCount      int            `json:"acknowledged_count"`
	RecoveredCount  int            `json:"recovered_count"`
	EscalatedCount  int            `json:"escalated_count"`
	BySeverity      map[string]int `json:"by_severity"`
	MTTRSeconds     float64        `json:"mttr_seconds"`
	MTBFSeconds     float64        `json:"mtbf_seconds"`
}

// AlarmService handles alarm management operations.
// This is a 1:1 port of Python's AlarmService, including escalation,
// suppression, statistics, and notification integration.
type AlarmService struct {
	alarmRepo  *storage.AlarmRepo
	evaluator  *engine.RuleEvaluator
	notifySvc  *NotifyService

	mu                   sync.Mutex
	escalationConfigs    map[string]AlarmEscalationConfig
	suppressionRules     []AlarmSuppressionRule
	stats                AlarmStatistics
	alarmStartTimes      map[string]time.Time
	lastFireTime         time.Time
	firstAlarmTime       time.Time
	originalSeverities   map[string]string
	handledAlarmIDs     map[string]bool
}

// AlarmSuppressionRule defines a rule for suppressing alarms.
type AlarmSuppressionRule struct {
	RuleID       string
	Name         string
	DeviceIDs    []string
	RuleIDs      []string
	Severities   []string
	TimeStart    string
	TimeEnd      string
	Enabled      bool
	ExpiresAt    time.Time
}

// NewAlarmService creates a new AlarmService.
func NewAlarmService(alarmRepo *storage.AlarmRepo, evaluator *engine.RuleEvaluator) *AlarmService {
	s := &AlarmService{
		alarmRepo:         alarmRepo,
		evaluator:         evaluator,
		alarmStartTimes:   make(map[string]time.Time),
		originalSeverities: make(map[string]string),
		handledAlarmIDs:   make(map[string]bool),
	}
	s.escalationConfigs = map[string]AlarmEscalationConfig{
		SeverityCritical: {Severity: SeverityCritical, ThresholdSecs: 300, EscalateTo: SeverityCritical},
		SeverityMajor:    {Severity: SeverityMajor, ThresholdSecs: 900, EscalateTo: SeverityCritical},
		SeverityMinor:    {Severity: SeverityMinor, ThresholdSecs: 1800, EscalateTo: SeverityMajor},
		SeverityWarning:  {Severity: SeverityWarning, ThresholdSecs: 3600, EscalateTo: SeverityMinor},
	}
	s.stats.BySeverity = make(map[string]int)
	// The evaluator owns the alarm transitions, and it cannot import this package,
	// so the wiring runs the other way: without this hook an alarm is written to
	// the database and pushed to the UI, but no operator is ever notified.
	if evaluator != nil {
		evaluator.SetAlarmHooks(engine.AlarmHooks{
			OnFired: func(alarm *models.AlarmResponse, rule *models.RuleResponse) {
				if alarm == nil {
					return
				}
				ev := AlarmFiredEvent{
					AlarmID:      alarm.AlarmID,
					RuleID:       alarm.RuleID,
					DeviceID:     alarm.DeviceID,
					Severity:     alarm.Severity,
					Message:      alarm.Message,
					TriggerValue: alarm.TriggerValue,
					TriggerCount: alarm.TriggerCount,
				}
				if rule != nil {
					ev.RuleName = rule.Name
					ev.Channels = rule.NotifyChannels
				}
				s.OnAlarmFired(ev)
			},
			OnRecovered: func(alarmID string, rule *models.RuleResponse) {
				s.OnAlarmRecovered(alarmID)
			},
		})
	}
	return s
}

// SetNotifyService injects the notification service for alarm notifications.
func (s *AlarmService) SetNotifyService(ns *NotifyService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifySvc = ns
}

// Get retrieves an alarm by ID.
func (s *AlarmService) Get(alarmID string) (*models.AlarmResponse, error) {
	return s.alarmRepo.Get(alarmID)
}

// List returns alarms with filters and pagination.
func (s *AlarmService) List(filter models.AlarmFilter, page, size int) ([]models.AlarmResponse, int, error) {
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > constants.MaxQuerySize {
		size = constants.DefaultPageSize
	}
	return s.alarmRepo.List(filter, page, size)
}

// Acknowledge acknowledges an alarm.
func (s *AlarmService) Acknowledge(alarmID, userID string) error {
	return s.alarmRepo.Acknowledge(alarmID, userID)
}

// Recover marks an alarm as recovered and updates statistics.
func (s *AlarmService) Recover(alarmID string) error {
	if err := s.alarmRepo.Recover(alarmID); err != nil {
		return err
	}
	s.OnAlarmRecovered(alarmID)
	return nil
}

// Delete removes an alarm from the database.
func (s *AlarmService) Delete(alarmID string) error {
	return s.alarmRepo.Delete(alarmID)
}

// BatchAcknowledge acknowledges multiple alarms.
func (s *AlarmService) BatchAcknowledge(req *models.AlarmBatchAckRequest, userID string) (int, int, error) {
	acked := 0
	failed := 0
	for _, alarmID := range req.AlarmIDs {
		if err := s.alarmRepo.Acknowledge(alarmID, userID); err != nil {
			failed++
		} else {
			acked++
		}
	}
	return acked, failed, nil
}

// GetStatistics returns aggregate alarm statistics.
func (s *AlarmService) GetStatistics() AlarmStatistics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// IsSuppressed checks if an alarm should be suppressed.
func (s *AlarmService) IsSuppressed(deviceID, ruleID, severity string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, rule := range s.suppressionRules {
		if !rule.Enabled {
			continue
		}
		if !rule.ExpiresAt.IsZero() && now.After(rule.ExpiresAt) {
			continue
		}
		// Check device match
		deviceMatch := len(rule.DeviceIDs) == 0
		for _, d := range rule.DeviceIDs {
			if d == deviceID {
				deviceMatch = true
				break
			}
		}
		// Check rule match
		ruleMatch := len(rule.RuleIDs) == 0
		for _, r := range rule.RuleIDs {
			if r == ruleID {
				ruleMatch = true
				break
			}
		}
		// Check severity match
		sevMatch := len(rule.Severities) == 0
		for _, sv := range rule.Severities {
			if sv == severity {
				sevMatch = true
				break
			}
		}
		if deviceMatch && ruleMatch && sevMatch {
			return true
		}
	}
	return false
}

// AddSuppressionRule adds a suppression rule.
func (s *AlarmService) AddSuppressionRule(rule AlarmSuppressionRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suppressionRules = append(s.suppressionRules, rule)
	logrus.WithField("rule_id", rule.RuleID).Info("Alarm suppression rule added")
}

// RemoveSuppressionRule removes a suppression rule by ID.
func (s *AlarmService) RemoveSuppressionRule(ruleID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.suppressionRules {
		if r.RuleID == ruleID {
			s.suppressionRules = append(s.suppressionRules[:i], s.suppressionRules[i+1:]...)
			break
		}
	}
}

// ListSuppressionRules returns all suppression rules.
func (s *AlarmService) ListSuppressionRules() []AlarmSuppressionRule {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]AlarmSuppressionRule, len(s.suppressionRules))
	copy(result, s.suppressionRules)
	return result
}

// AlarmFiredEvent describes one alarm transition handed over by the rule
// evaluator. It is a struct rather than a positional list because the
// notification text needs the rule name and trigger values, not just the IDs.
type AlarmFiredEvent struct {
	AlarmID      string
	RuleID       string
	RuleName     string
	DeviceID     string
	Severity     string
	Message      string
	Channels     []string
	TriggerValue map[string]interface{}
	TriggerCount int
}

// OnAlarmFired is called when an alarm fires. Updates statistics and sends notifications.
func (s *AlarmService) OnAlarmFired(ev AlarmFiredEvent) {
	alarmID, ruleID, severity := ev.AlarmID, ev.RuleID, ev.Severity
	s.mu.Lock()
	// Check if already handled
	if s.handledAlarmIDs[alarmID] {
		s.mu.Unlock()
		return
	}
	s.handledAlarmIDs[alarmID] = true

	// Update statistics
	s.stats.TotalCount++
	s.stats.FiringCount++
	s.stats.BySeverity[severity]++

	// Track start time for MTTR
	s.alarmStartTimes[alarmID] = time.Now()

	// Track first alarm time and MTBF
	now := time.Now()
	if s.firstAlarmTime.IsZero() {
		s.firstAlarmTime = now
	}
	if !s.lastFireTime.IsZero() {
		interval := now.Sub(s.lastFireTime).Seconds()
		if s.stats.MTBFSeconds == 0 {
			s.stats.MTBFSeconds = interval
		} else {
			s.stats.MTBFSeconds = (s.stats.MTBFSeconds + interval) / 2
		}
	}
	s.lastFireTime = now

	// Store original severity for escalation
	s.originalSeverities[alarmID] = severity
	ns := s.notifySvc
	s.mu.Unlock()

	// Send notification (outside lock to avoid deadlock)
	if ns == nil {
		return
	}
	notif := &AlarmNotification{
		AlarmID:      alarmID,
		RuleID:       ruleID,
		RuleName:     ev.RuleName,
		DeviceID:     ev.DeviceID,
		DeviceName:   ev.DeviceID,
		Severity:     severity,
		Action:       "firing",
		Message:      ev.Message,
		TriggerValue: ev.TriggerValue,
		TriggerCount: ev.TriggerCount,
		Timestamp:    time.Now().Format(time.RFC3339),
	}
	results := ns.SendAlarmNotification(notif, ev.Channels)
	for ch, delivered := range results {
		if !delivered {
			logrus.WithFields(logrus.Fields{"alarm_id": alarmID, "rule_id": ruleID, "channel": ch}).
				Warn("Alarm notification channel did not deliver")
		}
	}
}

// OnAlarmRecovered is called when an alarm recovers. Updates statistics.
func (s *AlarmService) OnAlarmRecovered(alarmID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stats.RecoveredCount++
	s.stats.FiringCount--
	if s.stats.FiringCount < 0 {
		s.stats.FiringCount = 0
	}

	// Calculate MTTR
	if startTime, ok := s.alarmStartTimes[alarmID]; ok {
		recoveryTime := time.Since(startTime).Seconds()
		if s.stats.MTTRSeconds == 0 {
			s.stats.MTTRSeconds = recoveryTime
		} else {
			s.stats.MTTRSeconds = (s.stats.MTTRSeconds + recoveryTime) / 2
		}
		delete(s.alarmStartTimes, alarmID)
	}

	// Cleanup
	delete(s.originalSeverities, alarmID)
	delete(s.handledAlarmIDs, alarmID)
}

// OnAlarmAcknowledged is called when an alarm is acknowledged.
func (s *AlarmService) OnAlarmAcknowledged(alarmID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.AckedCount++
}

// --- Data Service ---

// DataService handles data query operations.
type DataService struct {
	tsStorage *storage.TimeSeriesStorage
	cache     *storage.CacheManager
}

// NewDataService creates a new DataService.
func NewDataService(tsStorage *storage.TimeSeriesStorage, cache *storage.CacheManager) *DataService {
	return &DataService{tsStorage: tsStorage, cache: cache}
}

// QueryHistory queries historical data.
func (s *DataService) QueryHistory(deviceID, pointName string, startTime, endTime time.Time) ([]storage.PointData, error) {
	return s.tsStorage.QueryPoints(deviceID, pointName, startTime, endTime)
}

// GetLatest returns the latest values for all points of a device.
func (s *DataService) GetLatest(deviceID string) (map[string]storage.PointData, error) {
	return s.tsStorage.GetLatestPoints(deviceID)
}

// GetCached returns all cached data points.
func (s *DataService) GetCached() []storage.PointData {
	return s.cache.GetAll()
}

// --- System Service ---

// SystemService handles system operations.
type SystemService struct {
	config      interface{} // *config.AppConfig
	startTime   time.Time
}

// NewSystemService creates a new SystemService.
func NewSystemService(cfg interface{}) *SystemService {
	return &SystemService{
		config:    cfg,
		startTime: time.Now(),
	}
}

// GetSystemInfo returns system information.
func (s *SystemService) GetSystemInfo() map[string]interface{} {
	return map[string]interface{}{
		"version":     "1.0.0-go",
		"uptime_s":   time.Since(s.startTime).Seconds(),
		"start_time":  s.startTime.Format(time.RFC3339),
		"go_runtime":  true,
	}
}

// --- Notification Service ---

// NotifyService handles notification dispatch to multiple channels.
// Supports DingTalk, WeCom, Email (SMTP), and custom Webhook channels.
// This is a 1:1 port of Python's NotificationManager.
type NotifyService struct {
	mu          sync.Mutex
	notifyCfg   interface{} // *config.NotifyConfig
	httpClient  *http.Client
	// throttles holds the per-channel storm state behind
	// notify.<channel>.max_per_minute and cooldown_seconds.
	throttles map[string]*channelThrottle
}

// NewNotifyService creates a new NotifyService.
func NewNotifyService() *NotifyService {
	return &NotifyService{
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// SetConfig injects the notification configuration.
func (s *NotifyService) SetConfig(cfg interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyCfg = cfg
}

// AlarmNotification represents a structured alarm notification payload.
type AlarmNotification struct {
	AlarmID         string                 `json:"alarm_id"`
	RuleID          string                 `json:"rule_id"`
	RuleName        string                 `json:"rule_name"`
	DeviceID        string                 `json:"device_id"`
	DeviceName      string                 `json:"device_name"`
	Severity        string                 `json:"severity"`
	Action          string                 `json:"action"`
	Message         string                 `json:"message"`
	TriggerValue    map[string]interface{} `json:"trigger_value"`
	TriggerCount    int                    `json:"trigger_count"`
	EscalationLevel int                    `json:"escalation_level"`
	OriginalSeverity string               `json:"original_severity"`
	Timestamp       string                 `json:"timestamp"`
}

// ErrNotifyNotConfigured marks a channel that has no usable settings. A
// multi-channel fan-out skips it (nothing was asked to deliver), while a
// single-channel test surfaces it, so "no channel configured" can never be
// reported to the UI as a successful send.
var ErrNotifyNotConfigured = errors.New("notification channel is not configured")

// defaultNotifyChannels is where a notification goes when the caller (or the
// rule) names none.
var defaultNotifyChannels = []string{"dingtalk", "email", "webhook"}

type notConfiguredError struct{ msg string }

func (e notConfiguredError) Error() string { return e.msg }
func (e notConfiguredError) Unwrap() error { return ErrNotifyNotConfigured }

func notConfigured(channel, reason string) error {
	return notConfiguredError{fmt.Sprintf("%s channel is not configured%s", channel, reason)}
}

// SendNotification sends a notification to the specified channels.
// If channels is empty, sends to all configured channels.
func (s *NotifyService) SendNotification(channels []string, title, message string, severity string) error {
	s.mu.Lock()
	cfg, _ := s.getNotifyConfig()
	s.mu.Unlock()
	return s.sendToChannels(cfg, channels, title, message, severity)
}

// SendNotificationWith dispatches using a caller-supplied config instead of the
// service's own, so the notification page can test a channel whose settings are
// not saved yet.
func (s *NotifyService) SendNotificationWith(cfg *config.NotifyConfig, channels []string, title, message string, severity string) error {
	return s.sendToChannels(cfg, channels, title, message, severity)
}

// sendToChannels fans one message out to the named channels and reports every
// channel that was expected to deliver but failed. Channels with no usable
// settings are skipped: a rule that lists three channels must not look broken
// just because two of them were never configured. Delivery runs without holding
// the service lock, so a slow channel cannot stall the others.
func (s *NotifyService) sendToChannels(cfg *config.NotifyConfig, channels []string, title, message string, severity string) error {
	if len(channels) == 0 {
		channels = defaultNotifyChannels
	}

	var failures []string
	for _, ch := range channels {
		err := s.dispatchChannel(cfg, ch, title, message, severity)
		if err != nil && !errors.Is(err, ErrNotifyNotConfigured) {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// dispatchChannel sends to exactly one channel and returns the raw outcome,
// including the not-configured case, so single-channel callers (the "test this
// channel" button) can surface why nothing arrived.
func (s *NotifyService) dispatchChannel(cfg *config.NotifyConfig, channel, title, message, severity string) error {
	commit, err := s.acquireSlot(cfg, channel, title, message)
	if err != nil {
		return err
	}
	err = s.sendToChannel(cfg, channel, title, message, severity)
	commit(err == nil)
	return err
}

// sendToChannel performs the actual HTTP/SMTP delivery for one channel.
func (s *NotifyService) sendToChannel(cfg *config.NotifyConfig, channel, title, message, severity string) error {
	switch channel {
	case "dingtalk":
		return s.sendDingTalk(cfg, title, message, severity)
	case "email":
		return s.sendEmail(cfg, title, message, severity)
	case "webhook":
		return s.sendWebhook(cfg, title, message, severity)
	case "wechat", "wecom":
		return s.sendWeCom(cfg, title, message, severity)
	}
	logrus.WithField("channel", channel).Debug("Unknown notification channel")
	return fmt.Errorf("unknown notification channel %q", channel)
}

// ErrNotifySuppressed marks a message the channel's own storm limits refused to
// send. It is not a delivery failure to fix, but it must never be reported as
// delivered either.
var ErrNotifySuppressed = errors.New("notification suppressed by channel storm limits")

// notifyLimits returns the suppression settings a channel advertises in the
// config. Both knobs were editable in the UI and read by nothing, so a flapping
// rule could emit one notification per evaluation forever -- DingTalk bans a
// robot that exceeds 20 messages per minute for 50 minutes, which turns one
// noisy rule into every alarm being lost. WeCom robots are capped the same way,
// so their section carries the same two fields.
func notifyLimits(cfg *config.NotifyConfig, channel string) (maxPerMinute int, cooldown time.Duration) {
	if cfg == nil {
		return 0, 0
	}
	switch channel {
	case "dingtalk":
		return cfg.Dingtalk.MaxPerMinute, time.Duration(cfg.Dingtalk.CooldownSeconds * float64(time.Second))
	case "email":
		return cfg.Email.MaxPerMinute, time.Duration(cfg.Email.CooldownSeconds * float64(time.Second))
	case "webhook":
		return cfg.Webhook.MaxPerMinute, time.Duration(cfg.Webhook.CooldownSeconds * float64(time.Second))
	case "wechat", "wecom":
		return cfg.Wechat.MaxPerMinute, time.Duration(cfg.Wechat.CooldownSeconds * float64(time.Second))
	}
	// Unknown channels have no limits to read.
	return 0, 0
}

// channelThrottle holds one channel's rate state.
type channelThrottle struct {
	mu         sync.Mutex
	window     []time.Time          // successful sends inside the rolling minute
	lastByMsg  map[string]time.Time // identical message -> last successful send
	suppressed uint64
}

// acquireSlot checks the channel's limits before a send. The returned func must
// be called with whether delivery succeeded: only a sent message consumes the
// cooldown or the per-minute budget, so a failing endpoint cannot lock the
// channel out.
func (s *NotifyService) acquireSlot(cfg *config.NotifyConfig, channel, title, message string) (func(bool), error) {
	noop := func(bool) {}
	maxPerMinute, cooldown := notifyLimits(cfg, channel)
	if maxPerMinute <= 0 && cooldown <= 0 {
		return noop, nil
	}

	s.mu.Lock()
	if s.throttles == nil {
		s.throttles = make(map[string]*channelThrottle)
	}
	th := s.throttles[channel]
	if th == nil {
		th = &channelThrottle{lastByMsg: make(map[string]time.Time)}
		s.throttles[channel] = th
	}
	s.mu.Unlock()

	now := time.Now()
	key := title + "\n" + message

	th.mu.Lock()
	defer th.mu.Unlock()
	reason := ""
	if cooldown > 0 {
		if last, ok := th.lastByMsg[key]; ok && now.Sub(last) < cooldown {
			// Scoped to the identical text on purpose: a blanket per-channel
			// cooldown would drop the *next* device's alarm.
			reason = fmt.Sprintf("identical message was sent %.0fs ago, cooldown_seconds=%.0fs",
				now.Sub(last).Seconds(), cooldown.Seconds())
		}
	}
	if reason == "" && maxPerMinute > 0 {
		cutoff := now.Add(-time.Minute)
		kept := th.window[:0]
		for _, t := range th.window {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		th.window = kept
		if len(th.window) >= maxPerMinute {
			reason = fmt.Sprintf("max_per_minute=%d already reached in the last 60s", maxPerMinute)
		}
	}
	if reason != "" {
		th.suppressed++
		n := th.suppressed
		if n == 1 || n%100 == 0 {
			logrus.WithField("channel", channel).
				WithField("suppressed_total", n).
				Info("Notification suppressed by channel storm limits: " + reason)
		}
		return noop, fmt.Errorf("%w on %s: %s", ErrNotifySuppressed, channel, reason)
	}
	return func(ok bool) {
		if !ok {
			return
		}
		th.mu.Lock()
		th.window = append(th.window, now)
		th.lastByMsg[key] = now
		th.mu.Unlock()
	}, nil
}

// ChannelConfigured reports whether a channel has usable settings. It is the
// single definition of "configured" that the senders guard with, so endpoints
// can answer "not configured" instead of reporting a send that never happened.
func ChannelConfigured(cfg *config.NotifyConfig, channel string) bool {
	if cfg == nil {
		return false
	}
	switch channel {
	case "dingtalk":
		return cfg.Dingtalk.Enabled && cfg.Dingtalk.WebhookURL != ""
	case "email":
		return cfg.Email.SMTPHost != "" && len(cfg.Email.ToAddrs) > 0
	case "webhook":
		return cfg.Webhook.Enabled && cfg.Webhook.URL != ""
	case "wechat", "wecom":
		return cfg.Wechat.WebhookURL != ""
	}
	return false
}

// SendAlarmNotification sends a structured alarm notification and reports, per
// requested channel, whether it actually delivered.
func (s *NotifyService) SendAlarmNotification(notif *AlarmNotification, channels []string) map[string]bool {
	results := make(map[string]bool)
	if len(channels) == 0 {
		channels = defaultNotifyChannels
	}

	title := fmt.Sprintf("[%s] %s - %s", notif.Severity, notif.RuleName, notif.Action)
	message := fmt.Sprintf("设备: %s\n规则: %s\n严重度: %s\n动作: %s\n消息: %s",
		notif.DeviceName, notif.RuleName, notif.Severity, notif.Action, notif.Message)

	s.mu.Lock()
	cfg, _ := s.getNotifyConfig()
	s.mu.Unlock()

	for _, ch := range channels {
		err := s.dispatchChannel(cfg, ch, title, message, notif.Severity)
		results[ch] = err == nil
	}
	return results
}

// sendDingTalk sends a DingTalk robot notification.
func (s *NotifyService) sendDingTalk(cfg *config.NotifyConfig, title, message, severity string) error {
	if !ChannelConfigured(cfg, "dingtalk") {
		logrus.WithField("channel", "dingtalk").Debug("DingTalk not configured or disabled")
		return notConfigured("dingtalk", " or is disabled")
	}

	payload := map[string]interface{}{
		"msgtype": "markdown",
		"markdown": map[string]interface{}{
			"title": title,
			"text":  fmt.Sprintf("### %s\n\n%s\n\n> 严重度: %s", title, message, severity),
		},
		"at": map[string]interface{}{
			"atMobiles": cfg.Dingtalk.ATMobiles,
			"isAtAll":   cfg.Dingtalk.IsAtAll,
		},
	}

	body, _ := json.Marshal(payload)
	signedURL, err := signDingTalkURL(cfg.Dingtalk.WebhookURL, cfg.Dingtalk.Secret)
	if err != nil {
		return fmt.Errorf("dingtalk: %w", err)
	}
	resp, err := s.httpClient.Post(signedURL, "application/json", bytes.NewReader(body))
	if err != nil {
		logrus.WithField("channel", "dingtalk").WithError(err).Warn("DingTalk notification failed")
		return fmt.Errorf("dingtalk request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		logrus.WithField("channel", "dingtalk").
			WithField("status", resp.StatusCode).
			Warn("DingTalk notification returned non-200")
		return fmt.Errorf("dingtalk endpoint returned status %d", resp.StatusCode)
	}
	logrus.WithField("channel", "dingtalk").Info("DingTalk notification sent")
	return nil
}

// signDingTalkURL appends the timestamp/sign a DingTalk robot with 加签 security
// requires; robots set up that way reject unsigned posts outright.
func signDingTalkURL(webhookURL, secret string) (string, error) {
	if secret == "" {
		return webhookURL, nil
	}
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(timestamp + "\n" + secret)); err != nil {
		return "", err
	}
	sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	separator := "?"
	if strings.Contains(webhookURL, "?") {
		separator = "&"
	}
	return fmt.Sprintf("%s%stimestamp=%s&sign=%s", webhookURL, separator, timestamp, sign), nil
}

// sendWeCom sends a WeCom (Enterprise WeChat) robot notification.
func (s *NotifyService) sendWeCom(cfg *config.NotifyConfig, title, message, severity string) error {
	if !ChannelConfigured(cfg, "wecom") {
		logrus.WithField("channel", "wechat").Debug("WeCom not configured")
		return notConfigured("wecom", " (no webhook URL set)")
	}

	payload := map[string]interface{}{
		"msgtype": "markdown",
		"markdown": map[string]interface{}{
			"content": fmt.Sprintf("### %s\n\n%s\n\n> 严重度: %s", title, message, severity),
		},
	}

	body, _ := json.Marshal(payload)
	resp, err := s.httpClient.Post(cfg.Wechat.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		logrus.WithField("channel", "wechat").WithError(err).Warn("WeCom notification failed")
		return fmt.Errorf("wecom request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		logrus.WithField("channel", "wechat").WithField("status", resp.StatusCode).Warn("WeCom notification returned error status")
		return fmt.Errorf("wecom endpoint returned status %d", resp.StatusCode)
	}
	logrus.WithField("channel", "wechat").Info("WeCom notification sent")
	return nil
}

// sendEmail sends an email notification via SMTP.
func (s *NotifyService) sendEmail(cfg *config.NotifyConfig, title, message, severity string) error {
	if !ChannelConfigured(cfg, "email") {
		logrus.WithField("channel", "email").Debug("Email not configured")
		return notConfigured("email", " (smtp_host or recipients are empty)")
	}

	subject := fmt.Sprintf("=?UTF-8?B?%s?=", base64.StdEncoding.EncodeToString([]byte(title)))
	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		cfg.Email.FromAddr,
		strings.Join(cfg.Email.ToAddrs, ", "),
		subject,
		message,
	)

	auth := smtp.PlainAuth("", cfg.Email.SMTPUser, cfg.Email.SMTPPassword, cfg.Email.SMTPHost)
	if err := sendSMTPMail(&cfg.Email, auth, body); err != nil {
		logrus.WithField("channel", "email").WithError(err).Warn("Email notification failed")
		return fmt.Errorf("email send failed: %w", err)
	}
	logrus.WithField("channel", "email").Info("Email notification sent")
	return nil
}

// sendSMTPMail delivers one message honouring the channel's TLS switches:
// use_ssl wraps the socket in TLS from the start (port 465), otherwise the
// connection is upgraded with STARTTLS when TLS is requested. smtp.SendMail
// supports neither, which is why the session is driven by hand here.
func sendSMTPMail(e *config.NotifyEmailConfig, auth smtp.Auth, msg string) error {
	addr := net.JoinHostPort(e.SMTPHost, strconv.Itoa(e.SMTPPort))
	tlsCfg := &tls.Config{ServerName: e.SMTPHost}

	var conn net.Conn
	var err error
	if e.UseSSL {
		conn, err = tls.Dial("tcp", addr, tlsCfg)
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()

	cli, err := smtp.NewClient(conn, e.SMTPHost)
	if err != nil {
		return err
	}
	defer cli.Quit()

	if !e.UseSSL && (e.UseTLS || e.UseStartTLS) {
		if supported, _ := cli.Extension("STARTTLS"); !supported {
			return fmt.Errorf("SMTP server %s does not support STARTTLS", addr)
		}
		if err := cli.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS with %s failed: %w", addr, err)
		}
	}

	if err := cli.Auth(auth); err != nil {
		return err
	}
	if err := cli.Mail(e.FromAddr); err != nil {
		return err
	}
	for _, to := range e.ToAddrs {
		if err := cli.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := cli.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return err
	}
	return w.Close()
}

// sendWebhook sends a custom webhook notification.
func (s *NotifyService) sendWebhook(cfg *config.NotifyConfig, title, message, severity string) error {
	if !ChannelConfigured(cfg, "webhook") {
		logrus.WithField("channel", "webhook").Debug("Webhook not configured or disabled")
		return notConfigured("webhook", " or is disabled")
	}

	payload := map[string]interface{}{
		"title":     title,
		"message":   message,
		"severity":  severity,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	body, _ := json.Marshal(payload)
	method := strings.ToUpper(cfg.Webhook.Method)
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, cfg.Webhook.URL, bytes.NewReader(body))
	if err != nil {
		logrus.WithField("channel", "webhook").WithError(err).Warn("Webhook request creation failed")
		return fmt.Errorf("webhook request invalid: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Add custom headers
	for k, v := range cfg.Webhook.Headers {
		req.Header.Set(k, v)
	}

	// Add auth header if configured
	switch cfg.Webhook.AuthType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+cfg.Webhook.AuthToken)
	case "basic":
		req.SetBasicAuth(cfg.Webhook.AuthUsername, cfg.Webhook.AuthPassword)
	case "api_key":
		if cfg.Webhook.AuthToken != "" {
			req.Header.Set("X-API-Key", cfg.Webhook.AuthToken)
		}
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		logrus.WithField("channel", "webhook").WithError(err).Warn("Webhook notification failed")
		return fmt.Errorf("webhook request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		logrus.WithField("channel", "webhook").
			WithField("status", resp.StatusCode).
			Warn("Webhook notification returned error status")
		return fmt.Errorf("webhook endpoint returned status %d", resp.StatusCode)
	}
	logrus.WithField("channel", "webhook").Info("Webhook notification sent")
	return nil
}

// getNotifyConfig safely retrieves the notification config.
func (s *NotifyService) getNotifyConfig() (*config.NotifyConfig, bool) {
	if cfg, ok := s.notifyCfg.(*config.NotifyConfig); ok && cfg != nil {
		return cfg, true
	}
	// Fall back to the live process config. Without this a service that was never
	// handed a config pointer, or one whose pointer was orphaned by ReloadConfig
	// replacing the config object, reports every channel as unconfigured and
	// drops alarm notifications in silence.
	return &config.GetConfig().Notify, true
}
