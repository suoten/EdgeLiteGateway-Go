package drivers

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// SimulatorDriver generates simulated data for testing and demo purposes.
// It supports sine wave, random, ramp, and constant generation modes.
type SimulatorDriver struct {
	BaseDriver
	started    time.Time
	rng        *rand.Rand
	mu         sync.Mutex
	cfg        simulatorConfig
	held       map[string]heldValue
	phaseStart map[string]time.Time
	walk       map[string]float64
	// A fault-injected disconnect heals itself at offlineUntil.
	offlineUntil time.Time
}

type heldValue struct {
	value float64
	until time.Time
}

// simulatorConfig mirrors the device-level knobs the UI exposes. Every field here
// has a consumer below; the driver previously read none of them, so a simulator
// device ignored its whole configuration form.
type simulatorConfig struct {
	rangeMin    float64
	rangeMax    float64
	noise       float64
	drift       float64
	defaultMode string
	period      float64 // seconds for one full waveform cycle
	faultMode   string
	faultRate   float64 // percent, 0-100
	faultDelay  time.Duration
	writeHold   time.Duration
	formula     string
}

func newSimulatorConfig(config map[string]interface{}) simulatorConfig {
	period := GetConfigFloat(config, "period", 60)
	if period <= 0 {
		period = 60
	}
	holdSeconds := GetConfigFloat(config, "write_hold_seconds", 10)
	if holdSeconds < 0 {
		holdSeconds = 0
	}
	rate := GetConfigFloat(config, "fault_rate", 0)
	if rate < 0 {
		rate = 0
	}
	if rate > 100 {
		rate = 100
	}
	timeoutSeconds := GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout))
	if timeoutSeconds < 0 {
		timeoutSeconds = 0
	}
	return simulatorConfig{
		rangeMin:    GetConfigFloat(config, "value_range_min", 0),
		rangeMax:    GetConfigFloat(config, "value_range_max", 100),
		noise:       GetConfigFloat(config, "noise_amplitude", 0),
		drift:       GetConfigFloat(config, "trend_drift", 0),
		defaultMode: GetConfigString(config, "sim_mode", "sine"),
		period:      period,
		faultMode:   GetConfigString(config, "fault_simulation", "none"),
		faultRate:   rate,
		faultDelay:  time.Duration(timeoutSeconds * float64(time.Second)),
		writeHold:   time.Duration(holdSeconds * float64(time.Second)),
		formula:     strings.TrimSpace(GetConfigString(config, "formula", "")),
	}
}

// check verifies the formula is parseable, failing before a device is saved
// rather than producing a point stream whose every row is quality "bad".
func (c simulatorConfig) check() error {
	if c.formula != "" {
		if _, err := SafeEvalExpr(c.formula, simulatorFormulaVars(0, 0, 0, c.rangeMin, c.rangeMax, c.period)); err != nil {
			return fmt.Errorf("invalid formula %q: %w", c.formula, err)
		}
	}
	return nil
}

// simulatorFormulaVars exposes the variables a custom formula may reference:
// t (seconds since the driver started), cycle (completed waveform periods,
// fractional), phase (radians), min/max/mid/amp (the configured range) and
// period (seconds per cycle).
func simulatorFormulaVars(t, cycle, phase, minVal, maxVal, period float64) map[string]float64 {
	return map[string]float64{
		"t":      t,
		"cycle":  cycle,
		"phase":  phase,
		"min":    minVal,
		"max":    maxVal,
		"mid":    (maxVal + minVal) / 2,
		"amp":    (maxVal - minVal) / 2,
		"period": period,
	}
}

// NewSimulatorDriver creates a new SimulatorDriver.
func NewSimulatorDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	cfg := newSimulatorConfig(config)
	if err := cfg.check(); err != nil {
		return nil, err
	}
	d := &SimulatorDriver{
		started:    time.Now(),
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
		cfg:        cfg,
		held:       make(map[string]heldValue),
		phaseStart: make(map[string]time.Time),
		walk:       make(map[string]float64),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	d.SetConnected(true) // Simulator is always connected
	return d, nil
}

func (d *SimulatorDriver) Name() string { return "simulator" }

func (d *SimulatorDriver) Connect(ctx context.Context) error {
	cfg := newSimulatorConfig(d.GetConfig())
	if err := cfg.check(); err != nil {
		d.SetConnected(false)
		return err
	}
	d.mu.Lock()
	d.cfg = cfg
	d.offlineUntil = time.Time{}
	d.mu.Unlock()
	d.SetConnected(true)
	return nil
}

func (d *SimulatorDriver) Disconnect() error {
	d.SetConnected(false)
	return nil
}

func (d *SimulatorDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	// The drivers are the only writers of the health counters the device panels
	// read (the collect scheduler keeps its own, separate tally), so a driver that
	// never records leaves /ops showing "no samples" for a device that has been
	// collecting for hours.
	started := time.Now()
	d.mu.Lock()
	if !d.IsConnected() {
		if d.offlineUntil.IsZero() || time.Now().Before(d.offlineUntil) {
			d.mu.Unlock()
			d.RecordReadFailure()
			return nil, fmt.Errorf("simulator not connected")
		}
		d.offlineUntil = time.Time{}
		d.SetConnected(true)
	}
	fault, delay := d.faultDecision()
	d.mu.Unlock()

	if delay > 0 {
		// A simulated slow read blocks like a real one, but yields to the
		// collector's deadline instead of holding the device for its whole timeout.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if fault != "" {
		if fault == "disconnect" {
			d.SetConnected(false)
		}
		d.RecordReadFailure()
		return nil, fmt.Errorf("simulated %s", fault)
	}

	now := time.Now()
	result := make([]storage.PointData, 0, len(points))

	for _, pt := range points {
		quality := "good"
		if d.cfg.faultMode == "data_error" && d.rng.Float64()*100 < d.cfg.faultRate {
			quality = "bad"
		}
		val, err := d.generateValue(pt, now)
		if err != nil {
			// An unusable generator must not be reported as a reading. Returning nil
			// with quality "good" is how a point looked alive while producing nothing.
			result = append(result, storage.PointData{
				DeviceID: d.DeviceID(), PointName: pt.Name,
				Value: nil, Quality: "bad", Timestamp: now,
			})
			logrus.WithField("device_id", d.DeviceID()).
				WithField("point", pt.Name).
				Debug("Simulator point unavailable: " + err.Error())
			continue
		}
		result = append(result, storage.PointData{
			DeviceID:  d.DeviceID(),
			PointName: pt.Name,
			Value:     val,
			Quality:   quality,
			Timestamp: now,
		})
	}

	// A simulated read is pure in-memory work, so on this platform the clock
	// usually reports the same tick at both ends and the sample lands as 0. It is
	// a measurement, not a configured latency the driver ignored.
	d.RecordReadSuccess(ElapsedMs(started))
	return result, nil
}

// faultDecision picks the fault (if any) for this collection cycle and how long
// the read should stall before reporting it. It is called with d.mu held.
func (d *SimulatorDriver) faultDecision() (string, time.Duration) {
	mode := d.cfg.faultMode
	if mode == "" || mode == "none" || d.cfg.faultRate <= 0 {
		return "", 0
	}
	if d.rng.Float64()*100 >= d.cfg.faultRate {
		return "", 0
	}
	if mode == "random" {
		switch d.rng.Intn(3) {
		case 0:
			mode = "timeout"
		case 1:
			mode = "disconnect"
		default:
			mode = "data_error"
		}
	}
	if mode == "data_error" {
		// Degrades individual point qualities, not the whole read cycle.
		return "", 0
	}
	if mode == "disconnect" {
		// Nothing else ever reconnects a simulator, so a forced disconnect would
		// strand the device offline for the rest of the run. Recover on its own.
		d.offlineUntil = time.Now().Add(d.readDelay())
		return mode, 0
	}
	return mode, d.readDelay()
}

// readDelay is how long a simulated slow read stalls; disconnected devices come
// back after the same window.
func (d *SimulatorDriver) readDelay() time.Duration {
	if d.cfg.faultDelay > 0 {
		return d.cfg.faultDelay
	}
	return 5 * time.Second
}

// generateValue produces the raw waveform value for a point. Scale and offset are
// deliberately not applied here: the device service turns raw values into
// engineering units once, for every protocol alike.
func (d *SimulatorDriver) generateValue(pt models.PointDef, now time.Time) (float64, error) {
	// A value the operator wrote stays visible for write_hold_seconds; without this
	// the next poll overwrote it and a successful write could never be observed.
	if h, ok := d.held[pt.Name]; ok {
		if h.until.IsZero() || now.Before(h.until) {
			return h.value, nil
		}
		delete(d.held, pt.Name)
	}

	minVal := d.cfg.rangeMin
	maxVal := d.cfg.rangeMax
	if pt.Min != nil {
		minVal = *pt.Min
	}
	if pt.Max != nil {
		maxVal = *pt.Max
	}
	if maxVal < minVal {
		minVal, maxVal = maxVal, minVal
	}

	mode := pt.Mode
	if mode == "" {
		mode = d.cfg.defaultMode
	}
	if mode == "" {
		mode = "sine"
	}

	start, seen := d.phaseStart[pt.Name]
	if !seen {
		start = now.Add(-time.Duration(pointNameSeed(pt.Name) / (2 * math.Pi) * d.cfg.period * float64(time.Second)))
	}
	d.phaseStart[pt.Name] = start
	// The cycle position is driven by elapsed time, not by how many polls have
	// happened, so the waveform period is the configured one regardless of the
	// device's collect interval.
	cycles := now.Sub(start).Seconds() / d.cfg.period
	phase := 2 * math.Pi * cycles

	amplitude := (maxVal - minVal) / 2
	mid := (maxVal + minVal) / 2

	var value float64
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "sine":
		value = mid + amplitude*math.Sin(phase)
	case "random":
		value = minVal + d.rng.Float64()*(maxVal-minVal)
	case "ramp":
		// Trianglar sweep between the bounds, one full sweep per two cycles.
		t := math.Mod(cycles, 2)
		if t < 1 {
			value = minVal + t*(maxVal-minVal)
		} else {
			value = maxVal - (t-1)*(maxVal-minVal)
		}
	case "triangle":
		t := math.Mod(cycles, 2)
		if t < 1 {
			value = mid + amplitude*(-1+2*t)
		} else {
			value = mid + amplitude*(3-2*t)
		}
	case "sawtooth":
		t := math.Mod(cycles, 1)
		value = minVal + t*(maxVal-minVal)
	case "square":
		if math.Mod(cycles, 1) < 0.5 {
			value = maxVal
		} else {
			value = minVal
		}
	case "step":
		if math.Mod(math.Floor(cycles*2), 2) < 1 {
			value = minVal
		} else {
			value = maxVal
		}
	case "random_walk":
		step := (maxVal - minVal) * 0.02
		w := d.walk[pt.Name]
		if w == 0 {
			w = mid
		}
		w += (d.rng.Float64()*2 - 1) * step
		if w < minVal {
			w = minVal
		}
		if w > maxVal {
			w = maxVal
		}
		d.walk[pt.Name] = w
		value = w
	case "constant", "fixed":
		value = mid
	case "formula":
		if d.cfg.formula == "" {
			return 0, fmt.Errorf("sim_mode is \"formula\" but no formula is configured")
		}
		v, err := SafeEvalExpr(d.cfg.formula,
			simulatorFormulaVars(now.Sub(d.started).Seconds(), cycles, phase, minVal, maxVal, d.cfg.period))
		if err != nil {
			return 0, fmt.Errorf("ERR_SIM_FORMULA_EVAL_FAILED: %s", err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("ERR_SIM_FORMULA_EVAL_FAILED: formula %q produced %v", d.cfg.formula, v)
		}
		value = v
	default:
		return 0, fmt.Errorf("unknown sim_mode %q", mode)
	}

	if d.cfg.noise > 0 {
		value += (d.rng.Float64()*2 - 1) * d.cfg.noise
	}
	if d.cfg.drift != 0 {
		value += d.cfg.drift * now.Sub(start).Seconds() / 60
	}
	return value, nil
}

// pointNameSeed derives a deterministic initial phase in [0, 2π) from a point name.
func pointNameSeed(name string) float64 {
	var sum uint32
	for i := 0; i < len(name); i++ {
		sum = sum*31 + uint32(name[i])
	}
	return float64(sum%628) / 100.0
}

func (d *SimulatorDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	f, ok := toFloat64Value(value)
	if !ok {
		// Reporting success for a value the simulator cannot store made the write
		// look applied when nothing was held.
		d.RecordWriteFailure()
		return fmt.Errorf("simulator cannot store %T value for point %s", value, point)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	hold := d.cfg.writeHold
	var until time.Time
	if hold > 0 {
		until = time.Now().Add(hold)
	}
	d.held[point] = heldValue{value: f, until: until}
	d.RecordWriteSuccess()
	return nil
}

func (d *SimulatorDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *SimulatorDriver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("simulator disconnected")
	}
	return nil
}
