package drivers

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"go.bug.st/serial"

	"edgelite/internal/constants"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// ModbusTCPDriver implements a Modbus TCP client driver.
// It supports reading/writing holding registers, input registers, coils, and discrete inputs.
type ModbusTCPDriver struct {
	BaseDriver
	host      string
	port      int
	slaveID   int
	byteOrder string
	timeout   time.Duration
	conn      net.Conn
	mu        sync.Mutex
}

// NewModbusTCPDriver creates a new ModbusTCPDriver.
func NewModbusTCPDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	byteOrder, err := normalizeModbusByteOrder(GetConfigString(config, "byte_order", ""))
	if err != nil {
		return nil, err
	}
	d := &ModbusTCPDriver{
		host:      GetConfigString(config, "host", "127.0.0.1"),
		port:      GetConfigInt(config, "port", 502),
		slaveID:   GetConfigInt(config, "slave_id", 1),
		byteOrder: byteOrder,
		timeout:   time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *ModbusTCPDriver) Name() string { return "modbus_tcp" }

func (d *ModbusTCPDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn != nil {
		return nil // Already connected
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.SetConnectionState(StateConnecting, "modbus tcp connecting")

	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	dialer := net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("modbus tcp connect %s: %w", addr, err)
	}
	d.conn = conn
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "modbus tcp connected")
	logrus.WithField("device_id", d.DeviceID()).
		WithField("addr", addr).
		Debug("Modbus TCP connected")
	return nil
}

func (d *ModbusTCPDriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		err := d.conn.Close()
		d.conn = nil
		d.SetConnected(false)
		return err
	}
	return nil
}

func (d *ModbusTCPDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("modbus not connected")
	}

	// Check circuit breaker before read
	if d.IsCircuitOpen() {
		return nil, fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	now := time.Now()
	result := make([]storage.PointData, 0, len(points))
	anyFailed := false
	var lastLatencyMs float64

	for _, pt := range points {
		start := time.Now()
		val, err := d.readPoint(pt)
		lastLatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
		if err != nil {
			anyFailed = true
			logrus.WithField("device_id", d.DeviceID()).
				WithField("point", pt.Name).
				WithField("address", pt.Address).
				WithField("error", err.Error()).
				Warn("Modbus read failed")
			result = append(result, storage.PointData{
				DeviceID:  d.DeviceID(),
				PointName: pt.Name,
				Value:     nil,
				Quality:   "bad",
				Timestamp: now,
			})
			continue
		}
		result = append(result, storage.PointData{
			DeviceID:  d.DeviceID(),
			PointName: pt.Name,
			Value:     val,
			Quality:   "good",
			Timestamp: now,
		})
	}

	if anyFailed {
		d.RecordReadFailure()
	} else {
		d.RecordReadSuccess(lastLatencyMs)
	}

	return result, nil
}

// readPoint reads a single Modbus point.
func (d *ModbusTCPDriver) readPoint(pt models.PointDef) (interface{}, error) {
	regType, addr, qty, err := parseModbusAddress(pt.Address)
	if err != nil {
		return nil, err
	}
	// Bit areas are single-bit reads; register areas span as many 16-bit
	// registers as the data type requires (unless the address set an explicit
	// ".N" count).
	if regType == "coil" || regType == "discrete" {
		qty = 1
	} else if qty <= 0 {
		qty = modbusRegCount(pt.DataType)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	switch regType {
	case "holding":
		regs, err := d.readHoldingRegisters(addr, qty)
		if err != nil {
			return nil, err
		}
		return decodeModbusValue(reorderRegisters(regs, d.byteOrder), pt.DataType)
	case "input":
		regs, err := d.readInputRegisters(addr, qty)
		if err != nil {
			return nil, err
		}
		return decodeModbusValue(reorderRegisters(regs, d.byteOrder), pt.DataType)
	case "coil":
		coils, err := d.readCoils(addr, qty)
		if err != nil {
			return nil, err
		}
		if len(coils) > 0 {
			return coils[0], nil
		}
		return false, nil
	case "discrete":
		coils, err := d.readDiscreteInputs(addr, qty)
		if err != nil {
			return nil, err
		}
		if len(coils) > 0 {
			return coils[0], nil
		}
		return false, nil
	default:
		return nil, fmt.Errorf("unknown register type: %s", regType)
	}
}

// modbusReadRequest sends a Modbus TCP request and reads the response.
func (d *ModbusTCPDriver) sendRequest(functionCode uint8, data []byte) ([]byte, error) {
	// Build MBAP header (7 bytes) + PDU.
	// Per the Modbus spec the Length field counts the Unit ID + PDU bytes.
	transactionID := uint16(time.Now().UnixNano() & 0xFFFF)
	pdu := append([]byte{functionCode}, data...)
	pduLength := len(pdu)
	mbap := make([]byte, 7+pduLength)
	binary.BigEndian.PutUint16(mbap[0:2], transactionID)       // Transaction ID
	binary.BigEndian.PutUint16(mbap[2:4], 0)                   // Protocol ID
	binary.BigEndian.PutUint16(mbap[4:6], uint16(pduLength+1)) // Length: unit ID + PDU
	mbap[6] = byte(d.slaveID)                                  // Unit ID
	copy(mbap[7:], pdu)

	d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(mbap); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("write: %w", err)
	}

	// Read response header (minimum 7 bytes for exception)
	respHeader := make([]byte, 7)
	if _, err := readFull(d.conn, respHeader); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("read header: %w", err)
	}

	respLen := binary.BigEndian.Uint16(respHeader[4:6])
	if respLen < 2 {
		return nil, fmt.Errorf("invalid response length: %d", respLen)
	}

	respPDU := make([]byte, respLen-1) // Subtract unit ID
	if _, err := readFull(d.conn, respPDU); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("read pdu: %w", err)
	}

	// Check for exception response
	if respPDU[0]&0x80 != 0 {
		exCode := byte(0)
		if len(respPDU) > 1 {
			exCode = respPDU[1]
		}
		return nil, fmt.Errorf("modbus exception code: %d", exCode)
	}

	return respPDU, nil
}

// readHoldingRegisters reads holding registers.
func (d *ModbusTCPDriver) readHoldingRegisters(addr, qty int) ([]uint16, error) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(qty))

	resp, err := d.sendRequest(0x03, data)
	if err != nil {
		return nil, err
	}
	if len(resp) < 2 {
		return nil, fmt.Errorf("short response")
	}
	byteCount := int(resp[1])
	if len(resp) < 2+byteCount {
		return nil, fmt.Errorf("response too short")
	}
	regs := make([]uint16, byteCount/2)
	for i := 0; i < len(regs); i++ {
		regs[i] = binary.BigEndian.Uint16(resp[2+i*2:])
	}
	return regs, nil
}

// readInputRegisters reads input registers.
func (d *ModbusTCPDriver) readInputRegisters(addr, qty int) ([]uint16, error) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(qty))

	resp, err := d.sendRequest(0x04, data)
	if err != nil {
		return nil, err
	}
	if len(resp) < 2 {
		return nil, fmt.Errorf("short response")
	}
	byteCount := int(resp[1])
	if len(resp) < 2+byteCount {
		return nil, fmt.Errorf("response too short")
	}
	regs := make([]uint16, byteCount/2)
	for i := 0; i < len(regs); i++ {
		regs[i] = binary.BigEndian.Uint16(resp[2+i*2:])
	}
	return regs, nil
}

// readCoils reads coils.
func (d *ModbusTCPDriver) readCoils(addr, qty int) ([]bool, error) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(qty))

	resp, err := d.sendRequest(0x01, data)
	if err != nil {
		return nil, err
	}
	if len(resp) < 2 {
		return nil, fmt.Errorf("short response")
	}
	byteCount := int(resp[1])
	if len(resp) < 2+byteCount {
		return nil, fmt.Errorf("response too short")
	}
	coils := make([]bool, qty)
	for i := 0; i < qty && i < byteCount*8; i++ {
		if resp[2+i/8]&(1<<uint(i%8)) != 0 {
			coils[i] = true
		}
	}
	return coils, nil
}

// readDiscreteInputs reads discrete inputs.
func (d *ModbusTCPDriver) readDiscreteInputs(addr, qty int) ([]bool, error) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(qty))

	resp, err := d.sendRequest(0x02, data)
	if err != nil {
		return nil, err
	}
	if len(resp) < 2 {
		return nil, fmt.Errorf("short response")
	}
	byteCount := int(resp[1])
	if len(resp) < 2+byteCount {
		return nil, fmt.Errorf("response too short")
	}
	coils := make([]bool, qty)
	for i := 0; i < qty && i < byteCount*8; i++ {
		if resp[2+i/8]&(1<<uint(i%8)) != 0 {
			coils[i] = true
		}
	}
	return coils, nil
}

func (d *ModbusTCPDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	return d.writePoint(ctx, point, value, "")
}

// WritePointTyped writes with the point's declared data type, which decides how a
// value spanning several registers is laid out in them.
func (d *ModbusTCPDriver) WritePointTyped(ctx context.Context, point string, value interface{}, dataType string) error {
	return d.writePoint(ctx, point, value, dataType)
}

// WritePointAtAddress writes to the point's register address, which is what this
// driver parses; the operator's point name carries no register information.
func (d *ModbusTCPDriver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	return d.writePoint(ctx, address, value, dataType)
}

func (d *ModbusTCPDriver) writePoint(ctx context.Context, point string, value interface{}, dataType string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		return fmt.Errorf("modbus not connected")
	}

	// Check circuit breaker before write
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	// Parse the point address
	// The point name is expected to be in the same format as ReadPoints addresses
	regType, addr, qty, err := parseModbusAddress(point)
	if err != nil {
		return fmt.Errorf("invalid point address: %w", err)
	}
	// A bare address writes one 16-bit register unless the point's data type needs
	// more, mirroring what the read path does. Without that a float32 point would
	// be written as one register and read back as two.
	qty = modbusWriteQty(qty, dataType)

	switch regType {
	case "holding":
		// Write single or multiple holding registers
		if qty == 1 {
			val, err := toUint16ForType(value, dataType)
			if err != nil {
				return err
			}
			if err := d.writeSingleRegister(addr, val); err != nil {
				d.RecordWriteFailure()
				return err
			}
		} else {
			val, err := modbusEncodeRegisters(value, dataType, qty)
			if err != nil {
				return err
			}
			if err := d.writeMultipleRegisters(addr, reorderRegisters(val, d.byteOrder)); err != nil {
				d.RecordWriteFailure()
				return err
			}
		}
	case "coil":
		val, ok := value.(bool)
		if !ok {
			// Try numeric conversion
			n, ok2 := toUint16(value)
			if !ok2 {
				return fmt.Errorf("cannot convert value to bool: %v", value)
			}
			val = n != 0
		}
		if err := d.writeSingleCoil(addr, val); err != nil {
			d.RecordWriteFailure()
			return err
		}
	default:
		return fmt.Errorf("write not supported for register type: %s", regType)
	}

	d.RecordWriteSuccess()
	logrus.WithField("device_id", d.DeviceID()).
		WithField("point", point).
		WithField("value", value).
		Debug("Modbus write completed")
	return nil
}

// writeSingleRegister writes a single holding register (function code 0x06).
func (d *ModbusTCPDriver) writeSingleRegister(addr int, value uint16) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], value)
	_, err := d.sendRequest(0x06, data)
	return err
}

// writeMultipleRegisters writes multiple holding registers (function code 0x10).
func (d *ModbusTCPDriver) writeMultipleRegisters(addr int, values []uint16) error {
	byteCount := len(values) * 2
	data := make([]byte, 5+byteCount)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(len(values)))
	data[4] = byte(byteCount)
	for i, v := range values {
		binary.BigEndian.PutUint16(data[5+i*2:], v)
	}
	_, err := d.sendRequest(0x10, data)
	return err
}

// writeSingleCoil writes a single coil (function code 0x05).
func (d *ModbusTCPDriver) writeSingleCoil(addr int, value bool) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	if value {
		binary.BigEndian.PutUint16(data[2:4], 0xFF00)
	} else {
		binary.BigEndian.PutUint16(data[2:4], 0x0000)
	}
	_, err := d.sendRequest(0x05, data)
	return err
}

// toUint16 converts an interface{} value to uint16.
func toUint16(v interface{}) (uint16, bool) {
	switch n := v.(type) {
	case uint16:
		return n, true
	case int:
		return uint16(n), true
	case int16:
		return uint16(n), true
	case int32:
		return uint16(n), true
	case int64:
		return uint16(n), true
	case uint32:
		return uint16(n), true
	case float32:
		return uint16(n), true
	case float64:
		return uint16(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// toUint16ForType converts a value to one holding register, refusing a number
// outside the declared type's range. toUint16 alone wraps instead of checking,
// so writing 70000 to a uint16 point stored 4464 and the API answered success -
// a silently wrong value in a PLC is worse than a refused write.
func toUint16ForType(v interface{}, dataType string) (uint16, error) {
	if err := checkWriteInRange(v, dataType); err != nil {
		return 0, err
	}
	val, ok := toUint16(v)
	if !ok {
		return 0, fmt.Errorf("cannot convert value to uint16: %v", v)
	}
	return val, nil
}

// toUint16Slice converts an interface{} to a slice of uint16 with expected length.
func toUint16Slice(v interface{}, count int) ([]uint16, error) {
	result := make([]uint16, 0, count)
	switch val := v.(type) {
	case []uint16:
		result = append(result, val...)
	case []byte:
		for i := 0; i+1 < len(val) && i/2 < count; i += 2 {
			result = append(result, binary.BigEndian.Uint16(val[i:]))
		}
	case uint16:
		result = append(result, val)
	case int:
		result = append(result, uint16(val))
	case int32:
		result = append(result, uint16(val))
	case int64:
		result = append(result, uint16(val))
	case float64:
		result = append(result, uint16(val))
	default:
		// Try single conversion
		u, ok := toUint16(v)
		if !ok {
			return nil, fmt.Errorf("cannot convert %T to uint16 slice", v)
		}
		result = append(result, u)
	}
	// Pad with zeros if not enough values
	for len(result) < count {
		result = append(result, 0)
	}
	return result, nil
}

// The discovery budget. Sweeping the 247 addressable unit IDs of one port is
// what the Discover button on a device form means in practice, so it has to
// finish in seconds: a refused port answers immediately and only a host that
// accepts the connection and stays silent costs the answer timeout.
const (
	modbusProbeDialTimeout   = 400 * time.Millisecond
	modbusProbeAnswerTimeout = 700 * time.Millisecond
	modbusProbeWorkers       = 32
	modbusMaxUnitID          = 247
)

// modbusTarget is one (address, unit ID) pair to ask.
type modbusTarget struct {
	ip   string
	port int
	unit int
}

// Discover probes addresses for Modbus TCP slaves.
//
// A port that merely accepts a TCP connection is not a device, so the probe sends
// a real request and requires a Modbus frame in reply (see probeModbusUnit). With
// a single host it asks the configured unit ID (config.slave_id); set
// config.scan_units to sweep 1..N of that port, which is what a Modbus TCP port
// fronting a serial bus needs. config.scan_range instead walks the addresses above
// the configured host within one /24, probing config.slave_id on each.
func (d *ModbusTCPDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	host := GetConfigString(config, "host", "127.0.0.1")
	port := GetConfigInt(config, "port", 502)
	scanRange := GetConfigInt(config, "scan_range", 0)

	hosts := []string{host}
	units := modbusUnitsToProbe(config)
	if scanRange > 0 {
		expanded, err := expandModbusHostRange(host, scanRange)
		if err != nil {
			return nil, err
		}
		hosts = expanded
		units = []int{GetConfigInt(config, "slave_id", 1)}
	}

	jobs := make(chan modbusTarget, len(hosts)*len(units))
	for _, ip := range hosts {
		for _, unit := range units {
			jobs <- modbusTarget{ip: ip, port: port, unit: unit}
		}
	}
	close(jobs)

	type hit struct {
		ip   string
		unit int
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	hits := make(chan hit, cap(jobs))
	var wg sync.WaitGroup
	for i := 0; i < modbusProbeWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range jobs {
				if probeCtx.Err() != nil {
					return
				}
				if probeModbusUnit(probeCtx, target) {
					hits <- hit{ip: target.ip, unit: target.unit}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(hits)
	}()

	devices := make([]map[string]interface{}, 0, cap(hits))
	for h := range hits {
		devices = append(devices, map[string]interface{}{
			"ip":        h.ip,
			"port":      port,
			"protocol":  "modbus_tcp",
			"unit_id":   h.unit,
			"slave_id":  h.unit,
			"device_id": fmt.Sprintf("modbus-%s-%d-%d", h.ip, port, h.unit),
			"name":      fmt.Sprintf("%s:%d unit %d", h.ip, port, h.unit),
		})
	}
	sort.Slice(devices, func(i, j int) bool {
		a, b := devices[i], devices[j]
		if a["ip"] != b["ip"] {
			return a["ip"].(string) < b["ip"].(string)
		}
		return a["unit_id"].(int) < b["unit_id"].(int)
	})
	return devices, nil
}

// modbusUnitsToProbe turns config.scan_units into the unit IDs to ask. Sweeping is
// opt-in: asking all 247 addresses means 247 connections at a device that is often
// a single serial port, and a bridge which answers every unit ID it is asked about
// would then be listed 247 times.
func modbusUnitsToProbe(config map[string]interface{}) []int {
	count := GetConfigInt(config, "scan_units", 0)
	if count <= 0 {
		return []int{GetConfigInt(config, "slave_id", 1)}
	}
	if count > modbusMaxUnitID {
		count = modbusMaxUnitID
	}
	units := make([]int, 0, count)
	for unit := 1; unit <= count; unit++ {
		units = append(units, unit)
	}
	return units
}

// expandModbusHostRange walks the last octet of a dotted-quad host upward,
// clamped to the end of the /24, so a wide scan_range cannot spill into the next
// subnet the operator did not name.
func expandModbusHostRange(host string, count int) ([]string, error) {
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("scan_range needs an IPv4 dotted-quad host, got %q", host)
	}
	if !strings.Contains(host, ".") {
		return nil, fmt.Errorf("scan_range needs an IPv4 dotted-quad host, got %q", host)
	}
	octets := strings.Split(ip.To4().String(), ".")
	last, err := strconv.Atoi(octets[3])
	if err != nil {
		return nil, fmt.Errorf("scan_range cannot read the last octet of %q: %w", host, err)
	}
	base := strings.Join(octets[:3], ".")
	if count > 255-last {
		count = 255 - last
	}
	hosts := make([]string, 0, count+1)
	for i := 0; i <= count; i++ {
		hosts = append(hosts, fmt.Sprintf("%s.%d", base, last+i))
	}
	return hosts, nil
}

func (d *ModbusTCPDriver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("not connected")
	}
	// Read a single holding register as a health check
	start := time.Now()
	_, err := d.readHoldingRegisters(0, 1)
	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		d.RecordReadFailure()
		return err
	}
	d.RecordReadSuccess(latencyMs)
	return nil
}

// --- Helper functions ---

// parseModbusAddress parses a Modbus address string into a register type, a
// zero-based PDU offset, and an optional explicit register count.
//
// The convention is aligned with the ProtoForge simulator and with real PLCs,
// so that devices auto-pushed from ProtoForge collect from the same register
// they were written to. Accepted forms:
//
//	"HR100" / "4x100"  -> holding,  offset 100
//	"IR100" / "3x100"  -> input,    offset 100
//	"C100"  / "0x100"  -> coil,     offset 100
//	"DI100" / "1x100"  -> discrete, offset 100
//	"400100"           -> holding,  offset 100 (6-digit PLC notation)
//	"300100"/"000100"/"100100" -> input / coil / discrete, offset 100
//	"40001"            -> holding,  offset 0   (5-digit PLC notation)
//	"100"              -> holding,  offset 100 (bare number = absolute 0-based offset)
//
// An optional ".N" suffix forces the register count (e.g. "HR200.2"); when
// absent, qty==0 signals "infer the count from the point's data type".
func parseModbusAddress(addr string) (regType string, address int, qty int, err error) {
	raw := strings.TrimSpace(addr)
	if raw == "" {
		return "", 0, 0, fmt.Errorf("empty address")
	}

	// Optional explicit register-count suffix ".N" (EdgeLite extension).
	if dot := strings.Index(raw, "."); dot >= 0 {
		if n, cerr := strconv.Atoi(strings.TrimSpace(raw[dot+1:])); cerr == nil && n > 0 {
			qty = n
		}
		raw = strings.TrimSpace(raw[:dot])
	}

	upper := strings.ToUpper(raw)
	if upper == "" {
		return "", 0, 0, fmt.Errorf("invalid address: %s", addr)
	}

	if isAllDigits(upper) {
		// 5-digit PLC notation carries an implicit area prefix + 1-based origin.
		if len(upper) == 5 {
			n, _ := strconv.Atoi(upper)
			switch {
			case n >= 40001 && n <= 49999:
				return "holding", n - 40001, qty, nil
			case n >= 30001 && n <= 39999:
				return "input", n - 30001, qty, nil
			case n >= 10001 && n <= 19999:
				return "discrete", n - 10001, qty, nil
			case n >= 1 && n <= 9999:
				return "coil", n - 1, qty, nil
			}
		}
		// 6+ digit PLC notation: leading digit selects area.
		if len(upper) >= 6 {
			n, _ := strconv.Atoi(upper[1:])
			switch upper[0] {
			case '4':
				return "holding", n - 1, qty, nil
			case '3':
				return "input", n - 1, qty, nil
			case '0':
				return "coil", n - 1, qty, nil
			case '1':
				return "discrete", n - 1, qty, nil
			}
		}
		// Bare number: absolute 0-based offset into the holding area.
		n, cerr := strconv.Atoi(upper)
		if cerr != nil {
			return "", 0, 0, fmt.Errorf("invalid address: %s", addr)
		}
		return "holding", n, qty, nil
	}

	// Prefixed forms (longest prefixes matched first to avoid "DI" vs "D" clashes).
	var area string
	var numStr string
	switch {
	case strings.HasPrefix(upper, "HR"):
		area, numStr = "holding", upper[2:]
	case strings.HasPrefix(upper, "4X"):
		area, numStr = "holding", upper[2:]
	case strings.HasPrefix(upper, "IR"):
		area, numStr = "input", upper[2:]
	case strings.HasPrefix(upper, "3X"):
		area, numStr = "input", upper[2:]
	case strings.HasPrefix(upper, "DI"):
		area, numStr = "discrete", upper[2:]
	case strings.HasPrefix(upper, "1X"):
		area, numStr = "discrete", upper[2:]
	case strings.HasPrefix(upper, "COIL"):
		area, numStr = "coil", upper[4:]
	case strings.HasPrefix(upper, "0X"):
		area, numStr = "coil", upper[2:]
	case strings.HasPrefix(upper, "C"):
		area, numStr = "coil", upper[1:]
	default:
		return "", 0, 0, fmt.Errorf("invalid address: %s", addr)
	}
	n, cerr := strconv.Atoi(strings.TrimSpace(numStr))
	if cerr != nil {
		return "", 0, 0, fmt.Errorf("invalid address: %s", addr)
	}
	return area, n, qty, nil
}

// isAllDigits reports whether s consists solely of ASCII digits (empty => false).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// modbusRegCount returns the number of 16-bit registers a data type spans.
func modbusRegCount(dataType string) int {
	switch strings.ToLower(dataType) {
	case "int32", "uint32", "float32", "float", "long", "dword":
		return 2
	case "float64", "double":
		return 4
	default:
		return 1
	}
}

// decodeModbusValue converts raw register values to the specified data type.
func decodeModbusValue(regs []uint16, dataType string) (interface{}, error) {
	switch strings.ToLower(dataType) {
	case "int16", "short":
		if len(regs) < 1 {
			return nil, fmt.Errorf("no data")
		}
		return int16(regs[0]), nil
	case "uint16", "word":
		if len(regs) < 1 {
			return nil, fmt.Errorf("no data")
		}
		return regs[0], nil
	case "int32", "long":
		if len(regs) < 2 {
			return nil, fmt.Errorf("need 2 registers")
		}
		return int32(binary.BigEndian.Uint32(uint16ToBytes(regs[0], regs[1]))), nil
	case "uint32", "dword":
		if len(regs) < 2 {
			return nil, fmt.Errorf("need 2 registers")
		}
		return binary.BigEndian.Uint32(uint16ToBytes(regs[0], regs[1])), nil
	case "float32", "float":
		if len(regs) < 2 {
			return nil, fmt.Errorf("need 2 registers")
		}
		bits := binary.BigEndian.Uint32(uint16ToBytes(regs[0], regs[1]))
		return float32FromBits(bits), nil
	case "float64", "double":
		if len(regs) < 4 {
			return nil, fmt.Errorf("need 4 registers")
		}
		bits := binary.BigEndian.Uint64(uint16ToBytes(regs[0], regs[1], regs[2], regs[3]))
		return float64FromBits(bits), nil
	case "bool":
		if len(regs) < 1 {
			return nil, fmt.Errorf("no data")
		}
		return regs[0] != 0, nil
	default:
		// Default: uint16
		if len(regs) < 1 {
			return nil, fmt.Errorf("no data")
		}
		return regs[0], nil
	}
}

func uint16ToBytes(vals ...uint16) []byte {
	b := make([]byte, len(vals)*2)
	for i, v := range vals {
		binary.BigEndian.PutUint16(b[i*2:], v)
	}
	return b
}

// Modbus word/byte orders for values that span more than one register. The names
// describe the byte sequence of a 32-bit value, where A is the most significant
// byte: ABCD is the protocol default.
const (
	modbusOrderABCD = "ABCD"
	modbusOrderCDAB = "CDAB"
	modbusOrderBADC = "BADC"
	modbusOrderDCBA = "DCBA"
)

// normalizeModbusByteOrder maps the spellings device configs use onto the four
// register layouts this driver supports. An unknown value is an error rather than
// a silent fallback to ABCD, because a wrong-but-accepted order shows up as
// plausible-looking float32 values that are simply not the ones the PLC holds.
func normalizeModbusByteOrder(raw string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", "ABCD", "BIG", "BIG_ENDIAN", "BIGENDIAN", "MSB":
		return modbusOrderABCD, nil
	case "DCBA", "LITTLE", "LITTLE_ENDIAN", "LITTLEENDIAN", "LSB":
		return modbusOrderDCBA, nil
	case "CDAB", "WORD_SWAP", "WORDSWAP":
		return modbusOrderCDAB, nil
	case "BADC", "BYTE_SWAP", "BYTESWAP":
		return modbusOrderBADC, nil
	}
	return "", fmt.Errorf("invalid byte_order %q: expected ABCD, CDAB, BADC or DCBA", raw)
}

// reorderRegisters returns the registers such that decoding them big-endian
// yields the value the device laid out in the given order. Each of the four
// layouts is its own inverse, so the same call encodes a write.
//
// The names describe one 32-bit value and are applied per group, so a 64-bit
// point gets the same treatment twice rather than a fifth, undocumented layout:
// CDAB swaps the two registers of each 32-bit pair, BADC swaps the bytes inside
// each register, DCBA reverses the whole value.
//
// It is applied only to values that really span several registers: Modbus fixes
// the byte order inside a single 16-bit register, so swapping there would corrupt
// every 16-bit point of a device whose 32-bit points merely need a word swap.
func reorderRegisters(regs []uint16, order string) []uint16 {
	if order == modbusOrderABCD || len(regs) < 2 {
		return regs
	}
	raw := uint16ToBytes(regs...)
	out := make([]byte, len(raw))
	switch order {
	case modbusOrderCDAB:
		copy(out, raw)
		for i := 0; i+3 < len(raw); i += 4 {
			out[i], out[i+1] = raw[i+2], raw[i+3]
			out[i+2], out[i+3] = raw[i], raw[i+1]
		}
	case modbusOrderBADC:
		for i := 0; i < len(regs); i++ {
			out[i*2], out[i*2+1] = raw[i*2+1], raw[i*2]
		}
	case modbusOrderDCBA:
		for i := range raw {
			out[i] = raw[len(raw)-1-i]
		}
	default:
		return regs
	}
	result := make([]uint16, len(regs))
	for i := range result {
		result[i] = binary.BigEndian.Uint16(out[i*2:])
	}
	return result
}

// modbusEncodeRegisters renders value into the registers a point of the given
// data type occupies. The data type has to come from the point definition: a
// 32-bit register pair can hold an int32 or a float32, and the JSON number the
// API hands over cannot tell them apart. The previous behaviour truncated every
// multi-register value to the low 16 bits, so writing a float32 setpoint stored a
// different number than the caller asked for.
func modbusEncodeRegisters(v interface{}, dataType string, count int) ([]uint16, error) {
	if count < 1 {
		count = 1
	}
	needed := modbusRegCount(dataType)
	if needed > 1 && needed != count {
		return nil, fmt.Errorf("data type %q spans %d registers but the address writes %d", dataType, needed, count)
	}

	f, isFloat := toFloat64Value(v)
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "float32", "float":
		if !isFloat {
			return nil, fmt.Errorf("cannot encode %T as float32", v)
		}
		return registersFromBits(uint64(math.Float32bits(float32(f))), 2), nil
	case "float64", "double":
		if !isFloat {
			return nil, fmt.Errorf("cannot encode %T as float64", v)
		}
		return registersFromBits(math.Float64bits(f), 4), nil
	case "int32", "long":
		i, ok := toInt64Value(v)
		if !ok {
			return nil, fmt.Errorf("cannot encode %T as int32", v)
		}
		if i < math.MinInt32 || i > math.MaxInt32 {
			return nil, fmt.Errorf("value %d out of range for int32", i)
		}
		return registersFromBits(uint64(int32(i)), 2), nil
	case "uint32", "dword":
		i, ok := toInt64Value(v)
		if !ok || i < 0 || i > math.MaxUint32 {
			return nil, fmt.Errorf("value %v out of range for uint32", v)
		}
		return registersFromBits(uint64(uint32(i)), 2), nil
	case "bool":
		if b, ok := v.(bool); ok {
			if b {
				return []uint16{1}, nil
			}
			return []uint16{0}, nil
		}
	}
	// 16-bit and undeclared types keep the existing conversion.
	return toUint16Slice(v, count)
}

// registersFromBits splits a bit pattern into count big-endian registers, most
// significant register first.
func registersFromBits(bits uint64, count int) []uint16 {
	regs := make([]uint16, count)
	for i := 0; i < count; i++ {
		shift := uint((count - 1 - i) * 16)
		regs[i] = uint16(bits >> shift)
	}
	return regs
}

func toFloat64Value(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
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

func toInt64Value(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	case float32:
		f := float64(n)
		return int64(f), f == math.Trunc(f)
	case float64:
		return int64(n), n == math.Trunc(n)
	}
	return 0, false
}

// modbusWriteQty returns the register count a write should use. A bare address
// (no ".N" suffix) writes as many registers as the point's data type needs, which
// is what the read path already does; without this a float32 point would be
// written as one register and read back as two.
func modbusWriteQty(qty int, dataType string) int {
	if qty > 1 {
		return qty
	}
	if n := modbusRegCount(dataType); n > 1 {
		return n
	}
	return 1
}

func float32FromBits(bits uint32) float32 {
	return math.Float32frombits(bits)
}

func float64FromBits(bits uint64) float64 {
	return math.Float64frombits(bits)
}

// readFull reads exactly len(buf) bytes from the connection. It takes an
// io.Reader so the RTU frame code can run over a serial port as well as a socket.
func readFull(conn io.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		if n > 0 {
			total += n
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// probeModbusUnit asks one (address, unit ID) pair for protocol-level proof that a
// Modbus TCP slave answers behind it.
//
// A TCP connect proves nothing - a web server accepts connections too - so the
// probe asks for the device identification (function 0x2B, MEI 0x0E, vendor name),
// which the Modbus TCP profile requires a gateway to support and which does not
// depend on the device owning any particular address. When that function is
// refused, as it commonly is on serial bridges, the probe falls back to reading
// coil 0 and accepts only a normal response. An exception reply is therefore not
// a hit: it means either "no such unit" or "no such address", and a scanner
// cannot tell those apart, so it stays quiet instead of listing a device the
// operator then has to delete.
func probeModbusUnit(ctx context.Context, target modbusTarget) bool {
	addr := net.JoinHostPort(target.ip, strconv.Itoa(target.port))
	dialer := net.Dialer{Timeout: modbusProbeDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer conn.Close()

	if fc, ok := modbusProbeExchange(conn, target.unit, modbusDeviceIDProbe); ok {
		switch fc {
		case 0x2B: // echoed function code: a slave identified itself
			return true
		case 0xAB: // same function refused: fall back to the register probe
		default:
			return false
		}
	}
	fc, ok := modbusProbeExchange(conn, target.unit, modbusCoilsProbe)
	return ok && fc == 0x01
}

// modbusDeviceIDProbe is the PDU of a Read Device Information request: MEI 0x0E,
// read device ID, object 0x01 (Vendor Name).
var modbusDeviceIDProbe = []byte{0x2B, 0x0E, 0x01, 0x01}

// modbusCoilsProbe is the PDU of a read-coils request for the first coil.
var modbusCoilsProbe = []byte{0x01, 0x00, 0x00, 0x00, 0x01}

// modbusProbeExchange sends one PDU over an already connected socket and reports
// the replied function code. It returns false when nothing well-formed came back:
// a non-zero protocol identifier, a length that cannot describe the frame, or a
// unit ID other than the one asked for all mean the peer is not this device's
// Modbus TCP slave.
//
// The whole response is consumed even though only the function code is inspected:
// the caller may send the next probe on this same connection, and leftover payload
// bytes would misalign its reply.
func modbusProbeExchange(conn net.Conn, unit int, pdu []byte) (byte, bool) {
	frame := make([]byte, 0, 9+len(pdu))
	frame = append(frame, 0x00, 0x01, 0x00, 0x00,
		byte((len(pdu)+1)>>8), byte(len(pdu)+1), byte(unit))
	frame = append(frame, pdu...)

	if err := conn.SetWriteDeadline(time.Now().Add(modbusProbeAnswerTimeout)); err != nil {
		return 0, false
	}
	if _, err := conn.Write(frame); err != nil {
		return 0, false
	}

	// The MBAP header is 7 bytes and the function code is the 8th.
	reply := make([]byte, 8)
	if err := conn.SetReadDeadline(time.Now().Add(modbusProbeAnswerTimeout)); err != nil {
		return 0, false
	}
	if _, err := io.ReadFull(conn, reply); err != nil {
		return 0, false
	}
	if binary.BigEndian.Uint16(reply[2:4]) != 0 {
		return 0, false
	}
	length := binary.BigEndian.Uint16(reply[4:6])
	if length < 2 || length > 256 {
		return 0, false
	}
	if reply[6] != byte(unit) {
		return 0, false
	}
	if body := int(length) - 2; body > 0 {
		if _, err := io.CopyN(io.Discard, conn, int64(body)); err != nil {
			return 0, false
		}
	}
	return reply[7], true
}

// --- Modbus RTU Driver ---
//
// ModbusRTUDriver implements a Modbus RTU driver.
// It supports two modes:
//  1. RTU over TCP (serial bridge gateway): connects to a serial-to-TCP gateway
//     and sends RTU frames (with CRC-16) encapsulated in TCP.
//  2. Pure RTU over serial: opens the configured serial_port with the declared
//     baud rate, data bits, parity and stop bits.
//
// RTU frame format: [SlaveID] [FunctionCode] [Data...] [CRC16-Lo] [CRC16-Hi]
// Unlike Modbus TCP, there is no MBAP header.
type ModbusRTUDriver struct {
	BaseDriver
	host       string        // TCP gateway host (for RTU-over-TCP mode)
	port       int           // TCP gateway port
	serialPort string        // Serial port path (e.g. COM3, /dev/ttyUSB0)
	baudRate   int           // Baud rate (e.g. 9600, 19200, 38400, 115200)
	dataBits   int           // Data bits (7 or 8)
	stopBits   int           // Stop bits (1 or 2)
	parity     string        // Parity: "none", "even", "odd"
	byteOrder  string        // Register layout for multi-register values
	slaveID    int           // Modbus slave ID
	timeout    time.Duration // Read/write timeout
	conn       rtuTransport  // Serial port, or TCP connection in bridge mode
	mu         sync.Mutex
}

// NewModbusRTUDriver creates a new ModbusRTUDriver from device config.
func NewModbusRTUDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	byteOrder, err := normalizeModbusByteOrder(GetConfigString(config, "byte_order", ""))
	if err != nil {
		return nil, err
	}
	serialPort := GetConfigString(config, "serial_port", "")
	d := &ModbusRTUDriver{
		host:       GetConfigString(config, "host", "127.0.0.1"),
		port:       GetConfigInt(config, "port", 502),
		serialPort: serialPort,
		baudRate:   GetConfigInt(config, "baud_rate", 9600),
		dataBits:   GetConfigInt(config, "data_bits", 8),
		stopBits:   GetConfigInt(config, "stop_bits", 1),
		parity:     GetConfigString(config, "parity", "none"),
		byteOrder:  byteOrder,
		slaveID:    GetConfigInt(config, "slave_id", 1),
		timeout:    time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
	}
	// The line settings only matter when a real port is opened, but validating
	// them here means a typo surfaces as a device-save error instead of an
	// unreadable PLC minutes later.
	if serialPort != "" {
		if _, err := d.serialMode(); err != nil {
			return nil, err
		}
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

// rtuTransport is what the RTU frame code needs from either a serial port or a
// TCP socket: framed reads and writes plus a deadline.
type rtuTransport interface {
	io.ReadWriteCloser
	SetDeadline(t time.Time) error
}

// serialTransport adapts a go.bug.st/serial port to rtuTransport. The library has
// no write timeout, so SetDeadline arms only the read half; every RTU write is
// immediately followed by a response read, so a stuck slave still surfaces
// within the configured timeout.
type serialTransport struct {
	port serial.Port
}

func (s *serialTransport) Read(p []byte) (int, error)  { return s.port.Read(p) }
func (s *serialTransport) Write(p []byte) (int, error) { return s.port.Write(p) }
func (s *serialTransport) Close() error                { return s.port.Close() }

func (s *serialTransport) SetDeadline(t time.Time) error {
	return s.port.SetReadTimeout(time.Until(t))
}

// serialMode builds the line configuration the device declares.
func (d *ModbusRTUDriver) serialMode() (*serial.Mode, error) {
	var parity serial.Parity
	switch strings.ToLower(strings.TrimSpace(d.parity)) {
	case "", "none", "n":
		parity = serial.NoParity
	case "even", "e":
		parity = serial.EvenParity
	case "odd", "o":
		parity = serial.OddParity
	case "mark", "m":
		parity = serial.MarkParity
	case "space", "s":
		parity = serial.SpaceParity
	default:
		return nil, fmt.Errorf("invalid parity %q: expected none, even, odd, mark or space", d.parity)
	}
	var stopBits serial.StopBits
	switch d.stopBits {
	case 1:
		stopBits = serial.OneStopBit
	case 2:
		stopBits = serial.TwoStopBits
	default:
		return nil, fmt.Errorf("invalid stop_bits %d: expected 1 or 2", d.stopBits)
	}
	switch d.dataBits {
	case 5, 6, 7, 8:
	default:
		return nil, fmt.Errorf("invalid data_bits %d: expected 5, 6, 7 or 8", d.dataBits)
	}
	if d.baudRate <= 0 {
		return nil, fmt.Errorf("invalid baud_rate %d", d.baudRate)
	}
	return &serial.Mode{
		BaudRate: d.baudRate,
		DataBits: d.dataBits,
		Parity:   parity,
		StopBits: stopBits,
	}, nil
}

func (d *ModbusRTUDriver) Name() string { return "modbus_rtu" }

func (d *ModbusRTUDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn != nil {
		return nil
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.SetConnectionState(StateConnecting, "modbus rtu connecting")

	// A configured serial path means a real COM/tty device. Before this build
	// linked a serial library the same config logged a warning and silently dialed
	// the TCP bridge default, so the gateway appeared to collect from COM3 while it
	// was talking to 127.0.0.1:502.
	if d.serialPort != "" {
		mode, err := d.serialMode()
		if err != nil {
			d.SetConnectionState(StateDisconnected, err.Error())
			return err
		}
		port, err := serial.Open(d.serialPort, mode)
		if err != nil {
			d.SetConnected(false)
			d.SetConnectionState(StateDisconnected, err.Error())
			d.RecordReadFailure()
			return fmt.Errorf("modbus rtu open serial port %s: %w", d.serialPort, err)
		}
		d.conn = &serialTransport{port: port}
		d.SetConnected(true)
		d.SetConnectionState(StateConnected, "modbus rtu serial connected")
		logrus.WithField("device_id", d.DeviceID()).
			WithField("serial_port", d.serialPort).
			WithField("baud_rate", d.baudRate).
			WithField("slave_id", d.slaveID).
			Debug("Modbus RTU connected (serial)")
		return nil
	}

	// No serial path: RTU-over-TCP serial bridge mode.
	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	dialer := net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("modbus rtu connect %s: %w", addr, err)
	}
	d.conn = conn
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "modbus rtu connected")
	logrus.WithField("device_id", d.DeviceID()).
		WithField("addr", addr).
		WithField("slave_id", d.slaveID).
		WithField("baud_rate", d.baudRate).
		Debug("Modbus RTU connected (RTU-over-TCP)")
	return nil
}

func (d *ModbusRTUDriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		err := d.conn.Close()
		d.conn = nil
		d.SetConnected(false)
		return err
	}
	return nil
}

func (d *ModbusRTUDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("modbus rtu not connected")
	}

	// Check circuit breaker before read
	if d.IsCircuitOpen() {
		return nil, fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	now := time.Now()
	result := make([]storage.PointData, 0, len(points))
	anyFailed := false
	var lastLatencyMs float64

	for _, pt := range points {
		start := time.Now()
		val, err := d.readPoint(pt)
		lastLatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
		if err != nil {
			anyFailed = true
			logrus.WithField("device_id", d.DeviceID()).
				WithField("point", pt.Name).
				WithField("address", pt.Address).
				WithField("error", err.Error()).
				Warn("Modbus RTU read failed")
			result = append(result, storage.PointData{
				DeviceID:  d.DeviceID(),
				PointName: pt.Name,
				Value:     nil,
				Quality:   "bad",
				Timestamp: now,
			})
			continue
		}
		result = append(result, storage.PointData{
			DeviceID:  d.DeviceID(),
			PointName: pt.Name,
			Value:     val,
			Quality:   "good",
			Timestamp: now,
		})
	}

	if anyFailed {
		d.RecordReadFailure()
	} else {
		d.RecordReadSuccess(lastLatencyMs)
	}

	return result, nil
}

// readPoint reads a single Modbus RTU point.
func (d *ModbusRTUDriver) readPoint(pt models.PointDef) (interface{}, error) {
	regType, addr, qty, err := parseModbusAddress(pt.Address)
	if err != nil {
		return nil, err
	}
	if regType == "coil" || regType == "discrete" {
		qty = 1
	} else if qty <= 0 {
		qty = modbusRegCount(pt.DataType)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	switch regType {
	case "holding":
		regs, err := d.readHoldingRegistersRTU(addr, qty)
		if err != nil {
			return nil, err
		}
		return decodeModbusValue(reorderRegisters(regs, d.byteOrder), pt.DataType)
	case "input":
		regs, err := d.readInputRegistersRTU(addr, qty)
		if err != nil {
			return nil, err
		}
		return decodeModbusValue(reorderRegisters(regs, d.byteOrder), pt.DataType)
	case "coil":
		coils, err := d.readCoilsRTU(addr, qty)
		if err != nil {
			return nil, err
		}
		if len(coils) > 0 {
			return coils[0], nil
		}
		return false, nil
	case "discrete":
		coils, err := d.readDiscreteInputsRTU(addr, qty)
		if err != nil {
			return nil, err
		}
		if len(coils) > 0 {
			return coils[0], nil
		}
		return false, nil
	default:
		return nil, fmt.Errorf("unknown register type: %s", regType)
	}
}

// sendRTURequest sends a Modbus RTU frame and reads the response.
// RTU frame: [SlaveID(1)] [FunctionCode(1)] [Data...] [CRC16-Lo(1)] [CRC16-Hi(1)]
func (d *ModbusRTUDriver) sendRTURequest(functionCode uint8, data []byte) ([]byte, error) {
	// Build RTU frame
	pdu := make([]byte, 0, 2+len(data)+2)
	pdu = append(pdu, byte(d.slaveID))
	pdu = append(pdu, functionCode)
	pdu = append(pdu, data...)

	// Append CRC-16
	crc := calculateCRC16(pdu)
	pdu = append(pdu, byte(crc&0xFF)) // CRC low byte first
	pdu = append(pdu, byte(crc>>8))   // CRC high byte

	// Send frame
	d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(pdu); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("rtu write: %w", err)
	}

	// Read response: minimum 5 bytes (slaveID + functionCode + exception + CRC(2))
	// First read the header to determine response length
	header := make([]byte, 2)
	if _, err := readFull(d.conn, header); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("rtu read header: %w", err)
	}

	// Check if this is an exception response
	if header[1]&0x80 != 0 {
		// Exception: slaveID + functionCode(0x80|fc) + exceptionCode + CRC(2) = 5 bytes total
		exData := make([]byte, 3) // exception code + CRC(2)
		if _, err := readFull(d.conn, exData); err != nil {
			return nil, fmt.Errorf("rtu read exception: %w", err)
		}
		// Verify CRC
		fullFrame := append(header, exData...)
		if !verifyCRC(fullFrame) {
			return nil, fmt.Errorf("rtu crc mismatch on exception response")
		}
		return nil, fmt.Errorf("modbus rtu exception code: %d", exData[0])
	}

	// Read the rest based on function code
	var respData []byte
	switch functionCode {
	case 0x01, 0x02: // Read coils/discrete inputs
		// Response: byteCount(1) + data + CRC(2)
		byteCountBuf := make([]byte, 1)
		if _, err := readFull(d.conn, byteCountBuf); err != nil {
			return nil, fmt.Errorf("rtu read byte count: %w", err)
		}
		byteCount := int(byteCountBuf[0])
		rest := make([]byte, byteCount+2) // data + CRC
		if _, err := readFull(d.conn, rest); err != nil {
			return nil, fmt.Errorf("rtu read coil data: %w", err)
		}
		respData = append([]byte{byteCountBuf[0]}, rest[:byteCount]...)
		// Verify CRC on full frame
		fullFrame := append(header, byteCountBuf...)
		fullFrame = append(fullFrame, rest...)
		if !verifyCRC(fullFrame) {
			return nil, fmt.Errorf("rtu crc mismatch on read coils")
		}
	case 0x03, 0x04: // Read holding/input registers
		byteCountBuf := make([]byte, 1)
		if _, err := readFull(d.conn, byteCountBuf); err != nil {
			return nil, fmt.Errorf("rtu read byte count: %w", err)
		}
		byteCount := int(byteCountBuf[0])
		rest := make([]byte, byteCount+2) // data + CRC
		if _, err := readFull(d.conn, rest); err != nil {
			return nil, fmt.Errorf("rtu read register data: %w", err)
		}
		respData = append([]byte{byteCountBuf[0]}, rest[:byteCount]...)
		fullFrame := append(header, byteCountBuf...)
		fullFrame = append(fullFrame, rest...)
		if !verifyCRC(fullFrame) {
			return nil, fmt.Errorf("rtu crc mismatch on read registers")
		}
	case 0x05, 0x06, 0x0F, 0x10: // Write functions
		// Response: echo address + value/qty + CRC(2) = 4 data bytes + 2 CRC
		rest := make([]byte, 6) // 4 data + 2 CRC
		if _, err := readFull(d.conn, rest); err != nil {
			return nil, fmt.Errorf("rtu read write response: %w", err)
		}
		respData = rest[:4]
		fullFrame := append(header, rest...)
		if !verifyCRC(fullFrame) {
			return nil, fmt.Errorf("rtu crc mismatch on write response")
		}
	default:
		return nil, fmt.Errorf("unsupported function code: 0x%02X", functionCode)
	}

	return respData, nil
}

// readHoldingRegistersRTU reads holding registers via RTU.
func (d *ModbusRTUDriver) readHoldingRegistersRTU(addr, qty int) ([]uint16, error) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(qty))
	resp, err := d.sendRTURequest(0x03, data)
	if err != nil {
		return nil, err
	}
	if len(resp) < 1 {
		return nil, fmt.Errorf("short response")
	}
	byteCount := int(resp[0])
	if len(resp) < 1+byteCount {
		return nil, fmt.Errorf("response too short")
	}
	regs := make([]uint16, byteCount/2)
	for i := 0; i < len(regs); i++ {
		regs[i] = binary.BigEndian.Uint16(resp[1+i*2:])
	}
	return regs, nil
}

// readInputRegistersRTU reads input registers via RTU.
func (d *ModbusRTUDriver) readInputRegistersRTU(addr, qty int) ([]uint16, error) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(qty))
	resp, err := d.sendRTURequest(0x04, data)
	if err != nil {
		return nil, err
	}
	if len(resp) < 1 {
		return nil, fmt.Errorf("short response")
	}
	byteCount := int(resp[0])
	regs := make([]uint16, byteCount/2)
	for i := 0; i < len(regs); i++ {
		regs[i] = binary.BigEndian.Uint16(resp[1+i*2:])
	}
	return regs, nil
}

// readCoilsRTU reads coils via RTU.
func (d *ModbusRTUDriver) readCoilsRTU(addr, qty int) ([]bool, error) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(qty))
	resp, err := d.sendRTURequest(0x01, data)
	if err != nil {
		return nil, err
	}
	if len(resp) < 1 {
		return nil, fmt.Errorf("short response")
	}
	byteCount := int(resp[0])
	coils := make([]bool, qty)
	for i := 0; i < qty && i < byteCount*8; i++ {
		if resp[1+i/8]&(1<<uint(i%8)) != 0 {
			coils[i] = true
		}
	}
	return coils, nil
}

// readDiscreteInputsRTU reads discrete inputs via RTU.
func (d *ModbusRTUDriver) readDiscreteInputsRTU(addr, qty int) ([]bool, error) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], uint16(addr))
	binary.BigEndian.PutUint16(data[2:4], uint16(qty))
	resp, err := d.sendRTURequest(0x02, data)
	if err != nil {
		return nil, err
	}
	if len(resp) < 1 {
		return nil, fmt.Errorf("short response")
	}
	byteCount := int(resp[0])
	coils := make([]bool, qty)
	for i := 0; i < qty && i < byteCount*8; i++ {
		if resp[1+i/8]&(1<<uint(i%8)) != 0 {
			coils[i] = true
		}
	}
	return coils, nil
}

func (d *ModbusRTUDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	return d.writePoint(ctx, point, value, "")
}

// WritePointTyped writes with the point's declared data type, which decides how a
// value spanning several registers is laid out in them.
func (d *ModbusRTUDriver) WritePointTyped(ctx context.Context, point string, value interface{}, dataType string) error {
	return d.writePoint(ctx, point, value, dataType)
}

// WritePointAtAddress writes to the point's register address (see ModbusTCP).
func (d *ModbusRTUDriver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	return d.writePoint(ctx, address, value, dataType)
}

func (d *ModbusRTUDriver) writePoint(ctx context.Context, point string, value interface{}, dataType string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		return fmt.Errorf("modbus rtu not connected")
	}

	// Check circuit breaker before write
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	regType, addr, qty, err := parseModbusAddress(point)
	if err != nil {
		return fmt.Errorf("invalid point address: %w", err)
	}
	qty = modbusWriteQty(qty, dataType)

	switch regType {
	case "holding":
		if qty == 1 {
			val, err := toUint16ForType(value, dataType)
			if err != nil {
				return err
			}
			data := make([]byte, 4)
			binary.BigEndian.PutUint16(data[0:2], uint16(addr))
			binary.BigEndian.PutUint16(data[2:4], val)
			_, err = d.sendRTURequest(0x06, data)
			return err
		}
		val, err := modbusEncodeRegisters(value, dataType, qty)
		if err != nil {
			return err
		}
		val = reorderRegisters(val, d.byteOrder)
		byteCount := len(val) * 2
		data := make([]byte, 5+byteCount)
		binary.BigEndian.PutUint16(data[0:2], uint16(addr))
		binary.BigEndian.PutUint16(data[2:4], uint16(len(val)))
		data[4] = byte(byteCount)
		for i, v := range val {
			binary.BigEndian.PutUint16(data[5+i*2:], v)
		}
		_, err = d.sendRTURequest(0x10, data)
		return err
	case "coil":
		val, ok := value.(bool)
		if !ok {
			n, ok2 := toUint16(value)
			if !ok2 {
				return fmt.Errorf("cannot convert value to bool: %v", value)
			}
			val = n != 0
		}
		data := make([]byte, 4)
		binary.BigEndian.PutUint16(data[0:2], uint16(addr))
		if val {
			binary.BigEndian.PutUint16(data[2:4], 0xFF00)
		} else {
			binary.BigEndian.PutUint16(data[2:4], 0x0000)
		}
		_, err := d.sendRTURequest(0x05, data)
		return err
	default:
		return fmt.Errorf("write not supported for register type: %s", regType)
	}
}

func (d *ModbusRTUDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *ModbusRTUDriver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("not connected")
	}
	start := time.Now()
	_, err := d.readHoldingRegistersRTU(0, 1)
	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		d.RecordReadFailure()
		return err
	}
	d.RecordReadSuccess(latencyMs)
	return nil
}

// calculateCRC16 computes the Modbus CRC-16 for the given data.
// Uses the CRC-16/MODBUS algorithm (polynomial 0xA001, init 0xFFFF).
func calculateCRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// verifyCRC checks if the CRC-16 at the end of the frame is valid.
func verifyCRC(frame []byte) bool {
	if len(frame) < 3 {
		return false
	}
	data := frame[:len(frame)-2]
	receivedCRC := uint16(frame[len(frame)-2]) | uint16(frame[len(frame)-1])<<8
	return calculateCRC16(data) == receivedCRC
}

// --- Modbus Slave Driver (Modbus TCP Server) ---
//
// ModbusSlaveDriver acts as a Modbus TCP slave/server.
// It listens for incoming Modbus TCP connections and serves data from
// its internal memory map. This allows EdgeLite to expose data to
// external SCADA/HMI systems.
//
// Supported function codes:
//   0x03 - Read Holding Registers
//   0x04 - Read Input Registers
//   0x01 - Read Coils
//   0x02 - Read Discrete Inputs
//   0x05 - Write Single Coil
//   0x06 - Write Single Register
//   0x0F - Write Multiple Coils
//   0x10 - Write Multiple Registers
type ModbusSlaveDriver struct {
	BaseDriver
	host         string
	port         int
	byteOrder    string
	holdingSize  int
	inputSize    int
	coilSize     int
	discreteSize int
	holdingRegs  []uint16
	inputRegs    []uint16
	coilBits     []bool
	discreteBits []bool
	listener     net.Listener
	access       *slaveAccess
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// NewModbusSlaveDriver creates a new ModbusSlaveDriver.
func NewModbusSlaveDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	byteOrder, err := normalizeModbusByteOrder(GetConfigString(config, "byte_order", ""))
	if err != nil {
		return nil, err
	}
	access, err := parseSlaveAccess(config)
	if err != nil {
		return nil, err
	}
	d := &ModbusSlaveDriver{
		host:         GetConfigString(config, "host", "0.0.0.0"),
		port:         GetConfigInt(config, "port", 5020),
		byteOrder:    byteOrder,
		holdingSize:  GetConfigInt(config, "holding_size", 1000),
		inputSize:    GetConfigInt(config, "input_size", 1000),
		coilSize:     GetConfigInt(config, "coil_size", 1000),
		discreteSize: GetConfigInt(config, "discrete_size", 1000),
		access:       access,
	}
	d.holdingRegs = make([]uint16, d.holdingSize)
	d.inputRegs = make([]uint16, d.inputSize)
	d.coilBits = make([]bool, d.coilSize)
	d.discreteBits = make([]bool, d.discreteSize)
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *ModbusSlaveDriver) Name() string { return "modbus_slave" }

// SnapshotHolding/Input/Coils/Discrete expose the slave's data stores for the
// register-table API. Copies under the driver lock so a concurrent master
// frame cannot tear a read.
func (d *ModbusSlaveDriver) SnapshotHolding() []uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]uint16, len(d.holdingRegs))
	copy(out, d.holdingRegs)
	return out
}

func (d *ModbusSlaveDriver) SnapshotInput() []uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]uint16, len(d.inputRegs))
	copy(out, d.inputRegs)
	return out
}

func (d *ModbusSlaveDriver) SnapshotCoils() []bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]bool, len(d.coilBits))
	copy(out, d.coilBits)
	return out
}

func (d *ModbusSlaveDriver) SnapshotDiscrete() []bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]bool, len(d.discreteBits))
	copy(out, d.discreteBits)
	return out
}

// SetRegister writes one register/coil through the API. Accepts the numeric
// forms the UI sends; bool targets take truthy values.
func (d *ModbusSlaveDriver) SetRegister(regType string, offset int, value interface{}) (interface{}, error) {
	switch strings.ToLower(regType) {
	case "holding", "input":
		var v uint16
		switch n := value.(type) {
		case float64:
			v = uint16(int64(n) & 0xFFFF)
		case string:
			parsed, err := strconv.ParseUint(strings.TrimSpace(n), 0, 16)
			if err != nil {
				return nil, fmt.Errorf("invalid register value %q", n)
			}
			v = uint16(parsed)
		default:
			return nil, fmt.Errorf("invalid register value type")
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if strings.ToLower(regType) == "holding" {
			if offset >= len(d.holdingRegs) {
				return nil, fmt.Errorf("offset %d out of range", offset)
			}
			d.holdingRegs[offset] = v
		} else {
			if offset >= len(d.inputRegs) {
				return nil, fmt.Errorf("offset %d out of range", offset)
			}
			d.inputRegs[offset] = v
		}
		return v, nil
	case "coil", "discrete":
		b := value == true || value == "true" || value == "on" || value == float64(1)
		d.mu.Lock()
		defer d.mu.Unlock()
		if strings.ToLower(regType) == "coil" {
			if offset >= len(d.coilBits) {
				return nil, fmt.Errorf("offset %d out of range", offset)
			}
			d.coilBits[offset] = b
		} else {
			if offset >= len(d.discreteBits) {
				return nil, fmt.Errorf("offset %d out of range", offset)
			}
			d.discreteBits[offset] = b
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unknown register type %q", regType)
	}
}

// slaveAccess is the peer policy of the Modbus TCP server. The slave publishes
// the gateway register map to whoever can reach the port, so allowed_ips,
// max_connections and the abuse counters are the only thing standing between an
// unauthenticated host on the plant network and process data.
//
// An empty allowed_ips means "any peer", which is what the field documented
// before it did anything; it stays that way so existing devices keep serving.
type slaveAccess struct {
	nets        []*net.IPNet
	hosts       map[string]bool
	maxConns    int
	abuseLimit  int
	abuseWindow time.Duration
	banDuration time.Duration

	mu    sync.Mutex
	conns map[string]int
	peers map[string]*slavePeerState
}

type slavePeerState struct {
	windowStart time.Time
	requests    int
	bannedUntil time.Time
}

// parseSlaveAccess validates the access-control fields at construction time: a
// typo in a whitelist has to fail the device save, not silently serve everyone.
func parseSlaveAccess(config map[string]interface{}) (*slaveAccess, error) {
	a := &slaveAccess{
		hosts:       map[string]bool{},
		maxConns:    GetConfigInt(config, "max_connections", 0),
		abuseLimit:  GetConfigInt(config, "abuse_threshold", 0),
		abuseWindow: time.Duration(GetConfigFloat(config, "abuse_window", 60)) * time.Second,
		banDuration: time.Duration(GetConfigFloat(config, "ban_duration", 300)) * time.Second,
		conns:       map[string]int{},
		peers:       map[string]*slavePeerState{},
	}
	if a.maxConns < 0 {
		return nil, fmt.Errorf("max_connections must be 0 (unlimited) or positive, got %d", a.maxConns)
	}
	if a.abuseLimit < 0 {
		return nil, fmt.Errorf("abuse_threshold must be 0 (disabled) or positive, got %d", a.abuseLimit)
	}
	if a.abuseLimit > 0 && a.abuseWindow <= 0 {
		return nil, fmt.Errorf("abuse_threshold %d needs a positive abuse_window", a.abuseLimit)
	}
	if a.abuseWindow <= 0 {
		a.abuseWindow = 60 * time.Second
	}
	if a.banDuration <= 0 {
		a.banDuration = 300 * time.Second
	}
	for _, raw := range configStringList(config["allowed_ips"]) {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, block, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("allowed_ips %q is not a valid CIDR block: %w", entry, err)
			}
			a.nets = append(a.nets, block)
			continue
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			// Resolving a hostname inside Accept would block the listener, and a
			// name that disappears later would silently widen the whitelist.
			return nil, fmt.Errorf("allowed_ips %q is not an IP address or CIDR block; hostnames are not supported", entry)
		}
		a.hosts[ip.String()] = true
	}
	return a, nil
}

// configStringList accepts both shapes the device form can produce for a list:
// a real JSON array and a single comma-separated string.
func configStringList(value interface{}) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			} else if item != nil {
				out = append(out, fmt.Sprint(item))
			}
		}
		return out
	case string:
		return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == '\n' })
	default:
		return []string{fmt.Sprint(v)}
	}
}

func (a *slaveAccess) permits(ip net.IP) bool {
	if a == nil || (len(a.nets) == 0 && len(a.hosts) == 0) {
		return true
	}
	if ip == nil {
		return false
	}
	if a.hosts[ip.String()] {
		return true
	}
	for _, block := range a.nets {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

func (a *slaveAccess) isBanned(key string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.peers[key]
	if state == nil {
		return false
	}
	if state.bannedUntil.IsZero() {
		return false
	}
	if time.Now().Before(state.bannedUntil) {
		return true
	}
	state.bannedUntil = time.Time{}
	state.requests = 0
	return false
}

// acquire claims a connection slot for the peer, honouring max_connections.
func (a *slaveAccess) acquire(key string) bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.maxConns > 0 {
		total := 0
		for _, n := range a.conns {
			total += n
		}
		if total >= a.maxConns {
			return false
		}
	}
	a.conns[key]++
	return true
}

func (a *slaveAccess) release(key string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conns[key] <= 1 {
		delete(a.conns, key)
		return
	}
	a.conns[key]--
}

// noteRequest counts one PDU against the peer's abuse window and reports whether
// the request may be served. Crossing the threshold bans the peer for
// ban_duration, which is the only defence against a client hammering the port.
func (a *slaveAccess) noteRequest(key string) bool {
	if a == nil || a.abuseLimit <= 0 {
		return true
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.peers[key]
	if state == nil {
		state = &slavePeerState{windowStart: now}
		a.peers[key] = state
	}
	if now.Sub(state.windowStart) > a.abuseWindow {
		state.windowStart = now
		state.requests = 0
	}
	state.requests++
	if state.requests > a.abuseLimit {
		state.bannedUntil = now.Add(a.banDuration)
		return false
	}
	return true
}

// peerKey normalizes a RemoteAddr into a bare IP string.
func peerKey(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

func (d *ModbusSlaveDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.listener != nil {
		return nil
	}

	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		d.SetConnected(false)
		return fmt.Errorf("modbus slave listen %s: %w", addr, err)
	}
	d.listener = listener
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.SetConnected(true)

	// Start accepting connections
	d.wg.Add(1)
	go d.acceptLoop()

	logrus.WithField("device_id", d.DeviceID()).
		WithField("addr", addr).
		Info("Modbus Slave TCP server started")
	return nil
}

func (d *ModbusSlaveDriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cancel != nil {
		d.cancel()
	}
	if d.listener != nil {
		d.listener.Close()
		d.listener = nil
	}
	d.SetConnected(false)
	d.wg.Wait()
	logrus.Info("Modbus Slave TCP server stopped")
	return nil
}

// acceptLoop accepts incoming TCP connections.
func (d *ModbusSlaveDriver) acceptLoop() {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			return
		default:
		}
		conn, err := d.listener.Accept()
		if err != nil {
			if d.ctx.Err() != nil {
				return
			}
			logrus.WithField("error", err.Error()).Warn("Modbus slave accept error")
			continue
		}
		key := peerKey(conn.RemoteAddr())
		ip := net.ParseIP(key)
		if d.access.isBanned(key) {
			_ = conn.Close()
			logrus.WithField("device_id", d.DeviceID()).
				WithField("peer", key).
				Warn("Modbus slave rejected a banned peer")
			continue
		}
		if !d.access.permits(ip) {
			_ = conn.Close()
			logrus.WithField("device_id", d.DeviceID()).
				WithField("peer", key).
				Warn("Modbus slave rejected a peer outside allowed_ips")
			continue
		}
		if !d.access.acquire(key) {
			_ = conn.Close()
			logrus.WithField("device_id", d.DeviceID()).
				WithField("peer", key).
				WithField("max_connections", d.access.maxConns).
				Warn("Modbus slave at connection capacity")
			continue
		}
		d.wg.Add(1)
		go d.handleConnection(conn, key)
	}
}

// handleConnection handles a single Modbus TCP client connection.
func (d *ModbusSlaveDriver) handleConnection(conn net.Conn, peer string) {
	defer d.wg.Done()
	defer conn.Close()
	defer d.access.release(peer)

	clientAddr := conn.RemoteAddr().String()
	logrus.WithField("client", clientAddr).Debug("Modbus slave client connected")

	for {
		select {
		case <-d.ctx.Done():
			return
		default:
		}

		// Read MBAP header (7 bytes)
		header := make([]byte, 7)
		if _, err := readFull(conn, header); err != nil {
			return
		}
		if !d.access.noteRequest(peer) {
			logrus.WithField("device_id", d.DeviceID()).
				WithField("peer", peer).
				WithField("abuse_threshold", d.access.abuseLimit).
				Warn("Modbus slave peer exceeded its request budget, banning")
			return
		}

		// Parse MBAP header
		transactionID := binary.BigEndian.Uint16(header[0:2])
		protocolID := binary.BigEndian.Uint16(header[2:4])
		length := binary.BigEndian.Uint16(header[4:6])
		unitID := header[6]

		if protocolID != 0 {
			continue // Not Modbus protocol
		}

		// Read PDU
		pduLength := int(length) - 1 // Subtract unit ID
		if pduLength <= 0 || pduLength > 256 {
			continue
		}
		pdu := make([]byte, pduLength)
		if _, err := readFull(conn, pdu); err != nil {
			return
		}

		// Process the request
		response := d.processRequest(unitID, pdu)

		// Build response MBAP + PDU
		respLen := len(response) + 1 // +1 for unit ID
		respMBAP := make([]byte, 7)
		binary.BigEndian.PutUint16(respMBAP[0:2], transactionID)
		binary.BigEndian.PutUint16(respMBAP[2:4], 0) // Protocol ID
		binary.BigEndian.PutUint16(respMBAP[4:6], uint16(respLen))
		respMBAP[6] = unitID

		conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write(append(respMBAP, response...)); err != nil {
			return
		}
	}
}

// processRequest processes a Modbus PDU and returns the response PDU.
func (d *ModbusSlaveDriver) processRequest(unitID byte, pdu []byte) []byte {
	if len(pdu) < 1 {
		return d.exceptionResponse(0, 0x01) // Illegal function
	}
	functionCode := pdu[0]

	switch functionCode {
	case 0x01: // Read Coils
		return d.handleReadCoils(pdu, false)
	case 0x02: // Read Discrete Inputs
		return d.handleReadCoils(pdu, true)
	case 0x03: // Read Holding Registers
		return d.handleReadRegisters(pdu, false)
	case 0x04: // Read Input Registers
		return d.handleReadRegisters(pdu, true)
	case 0x05: // Write Single Coil
		return d.handleWriteSingleCoil(pdu)
	case 0x06: // Write Single Register
		return d.handleWriteSingleRegister(pdu)
	case 0x0F: // Write Multiple Coils
		return d.handleWriteMultipleCoils(pdu)
	case 0x10: // Write Multiple Registers
		return d.handleWriteMultipleRegisters(pdu)
	default:
		return d.exceptionResponse(functionCode, 0x01) // Illegal function
	}
}

// handleReadRegisters handles function codes 0x03 and 0x04.
func (d *ModbusSlaveDriver) handleReadRegisters(pdu []byte, inputRegs bool) []byte {
	if len(pdu) < 5 {
		return d.exceptionResponse(pdu[0], 0x04) // Slave device failure
	}
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	qty := int(binary.BigEndian.Uint16(pdu[3:5]))

	if qty < 1 || qty > 125 {
		return d.exceptionResponse(pdu[0], 0x03) // Illegal data value
	}

	var regs []uint16
	if inputRegs {
		if addr+qty > len(d.inputRegs) {
			return d.exceptionResponse(pdu[0], 0x02) // Illegal data address
		}
		regs = d.inputRegs[addr : addr+qty]
	} else {
		if addr+qty > len(d.holdingRegs) {
			return d.exceptionResponse(pdu[0], 0x02) // Illegal data address
		}
		regs = d.holdingRegs[addr : addr+qty]
	}

	// Build response: functionCode + byteCount + data
	byteCount := qty * 2
	resp := make([]byte, 2+byteCount)
	resp[0] = pdu[0]
	resp[1] = byte(byteCount)
	for i, r := range regs {
		binary.BigEndian.PutUint16(resp[2+i*2:], r)
	}
	return resp
}

// handleReadCoils handles function codes 0x01 and 0x02.
func (d *ModbusSlaveDriver) handleReadCoils(pdu []byte, discrete bool) []byte {
	if len(pdu) < 5 {
		return d.exceptionResponse(pdu[0], 0x04)
	}
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	qty := int(binary.BigEndian.Uint16(pdu[3:5]))

	if qty < 1 || qty > 2000 {
		return d.exceptionResponse(pdu[0], 0x03)
	}

	var bits []bool
	if discrete {
		if addr+qty > len(d.discreteBits) {
			return d.exceptionResponse(pdu[0], 0x02)
		}
		bits = d.discreteBits[addr : addr+qty]
	} else {
		if addr+qty > len(d.coilBits) {
			return d.exceptionResponse(pdu[0], 0x02)
		}
		bits = d.coilBits[addr : addr+qty]
	}

	// Build response
	byteCount := (qty + 7) / 8
	resp := make([]byte, 2+byteCount)
	resp[0] = pdu[0]
	resp[1] = byte(byteCount)
	for i, b := range bits {
		if b {
			resp[2+i/8] |= 1 << uint(i%8)
		}
	}
	return resp
}

// handleWriteSingleCoil handles function code 0x05.
func (d *ModbusSlaveDriver) handleWriteSingleCoil(pdu []byte) []byte {
	if len(pdu) < 5 {
		return d.exceptionResponse(pdu[0], 0x04)
	}
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	value := binary.BigEndian.Uint16(pdu[3:5])

	if addr >= len(d.coilBits) {
		return d.exceptionResponse(pdu[0], 0x02)
	}

	d.mu.Lock()
	d.coilBits[addr] = (value == 0xFF00)
	d.mu.Unlock()

	// Echo back the request as response
	return pdu[:5]
}

// handleWriteSingleRegister handles function code 0x06.
func (d *ModbusSlaveDriver) handleWriteSingleRegister(pdu []byte) []byte {
	if len(pdu) < 5 {
		return d.exceptionResponse(pdu[0], 0x04)
	}
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	value := binary.BigEndian.Uint16(pdu[3:5])

	if addr >= len(d.holdingRegs) {
		return d.exceptionResponse(pdu[0], 0x02)
	}

	d.mu.Lock()
	d.holdingRegs[addr] = value
	d.mu.Unlock()

	// Echo back the request as response
	return pdu[:5]
}

// handleWriteMultipleCoils handles function code 0x0F.
func (d *ModbusSlaveDriver) handleWriteMultipleCoils(pdu []byte) []byte {
	if len(pdu) < 6 {
		return d.exceptionResponse(pdu[0], 0x04)
	}
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	qty := int(binary.BigEndian.Uint16(pdu[3:5]))
	byteCount := int(pdu[5])

	if qty < 1 || qty > 1968 || byteCount != (qty+7)/8 {
		return d.exceptionResponse(pdu[0], 0x03)
	}
	if len(pdu) < 6+byteCount {
		return d.exceptionResponse(pdu[0], 0x04)
	}
	if addr+qty > len(d.coilBits) {
		return d.exceptionResponse(pdu[0], 0x02)
	}

	d.mu.Lock()
	for i := 0; i < qty; i++ {
		d.coilBits[addr+i] = pdu[6+i/8]&(1<<uint(i%8)) != 0
	}
	d.mu.Unlock()

	// Response: functionCode + addr + qty
	resp := make([]byte, 5)
	resp[0] = pdu[0]
	binary.BigEndian.PutUint16(resp[1:3], uint16(addr))
	binary.BigEndian.PutUint16(resp[3:5], uint16(qty))
	return resp
}

// handleWriteMultipleRegisters handles function code 0x10.
func (d *ModbusSlaveDriver) handleWriteMultipleRegisters(pdu []byte) []byte {
	if len(pdu) < 6 {
		return d.exceptionResponse(pdu[0], 0x04)
	}
	addr := int(binary.BigEndian.Uint16(pdu[1:3]))
	qty := int(binary.BigEndian.Uint16(pdu[3:5]))
	byteCount := int(pdu[5])

	if qty < 1 || qty > 123 || byteCount != qty*2 {
		return d.exceptionResponse(pdu[0], 0x03)
	}
	if len(pdu) < 6+byteCount {
		return d.exceptionResponse(pdu[0], 0x04)
	}
	if addr+qty > len(d.holdingRegs) {
		return d.exceptionResponse(pdu[0], 0x02)
	}

	d.mu.Lock()
	for i := 0; i < qty; i++ {
		d.holdingRegs[addr+i] = binary.BigEndian.Uint16(pdu[6+i*2:])
	}
	d.mu.Unlock()

	resp := make([]byte, 5)
	resp[0] = pdu[0]
	binary.BigEndian.PutUint16(resp[1:3], uint16(addr))
	binary.BigEndian.PutUint16(resp[3:5], uint16(qty))
	return resp
}

// exceptionResponse builds a Modbus exception response.
func (d *ModbusSlaveDriver) exceptionResponse(functionCode byte, exceptionCode byte) []byte {
	return []byte{functionCode | 0x80, exceptionCode}
}

// ReadPoints reads data from the slave's internal memory map.
// This is used by the collect scheduler to expose slave data to EdgeLite.
func (d *ModbusSlaveDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	now := time.Now()
	result := make([]storage.PointData, 0, len(points))
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, pt := range points {
		regType, addr, qty, err := parseModbusAddress(pt.Address)
		if err != nil {
			result = append(result, storage.PointData{
				DeviceID: d.DeviceID(), PointName: pt.Name,
				Value: nil, Quality: "bad", Timestamp: now,
			})
			continue
		}
		if regType == "coil" || regType == "discrete" {
			qty = 1
		} else if qty <= 0 {
			qty = modbusRegCount(pt.DataType)
		}

		var val interface{}
		var reason string
		switch regType {
		case "holding":
			if addr+qty > len(d.holdingRegs) {
				reason = fmt.Sprintf("holding address %d+%d out of range", addr, qty)
			} else if v, derr := decodeModbusValue(reorderRegisters(d.holdingRegs[addr:addr+qty], d.byteOrder), pt.DataType); derr != nil {
				reason = derr.Error()
			} else {
				val = v
			}
		case "input":
			if addr+qty > len(d.inputRegs) {
				reason = fmt.Sprintf("input address %d+%d out of range", addr, qty)
			} else if v, derr := decodeModbusValue(reorderRegisters(d.inputRegs[addr:addr+qty], d.byteOrder), pt.DataType); derr != nil {
				reason = derr.Error()
			} else {
				val = v
			}
		case "coil":
			if addr >= len(d.coilBits) {
				reason = fmt.Sprintf("coil address %d out of range", addr)
			} else {
				val = d.coilBits[addr]
			}
		case "discrete":
			if addr >= len(d.discreteBits) {
				reason = fmt.Sprintf("discrete address %d out of range", addr)
			} else {
				val = d.discreteBits[addr]
			}
		default:
			reason = fmt.Sprintf("unknown register type: %s", regType)
		}
		if reason != "" {
			// A nil value reported as "good" made an unmapped address look like
			// real data downstream.
			result = append(result, storage.PointData{
				DeviceID: d.DeviceID(), PointName: pt.Name,
				Value: nil, Quality: "bad", Timestamp: now,
			})
			logrus.WithField("device_id", d.DeviceID()).
				WithField("point", pt.Name).
				Debug("Modbus slave point unavailable: " + reason)
			continue
		}

		result = append(result, storage.PointData{
			DeviceID: d.DeviceID(), PointName: pt.Name,
			Value: val, Quality: "good", Timestamp: now,
		})
	}
	return result, nil
}

// WritePoint writes a value to the slave's internal memory map.
func (d *ModbusSlaveDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	return d.writePoint(ctx, point, value, "")
}

// WritePointTyped writes with the point's declared data type, which decides how a
// value spanning several registers is laid out in them.
func (d *ModbusSlaveDriver) WritePointTyped(ctx context.Context, point string, value interface{}, dataType string) error {
	return d.writePoint(ctx, point, value, dataType)
}

// WritePointAtAddress writes to the point's register address (see ModbusTCP).
func (d *ModbusSlaveDriver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	return d.writePoint(ctx, address, value, dataType)
}

func (d *ModbusSlaveDriver) writePoint(ctx context.Context, point string, value interface{}, dataType string) error {
	regType, addr, qty, err := parseModbusAddress(point)
	if err != nil {
		return err
	}
	qty = modbusWriteQty(qty, dataType)

	d.mu.Lock()
	defer d.mu.Unlock()

	switch regType {
	case "holding":
		if addr+qty > len(d.holdingRegs) {
			return fmt.Errorf("address out of range")
		}
		if qty == 1 {
			v, err := toUint16ForType(value, dataType)
			if err != nil {
				return err
			}
			d.holdingRegs[addr] = v
		} else {
			vals, err := modbusEncodeRegisters(value, dataType, qty)
			if err != nil {
				return err
			}
			// Store in the layout the exposed slave advertises, so an external
			// master reading the same registers sees what it expects; the read path
			// reorders back, and each order is its own inverse.
			for i, v := range reorderRegisters(vals, d.byteOrder) {
				d.holdingRegs[addr+i] = v
			}
		}
	case "coil":
		if addr >= len(d.coilBits) {
			return fmt.Errorf("address out of range")
		}
		val, ok := value.(bool)
		if !ok {
			n, ok2 := toUint16(value)
			if !ok2 {
				return fmt.Errorf("cannot convert value to bool")
			}
			val = n != 0
		}
		d.coilBits[addr] = val
	default:
		return fmt.Errorf("write not supported for register type: %s", regType)
	}
	return nil
}

func (d *ModbusSlaveDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *ModbusSlaveDriver) HealthCheck(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.listener == nil {
		return fmt.Errorf("modbus slave not listening")
	}
	return nil
}
