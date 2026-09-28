package drivers

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// MC device codes for Mitsubishi MELSEC Communication protocol
var mcDeviceCodes = map[string]byte{
	"M": 0x90, "D": 0xA8, "R": 0xAF, "Z": 0xCC,
	"T": 0xC2, "C": 0xC4, "X": 0x9C, "Y": 0x9D,
	"S": 0xA1, "B": 0xA0, "W": 0xB4, "F": 0xAF,
}

type MCDriver struct {
	BaseDriver
	host    string
	port    int
	timeout time.Duration
	conn    net.Conn
	mu      sync.Mutex
	plcType string
}

func NewMCDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	d := &MCDriver{
		host:    GetConfigString(config, "host", "127.0.0.1"),
		port:    GetConfigInt(config, "port", 5000),
		plcType: GetConfigString(config, "plc_type", "Q"),
		timeout: time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *MCDriver) Name() string { return "mitsubishi_mc" }

func (d *MCDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		return nil
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.SetConnectionState(StateConnecting, "mc connecting")

	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	dialer := net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("mc connect %s: %w", addr, err)
	}
	d.conn = conn
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "mc connected")
	return nil
}

func (d *MCDriver) Disconnect() error {
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

func (d *MCDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("mc not connected")
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
				WithField("point", pt.Name).Warn("MC read failed")
			result = append(result, storage.PointData{
				DeviceID: d.DeviceID(), PointName: pt.Name, Value: nil, Quality: "bad", Timestamp: now,
			})
			continue
		}
		result = append(result, storage.PointData{
			DeviceID: d.DeviceID(), PointName: pt.Name, Value: val, Quality: "good", Timestamp: now,
		})
	}

	if anyFailed {
		d.RecordReadFailure()
	} else {
		d.RecordReadSuccess(lastLatencyMs)
	}

	return result, nil
}

func (d *MCDriver) readPoint(pt models.PointDef) (interface{}, error) {
	device, addr, err := parseMCAddress(pt.Address)
	if err != nil {
		return nil, err
	}
	dataType := strings.ToLower(strings.TrimSpace(pt.DataType))
	if dataType == "" {
		dataType = "int16"
	}
	// Bit-unit access only for bool points on a bit-accessible device; a word
	// device (or a bool modelled on a word) keeps the word read.
	if dataType == "bool" && isMCBitDevice(device) {
		return d.mcReadBit(device, addr)
	}
	words := mcWordCount(dataType)
	regs, err := d.mcReadWords(device, addr, words)
	if err != nil {
		return nil, err
	}
	if len(regs) < words {
		return nil, fmt.Errorf("mc read returned %d of %d words for %s", len(regs), words, pt.Address)
	}
	return decodeMCValue(regs, dataType), nil
}

// mcWordCount returns how many 16-bit words a value of dataType occupies.
// Reading a single word for REAL/DINT (the legacy behaviour) made the decoder
// fall through to the raw uint16, so 32/64-bit points reported the wrong type.
func mcWordCount(dataType string) int {
	switch dataType {
	case "int32", "uint32", "dint", "dword", "long", "float32", "float", "real":
		return 2
	case "float64", "double", "lreal":
		return 4
	default:
		return 1
	}
}

// mcIsWideDevice reports whether the configured CPU series uses the iQ-R
// extended device field (4-byte number + 2-byte code).
func (d *MCDriver) mcIsWideDevice() bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(d.plcType)), "IQ-R")
}

// mcBuildRequest assembles an SLMP 3E-frame binary request frame.
//
// Fixed header (9 bytes):
//
//	[0:2] subheader 50 00, [2] network, [3] PC, [4:6] target module IO,
//	[6] target station, [7:9] request data length (little-endian).
//
// Data block (counted by the length field): CPU monitor timer (0x0010) +
// command + subcommand + head device + device count + optional write payload.
//
// The head device field is the SLMP standard order: head device NUMBER
// (3 bytes little-endian, 4 bytes on iQ-R) followed by the device CODE
// (1 byte, 2 bytes on iQ-R). The 24/16-bit split is what pymcprotocol and real
// MELSEC CPUs use; sending code first shifts every later field by one byte and
// makes the CPU read a garbage device number.
func mcBuildRequest(cmd uint16, subcmd uint16, deviceCode byte, startAddr int, count int, payload []byte) []byte {
	return mcBuildRequestSeries(cmd, subcmd, deviceCode, startAddr, count, payload, false)
}

// mcBuildRequestSeries builds the same frame, with wideDevice selecting the
// iQ-R / iQ-R08PS extended device field (4-byte number + 2-byte code).
func mcBuildRequestSeries(cmd uint16, subcmd uint16, deviceCode byte, startAddr int, count int, payload []byte, wideDevice bool) []byte {
	block := make([]byte, 0, 16+len(payload))
	block = append(block, 0x10, 0x00) // CPU monitor timer 0x0010 (LE)
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, cmd)
	block = append(block, b...)
	binary.LittleEndian.PutUint16(b, subcmd)
	block = append(block, b...)
	if wideDevice {
		block = append(block,
			byte(startAddr&0xFF), byte((startAddr>>8)&0xFF), byte((startAddr>>16)&0xFF), byte((startAddr>>24)&0xFF),
			deviceCode, 0x00)
	} else {
		block = append(block, byte(startAddr&0xFF), byte((startAddr>>8)&0xFF), byte((startAddr>>16)&0xFF))
		block = append(block, deviceCode)
	}
	binary.LittleEndian.PutUint16(b, uint16(count))
	block = append(block, b...)
	block = append(block, payload...)

	frame := make([]byte, 0, 9+len(block))
	frame = append(frame, 0x50, 0x00, 0x00, 0xFF, 0xFF, 0x03, 0x00)
	binary.LittleEndian.PutUint16(b, uint16(len(block)))
	frame = append(frame, b...)
	frame = append(frame, block...)
	return frame
}

// mcCompletion reads the SLMP 3E response: 9-byte header, then the 2-byte
// completion code (little-endian), then the remaining data bytes. Returns the
// payload bytes after the completion code.
func mcCompletion(conn net.Conn, header []byte) ([]byte, error) {
	if header[0] != 0xD0 {
		return nil, fmt.Errorf("mc unexpected response: 0x%02x", header[0])
	}
	dataLen := int(binary.LittleEndian.Uint16(header[7:9]))
	if dataLen < 2 {
		return nil, fmt.Errorf("mc invalid response length: %d", dataLen)
	}
	// The length field counts the 2-byte completion code plus the data payload.
	rest := make([]byte, dataLen)
	if _, err := readFull(conn, rest); err != nil {
		return nil, fmt.Errorf("mc read body: %w", err)
	}
	completion := binary.LittleEndian.Uint16(rest[:2])
	if completion != 0 {
		return nil, fmt.Errorf("mc error: 0x%04x", completion)
	}
	return rest[2:], nil
}

func (d *MCDriver) mcReadWords(device string, startAddr int, count int) ([]uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, fmt.Errorf("not connected")
	}
	deviceCode, ok := mcDeviceCodes[strings.ToUpper(device)]
	if !ok {
		return nil, fmt.Errorf("unknown device: %s", device)
	}
	frame := mcBuildRequestSeries(0x0401, 0x0000, deviceCode, startAddr, count, nil, d.mcIsWideDevice())
	_ = d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(frame); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("mc write: %w", err)
	}
	respHeader := make([]byte, 9)
	if _, err := readFull(d.conn, respHeader); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("mc read header: %w", err)
	}
	data, err := mcCompletion(d.conn, respHeader)
	if err != nil {
		return nil, err
	}
	if len(data) < count*2 {
		return nil, fmt.Errorf("mc read returned %d bytes, need %d", len(data), count*2)
	}
	regs := make([]uint16, count)
	for i := 0; i < count; i++ {
		regs[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	return regs, nil
}

func (d *MCDriver) mcReadBit(device string, startAddr int) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return false, fmt.Errorf("not connected")
	}
	deviceCode, ok := mcDeviceCodes[strings.ToUpper(device)]
	if !ok {
		return false, fmt.Errorf("unknown device: %s", device)
	}
	frame := mcBuildRequestSeries(0x0401, 0x0001, deviceCode, startAddr, 1, nil, d.mcIsWideDevice())
	_ = d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(frame); err != nil {
		d.SetConnected(false)
		return false, fmt.Errorf("mc write bit: %w", err)
	}
	respHeader := make([]byte, 9)
	if _, err := readFull(d.conn, respHeader); err != nil {
		return false, fmt.Errorf("mc read bit header: %w", err)
	}
	data, err := mcCompletion(d.conn, respHeader)
	if err != nil {
		return false, err
	}
	if len(data) < 1 {
		return false, fmt.Errorf("mc read bit data: empty")
	}
	// Bit-unit responses pack two points per byte with the first point in the
	// high nibble, so a single-point read reports 0x10 for ON / 0x00 for OFF.
	return data[0]&0xF0 != 0, nil
}

func (d *MCDriver) mcWriteWords(device string, startAddr int, values []uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("not connected")
	}
	deviceCode, ok := mcDeviceCodes[strings.ToUpper(device)]
	if !ok {
		return fmt.Errorf("unknown device: %s", device)
	}
	count := len(values)
	payload := make([]byte, 0, count*2)
	for _, v := range values {
		payload = append(payload, byte(v&0xFF), byte((v>>8)&0xFF))
	}
	frame := mcBuildRequestSeries(0x1401, 0x0000, deviceCode, startAddr, count, payload, d.mcIsWideDevice())
	_ = d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(frame); err != nil {
		d.SetConnected(false)
		return fmt.Errorf("mc write: %w", err)
	}
	respHeader := make([]byte, 9)
	if _, err := readFull(d.conn, respHeader); err != nil {
		return fmt.Errorf("mc write resp: %w", err)
	}
	if _, err := mcCompletion(d.conn, respHeader); err != nil {
		return err
	}
	return nil
}

// mcWriteBit writes one bit of a bit-accessible device using the bit-unit
// write command (0x1401 / subcommand 0x0001).
func (d *MCDriver) mcWriteBit(device string, startAddr int, on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("not connected")
	}
	deviceCode, ok := mcDeviceCodes[strings.ToUpper(device)]
	if !ok {
		return fmt.Errorf("unknown device: %s", device)
	}
	payload := []byte{0x00}
	if on {
		payload[0] = 0x10 // first point of the packed byte lives in the high nibble
	}
	frame := mcBuildRequestSeries(0x1401, 0x0001, deviceCode, startAddr, 1, payload, d.mcIsWideDevice())
	_ = d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(frame); err != nil {
		d.SetConnected(false)
		return fmt.Errorf("mc write bit: %w", err)
	}
	respHeader := make([]byte, 9)
	if _, err := readFull(d.conn, respHeader); err != nil {
		return fmt.Errorf("mc write bit resp: %w", err)
	}
	if _, err := mcCompletion(d.conn, respHeader); err != nil {
		return err
	}
	return nil
}

// WritePointAtAddress writes to the point's MC device address ("D100"), which is
// what this driver parses; the operator's point name is not an address.
func (d *MCDriver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	return d.WritePoint(ctx, address, value)
}

func (d *MCDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	if !d.IsConnected() {
		return fmt.Errorf("mc not connected")
	}

	// Check circuit breaker before write
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	device, addr, err := parseMCAddress(point)
	if err != nil {
		return err
	}
	if b, ok := mcBoolValue(value); ok && isMCBitDevice(device) {
		if err := d.mcWriteBit(device, addr, b); err != nil {
			d.RecordWriteFailure()
			return err
		}
		d.RecordWriteSuccess()
		return nil
	}
	values, err := mcEncodeValue(value)
	if err != nil {
		return err
	}
	if err := d.mcWriteWords(device, addr, values); err != nil {
		d.RecordWriteFailure()
		return err
	}
	d.RecordWriteSuccess()
	return nil
}

// mcBoolValue reports whether a write value is an unambiguous boolean (a JSON
// boolean, or the strings "true"/"false" the write API can deliver).
func mcBoolValue(v interface{}) (bool, bool) {
	switch n := v.(type) {
	case bool:
		return n, true
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(n)); err == nil {
			return b, true
		}
	}
	return false, false
}

// mcEncodeValue renders a write value as the little-endian, low-word-first
// register list SLMP expects.
//
// WritePoint receives no data type, so the Go value decides the width: a
// float32, or a float carrying a fraction, is written as REAL (2 words) instead
// of being truncated to an integer, and an integer that does not fit 16 bits is
// written as DINT (2 words). Smaller values keep the legacy single-word write so
// existing INT callers are unaffected.
func mcEncodeValue(v interface{}) ([]uint16, error) {
	twoWords := func(bits uint32) []uint16 {
		return []uint16{uint16(bits & 0xFFFF), uint16(bits >> 16)}
	}
	switch n := v.(type) {
	case bool:
		return mcBoolWords(n), nil
	case string:
		if s := strings.TrimSpace(n); s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return mcEncodeValue(f)
			}
			if b, err := strconv.ParseBool(s); err == nil {
				return mcBoolWords(b), nil
			}
		}
		return []uint16{0}, nil
	case float32:
		return twoWords(math.Float32bits(n)), nil
	case float64:
		if n != math.Trunc(n) {
			return twoWords(math.Float32bits(float32(n))), nil
		}
		if n < math.MinInt16 || n > math.MaxInt16 {
			return twoWords(uint32(int64(n))), nil
		}
		return []uint16{uint16(int64(n))}, nil
	case int, int32, uint32, int64, uint64:
		i := toInt64(v)
		if i >= math.MinInt16 && i <= math.MaxInt16 {
			return []uint16{uint16(i)}, nil
		}
		return twoWords(uint32(i)), nil
	}
	return []uint16{uint16(toInt64(v))}, nil
}

func mcBoolWords(b bool) []uint16 {
	if b {
		return []uint16{1}
	}
	return []uint16{0}
}

func (d *MCDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *MCDriver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("not connected")
	}
	start := time.Now()
	_, err := d.mcReadWords("D", 0, 1)
	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		d.RecordReadFailure()
		return err
	}
	d.RecordReadSuccess(latencyMs)
	return nil
}

func parseMCAddress(addr string) (device string, address int, err error) {
	addr = strings.TrimSpace(strings.ToUpper(addr))
	if addr == "" {
		return "", 0, fmt.Errorf("empty mc address")
	}
	i := 0
	for i < len(addr) && addr[i] >= 'A' && addr[i] <= 'Z' {
		i++
	}
	if i == 0 || i > 2 {
		return "", 0, fmt.Errorf("invalid mc address: %s", addr)
	}
	device = addr[:i]
	addressStr := addr[i:]
	if addressStr == "" {
		return "", 0, fmt.Errorf("missing address number: %s", addr)
	}
	_, err = fmt.Sscanf(addressStr, "%d", &address)
	if err != nil {
		return "", 0, fmt.Errorf("invalid address number: %s", addressStr)
	}
	return device, address, nil
}

func isMCBitDevice(device string) bool {
	switch strings.ToUpper(device) {
	case "M", "X", "Y", "S", "B", "F":
		return true
	}
	return false
}

func decodeMCValue(regs []uint16, dataType string) interface{} {
	switch strings.ToLower(dataType) {
	case "int16", "short":
		if len(regs) >= 1 {
			return int16(regs[0])
		}
	case "uint16", "word":
		if len(regs) >= 1 {
			return regs[0]
		}
	case "int32", "long":
		if len(regs) >= 2 {
			return int32(uint32(regs[0]) | uint32(regs[1])<<16)
		}
	case "uint32", "dword":
		if len(regs) >= 2 {
			return uint32(regs[0]) | uint32(regs[1])<<16
		}
	case "float32", "float", "real":
		if len(regs) >= 2 {
			return float32FromBits(uint32(regs[0]) | uint32(regs[1])<<16)
		}
	case "float64", "double":
		if len(regs) >= 4 {
			bits := uint64(regs[0]) | uint64(regs[1])<<16 | uint64(regs[2])<<32 | uint64(regs[3])<<48
			return float64FromBits(bits)
		}
	case "bool":
		if len(regs) >= 1 {
			return regs[0] != 0
		}
	}
	if len(regs) >= 1 {
		return regs[0]
	}
	return nil
}
