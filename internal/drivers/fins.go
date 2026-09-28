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

// FINS area codes
const (
	finsAreaCIO = 0xB0
	finsAreaWR  = 0xB1
	finsAreaHR  = 0xB2
	finsAreaAR  = 0xB3
	finsAreaDM  = 0x82
)

type FINSDriver struct {
	BaseDriver
	host    string
	port    int
	node    int
	unit    int
	timeout time.Duration
	conn    net.Conn
	mu      sync.Mutex
}

func NewFINSDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	d := &FINSDriver{
		host:    GetConfigString(config, "host", "127.0.0.1"),
		port:    GetConfigInt(config, "port", 9600),
		node:    GetConfigInt(config, "node", 0),
		unit:    GetConfigInt(config, "unit", 0),
		timeout: time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *FINSDriver) Name() string { return "omron_fins" }

func (d *FINSDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		return nil
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.SetConnectionState(StateConnecting, "fins connecting")

	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	dialer := net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("fins connect %s: %w", addr, err)
	}
	d.conn = conn
	if err := d.finsTCPHandshake(); err != nil {
		conn.Close()
		d.conn = nil
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("fins handshake: %w", err)
	}
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "fins connected")
	return nil
}

func (d *FINSDriver) finsTCPHandshake() error {
	// FINS/TCP node-address negotiation. Body = Command(4)=0x00000000 +
	// ErrorCode(4)=0 + client node(4). Wrapped in the standard FINS/TCP header
	// ("FINS" magic + big-endian body length), which the real PLC (and the
	// ProtoForge simulator) require on every message.
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], 0x00000000) // command: node address send
	binary.BigEndian.PutUint32(body[4:8], 0x00000000) // error code
	binary.BigEndian.PutUint32(body[8:12], uint32(d.node))
	_ = d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(finsTCPFrame(body)); err != nil {
		return fmt.Errorf("fins handshake write: %w", err)
	}
	resp, err := finsTCPRead(d.conn)
	if err != nil {
		return fmt.Errorf("fins handshake read: %w", err)
	}
	if len(resp) < 8 {
		return fmt.Errorf("fins handshake response too short")
	}
	if cmd := binary.BigEndian.Uint32(resp[0:4]); cmd != 0x00000001 {
		return fmt.Errorf("fins unexpected handshake command: 0x%08x", cmd)
	}
	if ec := binary.BigEndian.Uint32(resp[4:8]); ec != 0 {
		return fmt.Errorf("fins handshake error code: 0x%08x", ec)
	}
	return nil
}

// finsTCPFrame prefixes a FINS/TCP message body with the 4-byte "FINS" magic
// and a big-endian length of the body.
func finsTCPFrame(body []byte) []byte {
	frame := make([]byte, 8+len(body))
	copy(frame[0:4], "FINS")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(body)))
	copy(frame[8:], body)
	return frame
}

// finsTCPRead reads one FINS/TCP message and returns its body (after the magic
// and length header).
func finsTCPRead(conn net.Conn) ([]byte, error) {
	header := make([]byte, 8)
	if _, err := readFull(conn, header); err != nil {
		return nil, err
	}
	if string(header[0:4]) != "FINS" {
		return nil, fmt.Errorf("fins invalid magic: %q", header[0:4])
	}
	bodyLen := int(binary.BigEndian.Uint32(header[4:8]))
	body := make([]byte, bodyLen)
	if bodyLen > 0 {
		if _, err := readFull(conn, body); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// finsFrameHeader returns the standard 10-byte FINS network header
// (ICF/RSV/GTC/DNA/DA1/DA2/SNA/SA1/SA2/SID).
func (d *FINSDriver) finsFrameHeader() []byte {
	return []byte{0x80, 0x00, 0x02, 0x00, byte(d.node), byte(d.unit), 0x00, 0x00, 0x00, 0x00}
}

func (d *FINSDriver) Disconnect() error {
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

func (d *FINSDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("fins not connected")
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
				WithField("point", pt.Name).Warn("FINS read failed")
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

func (d *FINSDriver) readPoint(pt models.PointDef) (interface{}, error) {
	area, offset, bitOffset, isBit, err := parseFINSAddress(pt.Address)
	if err != nil {
		return nil, err
	}
	dataType := finsResolveDataType(pt.Address, pt.DataType)
	readSize := finsWordCount(dataType)
	if isBit {
		dataType = "bool"
		readSize = 1
	}
	data, err := d.finsRead(area, offset, readSize)
	if err != nil {
		return nil, err
	}
	if len(data) < readSize*2 {
		return nil, fmt.Errorf("fins read returned %d bytes, need %d", len(data), readSize*2)
	}

	if dataType == "bool" {
		word := binary.BigEndian.Uint16(data[:2])
		if isBit {
			// Bit access addresses (D20.3) select a single bit of the word.
			return (word>>uint(bitOffset))&0x01 != 0, nil
		}
		// ProtoForge stores a plain bool point as a full word holding 0/1.
		return word != 0, nil
	}

	switch dataType {
	case "int16", "short":
		return int16(binary.BigEndian.Uint16(data[:2])), nil
	case "uint16", "word":
		return binary.BigEndian.Uint16(data[:2]), nil
	case "int32", "long", "dint":
		return int32(binary.BigEndian.Uint32(data[:4])), nil
	case "uint32", "dword":
		return binary.BigEndian.Uint32(data[:4]), nil
	case "float32", "float", "real":
		return float32FromBits(binary.BigEndian.Uint32(data[:4])), nil
	case "float64", "double":
		return float64FromBits(binary.BigEndian.Uint64(data[:8])), nil
	}
	// Unknown type: FINS words are big-endian 16-bit, keep the legacy default.
	return int16(binary.BigEndian.Uint16(data[:2])), nil
}

// finsTypeHint extracts the ProtoForge data-type suffix from a FINS address
// ("D2,r" -> "r"). The integration always appends it, so it is authoritative.
func finsTypeHint(addr string) string {
	_, hint, found := strings.Cut(strings.TrimSpace(addr), ",")
	if !found {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(hint))
}

// finsResolveDataType maps an address (plus its optional data-type suffix) and
// the point's declared data type onto the type used for encoding/decoding.
// Suffix wins because it is what the FINS wire format was chosen by, and because
// the driver's read/write helpers only receive the address string.
func finsResolveDataType(addr string, declared string) string {
	dt := strings.ToLower(strings.TrimSpace(declared))
	switch finsTypeHint(addr) {
	case "r":
		if dt == "float64" || dt == "double" {
			return "float64"
		}
		return "float32"
	case "i":
		return "int16"
	case "w":
		return "uint16"
	case "b":
		return "bool"
	case "dw":
		if dt == "uint32" {
			return "uint32"
		}
		return "int32"
	}
	return dt
}

// finsWordCount returns how many 16-bit FINS words hold a value of dataType.
func finsWordCount(dataType string) int {
	switch dataType {
	case "int32", "uint32", "dint", "dword", "long", "float32", "float", "real":
		return 2
	case "float64", "double":
		return 4
	default:
		return 1
	}
}

// finsEncodeValue renders a value as the big-endian bytes a FINS memory-area
// write expects, sized for dataType (2 words for REAL/DINT, 4 for LREAL).
func finsEncodeValue(dataType string, v interface{}) ([]byte, error) {
	buf := make([]byte, finsWordCount(dataType)*2)
	// The narrowing conversions below wrap, so a value the declared type cannot
	// hold would reach the PLC as a different number and count as a write that
	// succeeded.
	if err := checkWriteInRange(v, dataType); err != nil {
		return nil, err
	}
	switch dataType {
	case "int32", "long", "dint":
		binary.BigEndian.PutUint32(buf, uint32(finsToInt64(v)))
	case "uint32", "dword":
		binary.BigEndian.PutUint32(buf, uint32(finsToInt64(v)))
	case "float32", "float", "real":
		binary.BigEndian.PutUint32(buf, math.Float32bits(float32(finsToFloat(v))))
	case "float64", "double":
		binary.BigEndian.PutUint64(buf, math.Float64bits(finsToFloat(v)))
	case "bool":
		if finsToBool(v) {
			binary.BigEndian.PutUint16(buf, 1)
		}
	case "uint16", "word", "int16", "short", "":
		binary.BigEndian.PutUint16(buf, uint16(finsToInt64(v)))
	default:
		return nil, fmt.Errorf("fins: unsupported data type %q", dataType)
	}
	return buf, nil
}

// finsToInt64 coerces a write value, including the JSON strings the write API
// can deliver, to an integer.
func finsToInt64(v interface{}) int64 {
	if s, ok := v.(string); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return int64(f)
		}
		if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil && b {
			return 1
		}
	}
	return toInt64(v)
}

// finsToFloat coerces a write value to float64 (JSON strings included).
func finsToFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float32:
		return float64(n)
	case float64:
		return n
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f
		}
	}
	return float64(toInt64(v))
}

// finsToBool coerces a write value to a bool: JSON booleans, numbers and the
// strings "true"/"1" all read as true.
func finsToBool(v interface{}) bool {
	switch n := v.(type) {
	case bool:
		return n
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(n)); err == nil {
			return b
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f != 0
		}
		return false
	}
	return toInt64(v) != 0
}

func (d *FINSDriver) finsRead(area int, offset int, count int) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, fmt.Errorf("not connected")
	}
	// FINS core frame: 10-byte header + MRC(0x01)/SRC(0x01) memory-area read +
	// area(1) + word address(2 BE) + bit address(1) + word count(2 BE).
	fins := d.finsFrameHeader()
	fins = append(fins, 0x01, 0x01, byte(area))
	fins = append(fins, byte(offset>>8), byte(offset&0xFF), 0x00)
	fins = append(fins, byte(count>>8), byte(count&0xFF))

	// FINS/TCP send-frame command (0x00000002): Command(4)+ErrorCode(4)+frame.
	body := make([]byte, 8+len(fins))
	binary.BigEndian.PutUint32(body[0:4], 0x00000002)
	copy(body[8:], fins)

	_ = d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(finsTCPFrame(body)); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("fins write: %w", err)
	}
	resp, err := finsTCPRead(d.conn)
	_ = d.conn.SetDeadline(time.Time{})
	if err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("fins read: %w", err)
	}
	if len(resp) < 8 {
		return nil, fmt.Errorf("fins response too short")
	}
	if ec := binary.BigEndian.Uint32(resp[4:8]); ec != 0 {
		return nil, fmt.Errorf("fins transport error: 0x%08x", ec)
	}
	frame := resp[8:] // FINS response frame
	if len(frame) < 14 {
		return nil, fmt.Errorf("fins response frame too short")
	}
	if endCode := binary.BigEndian.Uint16(frame[12:14]); endCode != 0x0000 {
		return nil, fmt.Errorf("fins error: 0x%04x", endCode)
	}
	data := frame[14:]
	need := count * 2
	if len(data) < need {
		return nil, fmt.Errorf("fins data too short")
	}
	return data[:need], nil
}

// WritePointAtAddress writes to the point's FINS address ("D100"), which is what
// this driver parses; the operator's point name is not an address. The declared
// data type rides along because a FINS DM word holds any of several widths, and
// guessing from the JSON number wrote a uint16 43690 as an int16.
func (d *FINSDriver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	return d.writePoint(ctx, address, value, dataType)
}

func (d *FINSDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	return d.writePoint(ctx, point, value, "")
}

func (d *FINSDriver) writePoint(ctx context.Context, point string, value interface{}, declared string) error {
	if !d.IsConnected() {
		return fmt.Errorf("fins not connected")
	}

	// Check circuit breaker before write
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	area, offset, bitOffset, isBit, err := parseFINSAddress(point)
	if err != nil {
		return err
	}

	var writeData []byte
	if isBit {
		// Bit writes must not clobber the rest of the word: read the owning word,
		// toggle the addressed bit and write it back.
		cur, err := d.finsRead(area, offset, 1)
		if err != nil {
			d.RecordWriteFailure()
			return err
		}
		word := binary.BigEndian.Uint16(cur[:2])
		if finsToBool(value) {
			word |= 1 << uint(bitOffset)
		} else {
			word &^= 1 << uint(bitOffset)
		}
		writeData = []byte{byte(word >> 8), byte(word & 0xFF)}
	} else {
		dataType := finsResolveDataType(point, declared)
		if dataType == "" {
			dataType = finsDefaultWriteType(value)
		}
		writeData, err = finsEncodeValue(dataType, value)
		if err != nil {
			return err
		}
	}

	if err := d.finsWrite(area, offset, writeData); err != nil {
		d.RecordWriteFailure()
		return err
	}
	d.RecordWriteSuccess()
	return nil
}

// finsDefaultWriteType picks a wire type when the address carries no data-type
// suffix. A float32 value is unambiguous; a float carrying a fraction can only
// be a REAL field (truncating it to an integer, as the legacy code did, wrote
// 12 into a point holding 12.5); everything else keeps the 16-bit behaviour.
func finsDefaultWriteType(v interface{}) string {
	switch n := v.(type) {
	case float32:
		return "float32"
	case bool:
		return "bool"
	case float64:
		if n != math.Trunc(n) {
			return "float32"
		}
	}
	return "int16"
}

func (d *FINSDriver) finsWrite(area int, offset int, data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("not connected")
	}
	count := len(data) / 2
	// FINS core frame: header + MRC(0x01)/SRC(0x02) memory-area write +
	// area(1) + word address(2 BE) + bit(1) + word count(2 BE) + data.
	fins := d.finsFrameHeader()
	fins = append(fins, 0x01, 0x02, byte(area))
	fins = append(fins, byte(offset>>8), byte(offset&0xFF), 0x00)
	fins = append(fins, byte(count>>8), byte(count&0xFF))
	fins = append(fins, data...)

	body := make([]byte, 8+len(fins))
	binary.BigEndian.PutUint32(body[0:4], 0x00000002)
	copy(body[8:], fins)

	_ = d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(finsTCPFrame(body)); err != nil {
		d.SetConnected(false)
		return fmt.Errorf("fins write: %w", err)
	}
	resp, err := finsTCPRead(d.conn)
	_ = d.conn.SetDeadline(time.Time{})
	if err != nil {
		d.SetConnected(false)
		return fmt.Errorf("fins write resp: %w", err)
	}
	if len(resp) < 8 {
		return fmt.Errorf("fins write response too short")
	}
	frame := resp[8:]
	if len(frame) >= 14 {
		if endCode := binary.BigEndian.Uint16(frame[12:14]); endCode != 0x0000 {
			return fmt.Errorf("fins write error: 0x%04x", endCode)
		}
	}
	return nil
}

func (d *FINSDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *FINSDriver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("not connected")
	}
	start := time.Now()
	_, err := d.finsRead(finsAreaCIO, 0, 1)
	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		d.RecordReadFailure()
		return err
	}
	d.RecordReadSuccess(latencyMs)
	return nil
}

func parseFINSAddress(addr string) (area int, offset int, bitOffset int, isBit bool, err error) {
	addr = strings.TrimSpace(strings.ToUpper(addr))
	if addr == "" {
		return 0, 0, 0, false, fmt.Errorf("empty fins address")
	}
	// Strip the optional data-type suffix the integrations append
	// ("D2,r", "D20.0,b"); finsResolveDataType turns it back into a wire type.
	if comma := strings.IndexByte(addr, ','); comma >= 0 {
		addr = strings.TrimSpace(addr[:comma])
	}
	switch {
	case strings.HasPrefix(addr, "CIO"):
		area = finsAreaCIO
		addr = strings.TrimPrefix(addr, "CIO")
	case strings.HasPrefix(addr, "D"):
		area = finsAreaDM
		addr = strings.TrimPrefix(addr, "D")
	case strings.HasPrefix(addr, "W"):
		area = finsAreaWR
		addr = strings.TrimPrefix(addr, "W")
	case strings.HasPrefix(addr, "H"):
		area = finsAreaHR
		addr = strings.TrimPrefix(addr, "H")
	case strings.HasPrefix(addr, "A"):
		area = finsAreaAR
		addr = strings.TrimPrefix(addr, "A")
	default:
		return 0, 0, 0, false, fmt.Errorf("unknown fins area: %s", addr)
	}
	parts := strings.SplitN(addr, ".", 2)
	_, err = fmt.Sscanf(parts[0], "%d", &offset)
	if err != nil {
		return 0, 0, 0, false, fmt.Errorf("invalid fins offset: %s", parts[0])
	}
	if len(parts) > 1 {
		_, err = fmt.Sscanf(parts[1], "%d", &bitOffset)
		if err != nil || bitOffset < 0 || bitOffset > 15 {
			return 0, 0, 0, false, fmt.Errorf("invalid bit: %s", parts[1])
		}
		isBit = true
	}
	return area, offset, bitOffset, isBit, nil
}
