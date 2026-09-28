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

// ABDriver implements an Allen-Bradley EtherNet/IP (CIP) protocol driver.
// Supports ControlLogix (tag-based) and MicroLogix (data table) addressing.
type ABDriver struct {
	BaseDriver
	host      string
	port      int
	plcType   string
	timeout   time.Duration
	conn      net.Conn
	mu        sync.Mutex
	sessionID uint32
	// tagTypes caches the CIP type code the device reported per tag. It is
	// guarded separately from mu because probing a tag takes the connection
	// lock, and a write must not deadlock against itself.
	tagTypes  map[string]uint16
	tagTypeMu sync.Mutex
}

func NewABDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	d := &ABDriver{
		host: GetConfigString(config, "host", "127.0.0.1"),
		// EtherNet/IP(CIP) is served on TCP 44818.
		port:    GetConfigInt(config, "port", constants.AllenBradleyDefaultPort),
		plcType: GetConfigString(config, "plc_type", "ControlLogix"),
		timeout: time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *ABDriver) Name() string { return "allen_bradley" }

func (d *ABDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		return nil
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.SetConnectionState(StateConnecting, "ab connecting")

	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	dialer := net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("ab connect %s: %w", addr, err)
	}
	d.conn = conn
	if err := d.registerSession(); err != nil {
		conn.Close()
		d.conn = nil
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("ab session register: %w", err)
	}
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "ab connected")
	return nil
}

// registerSession performs the EtherNet/IP session registration.
func (d *ABDriver) registerSession() error {
	// EtherNet/IP RegisterSession command is 0x0065 (not 0x0004); the 4-byte
	// payload is the option (socket binding) + protocol version (1 = CIP).
	frame := make([]byte, 28)
	binary.LittleEndian.PutUint16(frame[0:2], 0x0065)       // Register Session
	binary.LittleEndian.PutUint16(frame[2:4], 4)            // Length
	binary.LittleEndian.PutUint32(frame[4:8], 0)            // Session
	binary.LittleEndian.PutUint32(frame[8:12], 0)           // Status
	binary.LittleEndian.PutUint32(frame[20:24], 0)          // Options
	binary.LittleEndian.PutUint32(frame[24:28], 0x00000001) // Option: socket binding, version 1

	d.conn.SetDeadline(time.Now().Add(d.timeout))
	if _, err := d.conn.Write(frame); err != nil {
		return fmt.Errorf("ab register write: %w", err)
	}
	resp := make([]byte, 28)
	if _, err := readFull(d.conn, resp); err != nil {
		return fmt.Errorf("ab register read: %w", err)
	}
	status := binary.LittleEndian.Uint32(resp[8:12])
	if status != 0 {
		return fmt.Errorf("ab register status: 0x%08x", status)
	}
	d.sessionID = binary.LittleEndian.Uint32(resp[4:8])
	if d.sessionID == 0 {
		return fmt.Errorf("ab register returned session handle 0")
	}
	return nil
}

func (d *ABDriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		// Send Unregister Session (EtherNet/IP command 0x0066)
		frame := make([]byte, 24)
		binary.LittleEndian.PutUint16(frame[0:2], 0x0066) // Unregister
		binary.LittleEndian.PutUint16(frame[2:4], 0)
		binary.LittleEndian.PutUint32(frame[4:8], d.sessionID)
		d.conn.SetDeadline(time.Now().Add(d.timeout))
		d.conn.Write(frame)
		err := d.conn.Close()
		d.conn = nil
		d.SetConnected(false)
		return err
	}
	return nil
}

func (d *ABDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("ab not connected")
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
				WithError(err).Warn("AB read failed")
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

func (d *ABDriver) readPoint(pt models.PointDef) (interface{}, error) {
	tag := pt.Address
	if tag == "" {
		return nil, fmt.Errorf("empty tag address")
	}
	cipType, data, err := d.readTagListRaw(tag, 1)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("no data returned")
	}
	// The tag's CIP data type reported by the device is authoritative; the
	// configured data type is only a hint used when the device omits it.
	// Decoding by the configured name first made a REAL tag read through an
	// int16 point return the low two bytes of the float's bit pattern (40000.0
	// came back as 16384) with quality "good", so the whole AB write→read chain
	// disagreed with the controller.
	if v, ok := decodeCipValueByType(cipType, data); ok {
		return v, nil
	}
	if dataType := strings.ToLower(pt.DataType); dataType != "" && dataType != "auto" {
		if v, ok := decodeCipValue(dataType, data); ok {
			return v, nil
		}
	}
	return nil, fmt.Errorf("cannot decode AB tag %q (cip type 0x%02x, %d bytes)", tag, cipType, len(data))
}

// decodeCipValue decodes raw CIP value bytes for a configured data type name.
func decodeCipValue(dataType string, data []byte) (interface{}, bool) {
	u16 := func() uint16 {
		if len(data) >= 2 {
			return binary.LittleEndian.Uint16(data[:2])
		}
		return uint16(data[0])
	}
	switch dataType {
	case "bool":
		return data[0] != 0 || (len(data) > 1 && data[1] != 0), true
	case "int8", "sint":
		return int8(data[0]), true
	case "uint8", "byte", "usint":
		return uint8(data[0]), true
	case "int16", "short":
		return int16(u16()), true
	case "uint16", "word", "uint":
		return u16(), true
	case "int32", "long", "dint":
		if len(data) >= 4 {
			return int32(binary.LittleEndian.Uint32(data[:4])), true
		}
		return int32(int16(u16())), true
	case "uint32", "dword", "udint":
		if len(data) >= 4 {
			return binary.LittleEndian.Uint32(data[:4]), true
		}
		return uint32(u16()), true
	case "int64", "lint":
		if len(data) >= 8 {
			return int64(binary.LittleEndian.Uint64(data[:8])), true
		}
	case "uint64", "ulint":
		if len(data) >= 8 {
			return binary.LittleEndian.Uint64(data[:8]), true
		}
	case "float32", "float", "real":
		if len(data) >= 4 {
			return float32FromBits(binary.LittleEndian.Uint32(data[:4])), true
		}
	case "float64", "double", "lreal":
		if len(data) >= 8 {
			return math.Float64frombits(binary.LittleEndian.Uint64(data[:8])), true
		}
	case "string":
		return string(data), true
	}
	return nil, false
}

// decodeCipValueByType decodes raw CIP value bytes using the CIP data type
// code returned by the device.
func decodeCipValueByType(cipType uint16, data []byte) (interface{}, bool) {
	var name string
	switch cipType {
	case CipDataTypeBool:
		name = "bool"
	case CipDataTypeSInt:
		name = "int8"
	case CipDataTypeByte, CipDataTypeUSInt:
		name = "uint8"
	case CipDataTypeInt:
		name = "int16"
	case CipDataTypeWord, CipDataTypeUInt:
		name = "uint16"
	case CipDataTypeDInt:
		name = "int32"
	case CipDataTypeDWord, CipDataTypeUDInt:
		name = "uint32"
	case CipDataTypeLInt:
		name = "int64"
	case CipDataTypeLWord, CipDataTypeULInt:
		name = "uint64"
	case CipDataTypeReal:
		name = "float32"
	case CipDataTypeLReal:
		name = "float64"
	case CipDataTypeString, CipDataTypeStringSimulator:
		name = "string"
	default:
		return nil, false
	}
	return decodeCipValue(name, data)
}

// CipDataType* are the CIP atomic symbol data type codes carried in Read Tag
// responses and Write Tag requests (ODVA GS CIP table, as implemented by
// every Logix CPU and by reference clients such as pycomm3 / libplctag).
// These are wire values a controller answers with: a code that is off by one
// makes the gateway size the payload wrong and report a fabricated number as
// a good-quality reading.
const (
	CipDataTypeBool   uint16 = 0x00C1
	CipDataTypeSInt   uint16 = 0x00C2
	CipDataTypeInt    uint16 = 0x00C3
	CipDataTypeDInt   uint16 = 0x00C4
	CipDataTypeLInt   uint16 = 0x00C5
	CipDataTypeUSInt  uint16 = 0x00C6
	CipDataTypeUInt   uint16 = 0x00C7
	CipDataTypeUDInt  uint16 = 0x00C8
	CipDataTypeULInt  uint16 = 0x00C9
	CipDataTypeReal   uint16 = 0x00CA
	CipDataTypeLReal  uint16 = 0x00CB
	CipDataTypeString uint16 = 0x00D0
	CipDataTypeByte   uint16 = 0x00D1
	CipDataTypeWord   uint16 = 0x00D2
	CipDataTypeDWord  uint16 = 0x00D3
	CipDataTypeLWord  uint16 = 0x00D4
)

// CipDataTypeStringSimulator is the code the ProtoForge Allen-Bradley
// simulator answers for a string tag. No CIP controller uses it (0x00A0 is not
// an atomic data type), but reads from the simulator must not fail for it, so
// it is treated as a second, variable-length string form next to STRING.
const CipDataTypeStringSimulator uint16 = 0x00A0

// ansiTagPath encodes a symbolic tag as a CIP ANSI Extended Symbol path
// segment: 0x91, 8-bit name length, then the name padded to an even byte
// count. A trailing element (e.g. "Tag[3]" -> 0x28 0x03) is appended when the
// tag uses array syntax.
func ansiTagPath(tag string) []byte {
	name := tag
	var element int64 = -1
	if open := strings.LastIndex(tag, "["); open > 0 && strings.HasSuffix(tag, "]") {
		if n, err := strconv.ParseInt(tag[open+1:len(tag)-1], 10, 32); err == nil {
			element = n
			name = tag[:open]
		}
	}
	nameBytes := []byte(name)
	path := make([]byte, 0, 2+len(nameBytes)+2)
	path = append(path, 0x91, byte(len(nameBytes)))
	path = append(path, nameBytes...)
	if len(nameBytes)%2 != 0 {
		path = append(path, 0x00) // pad to an even number of bytes
	}
	if element >= 0 {
		path = append(path, 0x28, byte(element))
		if element > 255 {
			path = append(path, 0x29, byte(element>>8), byte(element>>16), byte(element>>24))
		} else {
			path = append(path, 0x00) // pad
		}
	}
	return path
}

// cipRequest builds a SendRRData frame carrying a single unconnected CIP
// service message (EtherNet/IP item structure: null address item +
// unconnected data item 0x00B2).
func (d *ABDriver) cipRequest(sessionID uint32, cip []byte) []byte {
	rrData := make([]byte, 0, 16+len(cip))
	rrData = append(rrData, 0x00, 0x00, 0x00, 0x00) // Interface handle
	rrData = append(rrData, 0x00, 0x00)             // Timeout
	rrData = append(rrData, 0x02, 0x00)             // Item count: 2
	rrData = append(rrData, 0x00, 0x00, 0x00, 0x00) // Null address item (type 0, length 0)
	rrData = append(rrData, 0xB2, 0x00)             // Unconnected data item type
	rrData = append(rrData, byte(len(cip)&0xFF), byte(len(cip)>>8))
	rrData = append(rrData, cip...)

	encapHeader := make([]byte, 24)
	binary.LittleEndian.PutUint16(encapHeader[0:2], 0x006F) // SendRRData
	binary.LittleEndian.PutUint16(encapHeader[2:4], uint16(len(rrData)))
	binary.LittleEndian.PutUint32(encapHeader[4:8], sessionID)
	return append(encapHeader, rrData...)
}

// parseCipResponse extracts the CIP payload from a SendRRData response.
// Payload layout (relative to the encapsulation header): interface handle (4)
// + timeout (2) + item count (2) + null address item (4) + unconnected data
// item header (4) = 16 bytes, then the CIP reply:
// service (1) + reserved (1) + status (2) + data type (2) + element count (2) + value.
func parseCipResponse(header, body []byte) (uint16, []byte, error) {
	if len(header) < 24 {
		return 0, nil, fmt.Errorf("ab cip response header too short")
	}
	if status := binary.LittleEndian.Uint32(header[8:12]); status != 0 {
		return 0, nil, fmt.Errorf("ab eip status: 0x%08x", status)
	}
	if len(body) < 20 {
		return 0, nil, fmt.Errorf("ab cip response too short: %d bytes", len(body))
	}
	cip := body[16:]
	if cip[1] != 0x00 || (cip[0]&0x80) == 0 {
		return 0, nil, fmt.Errorf("ab cip malformed response (service echo 0x%02x)", cip[0])
	}
	status := binary.LittleEndian.Uint16(cip[2:4])
	if status != 0 {
		return status, nil, fmt.Errorf("ab cip error: 0x%02x", status)
	}
	return 0, cip[4:], nil
}

// readTagListRaw performs a CIP Read Tag Service (0x4C) or Read Tag List
// service (0xAB) for a single tag and returns the raw value bytes together
// with the CIP data type code reported by the device.
func (d *ABDriver) readTagListRaw(tag string, count int) (uint16, []byte, error) {
	path := ansiTagPath(tag)
	cipReq := make([]byte, 0, 4+len(path)+2)
	cipReq = append(cipReq, 0x4C, byte(len(path)/2)) // Service: Read Tag, path size in words
	cipReq = append(cipReq, path...)
	cipReq = append(cipReq, byte(count&0xFF), byte(count>>8))

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return 0, nil, fmt.Errorf("not connected")
	}
	if err := d.roundTrip(d.cipRequest(d.sessionID, cipReq)); err != nil {
		d.SetConnected(false)
		return 0, nil, err
	}
	dataType, data, err := d.readCipReply()
	if err != nil {
		return 0, nil, err
	}
	return dataType, data, nil
}

// readCipReply reads one SendRRData response from the connection (call with
// d.mu held) and returns the CIP data type code and value bytes. Only the read
// services answer with a data section, so this is the read path's parser.
func (d *ABDriver) readCipReply() (uint16, []byte, error) {
	value, err := d.readCipPayload()
	if err != nil {
		return 0, nil, err
	}
	if len(value) < 2 {
		return 0, nil, fmt.Errorf("ab cip read reply carries no data type: %d bytes", len(value))
	}
	cipType := binary.LittleEndian.Uint16(value[:2])
	return cipType, splitCipReadValue(cipType, value[2:]), nil
}

// cipElementWidth is the number of bytes one element of a CIP type occupies in
// a reply payload. Zero means the type is variable-length, where the reply's own
// length field has to decide the layout.
func cipElementWidth(cipType uint16) int {
	switch cipType {
	case CipDataTypeBool, CipDataTypeSInt, CipDataTypeUSInt, CipDataTypeByte:
		return 1
	case CipDataTypeInt, CipDataTypeUInt, CipDataTypeWord:
		return 2
	case CipDataTypeDInt, CipDataTypeUDInt, CipDataTypeDWord, CipDataTypeReal:
		return 4
	case CipDataTypeLInt, CipDataTypeULInt, CipDataTypeLWord, CipDataTypeLReal:
		return 8
	default:
		return 0
	}
}

// splitCipReadValue returns the value bytes of a Read Tag data section, with or
// without the 2-byte element count that only real ControlLogix firmware carries.
// For a fixed-size type the two layouts differ by exactly that width, so the
// remaining length decides which one was sent. Strings carry their own length,
// which is matched against the payload instead of guessing an offset.
func splitCipReadValue(cipType uint16, rest []byte) []byte {
	width := cipElementWidth(cipType)
	if width == 0 {
		if cipType == CipDataTypeString || cipType == CipDataTypeStringSimulator {
			// Logix: count(2) + char length(2) + chars. Simulator: length + chars.
			if len(rest) >= 4 && int(binary.LittleEndian.Uint16(rest[2:4])) == len(rest)-4 {
				return rest[4:]
			}
			if len(rest) >= 2 && int(binary.LittleEndian.Uint16(rest[0:2])) == len(rest)-2 {
				return rest[2:]
			}
		}
		return rest
	}
	if len(rest) == width {
		return rest
	}
	if len(rest) >= width+2 {
		return rest[2:]
	}
	return rest
}

// readCipPayload reads one SendRRData response (call with d.mu held) and
// returns the bytes after the 4-byte CIP reply header. A successful Write Tag
// reply carries nothing there, so write-only callers must not demand a payload.
func (d *ABDriver) readCipPayload() ([]byte, error) {
	respHeader := make([]byte, 24)
	if _, err := readFull(d.conn, respHeader); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("ab cip read header: %w", err)
	}
	respLen := int(binary.LittleEndian.Uint16(respHeader[2:4]))
	if respLen < 6 {
		return nil, fmt.Errorf("ab cip short response: %d", respLen)
	}
	if respLen > 1024*1024 {
		return nil, fmt.Errorf("ab cip response too large: %d bytes", respLen)
	}
	respBody := make([]byte, respLen)
	if _, err := readFull(d.conn, respBody); err != nil {
		d.SetConnected(false)
		return nil, fmt.Errorf("ab cip read body: %w", err)
	}
	_, value, err := parseCipResponse(respHeader, respBody)
	if err != nil {
		return nil, err
	}
	return value, nil
}

// writeTagRaw performs a CIP Write Tag Service (0x4D) for a single tag.
func (d *ABDriver) writeTagRaw(tag string, cipType uint16, data []byte) error {
	path := ansiTagPath(tag)
	cipReq := make([]byte, 0, 4+len(path)+4+len(data))
	cipReq = append(cipReq, 0x4D, byte(len(path)/2)) // Service: Write Tag, path size in words
	cipReq = append(cipReq, path...)
	cipReq = append(cipReq, byte(cipType&0xFF), byte(cipType>>8)) // Tag data type
	if cipType == CipDataTypeString || cipType == CipDataTypeStringSimulator {
		// Strings are the exception: the word after the type code is the byte
		// length of the character data, not an element count.
		cipReq = append(cipReq, byte(len(data)&0xFF), byte(len(data)>>8))
	} else {
		// For a scalar the word after the type code is the element count. Putting
		// the payload length there asks a real CPU for two INT elements and it
		// either rejects the frame or writes half the value.
		cipReq = append(cipReq, 0x01, 0x00)
	}
	cipReq = append(cipReq, data...)

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("not connected")
	}
	if err := d.roundTrip(d.cipRequest(d.sessionID, cipReq)); err != nil {
		d.SetConnected(false)
		return err
	}
	_, err := d.readCipPayload()
	return err
}

// roundTrip writes a complete EtherNet/IP frame honouring the connection
// deadline and any concurrent reader on the same connection.
func (d *ABDriver) roundTrip(frame []byte) error {
	if err := d.conn.SetDeadline(time.Now().Add(d.timeout)); err != nil {
		return fmt.Errorf("ab cip set deadline: %w", err)
	}
	if _, err := d.conn.Write(frame); err != nil {
		return fmt.Errorf("ab cip write: %w", err)
	}
	return nil
}

// WritePointAtAddress writes to the point's CIP tag address ("Program:Main.Tag"),
// which is what this driver parses; the operator's point name is not a tag.
func (d *ABDriver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	return d.WritePoint(ctx, address, value)
}

func (d *ABDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	if !d.IsConnected() {
		return fmt.Errorf("ab not connected")
	}

	// Check circuit breaker before write
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	tag := point
	if tag == "" {
		return fmt.Errorf("empty tag address")
	}

	// Lay the value out for the width the tag actually has. The Go type of the
	// incoming value says nothing about the controller: a value that happens to
	// fit an int16 sent to a DINT tag is read back as zero by a simulator and
	// rejected by real firmware, and either way the operator was told "written".
	cipType, writeData, err := d.writePayload(tag, value)
	if err != nil {
		d.RecordWriteFailure()
		return err
	}

	if err := d.writeTagRaw(tag, cipType, writeData); err != nil {
		d.RecordWriteFailure()
		return err
	}
	d.RecordWriteSuccess()
	return nil
}

// tagCipType asks the device what type a tag carries and remembers the answer.
// The Read Tag reply carries the type code, so the controller itself is the
// authority on how a write must be laid out.
func (d *ABDriver) tagCipType(tag string) (uint16, bool) {
	d.tagTypeMu.Lock()
	defer d.tagTypeMu.Unlock()
	if t, ok := d.tagTypes[tag]; ok {
		return t, t != 0
	}
	t, _, err := d.readTagListRaw(tag, 1)
	if err != nil || t == 0 {
		return 0, false
	}
	if d.tagTypes == nil {
		d.tagTypes = make(map[string]uint16, 8)
	}
	d.tagTypes[tag] = t
	return t, true
}

// writePayload picks the CIP type and encoded bytes for one write. When the
// device would not name a type it falls back to the Go type of the value --
// that is a guess, and only used for stacks that answer reads without a type.
func (d *ABDriver) writePayload(tag string, value interface{}) (uint16, []byte, error) {
	cipType, known := d.tagCipType(tag)
	if !known {
		t, data := inferABWrite(value)
		if data == nil {
			return 0, nil, fmt.Errorf("unsupported value type for AB write: %T", value)
		}
		return t, data, nil
	}
	data, err := encodeCipValue(cipType, value)
	if err != nil {
		return 0, nil, err
	}
	return cipType, data, nil
}

// inferABWrite encodes by the Go type alone, the pre-existing behaviour.
func inferABWrite(value interface{}) (uint16, []byte) {
	switch v := value.(type) {
	case bool:
		if v {
			return CipDataTypeBool, []byte{0x01, 0x00}
		}
		return CipDataTypeBool, []byte{0x00, 0x00}
	case int:
		n := toInt64(value)
		if n >= math.MinInt16 && n <= math.MaxInt16 {
			return CipDataTypeInt, []byte{byte(n), byte(n >> 8)}
		}
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, uint32(n))
		return CipDataTypeDInt, buf
	case int16:
		return CipDataTypeInt, []byte{byte(v), byte(v >> 8)}
	case uint16:
		return CipDataTypeUInt, []byte{byte(v), byte(v >> 8)}
	case int32, int64:
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, uint32(toInt64(value)))
		return CipDataTypeDInt, buf
	case uint32, uint64:
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, uint32(toInt64(value)))
		return CipDataTypeUDInt, buf
	case float32:
		f, _ := toFloat64(value)
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, math.Float32bits(float32(f)))
		return CipDataTypeReal, buf
	case float64:
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, math.Float64bits(v))
		return CipDataTypeLReal, buf
	case string:
		return CipDataTypeString, []byte(v)
	default:
		return 0, nil
	}
}

// encodeCipValue lays a Go value out for one CIP symbolic type. A value that
// does not fit its tag is an error: silently wrapping 70000 into an INT tag
// would put -32767 on the machine and report success.
func encodeCipValue(cipType uint16, value interface{}) ([]byte, error) {
	if cipType == CipDataTypeString || cipType == CipDataTypeStringSimulator {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("ab string tag needs a string value, got %T", value)
		}
		return []byte(s), nil
	}
	if cipType == CipDataTypeBool {
		switch v := value.(type) {
		case bool:
			if v {
				return []byte{0x01, 0x00}, nil
			}
			return []byte{0x00, 0x00}, nil
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "true", "1", "on":
				return []byte{0x01, 0x00}, nil
			case "false", "0", "off":
				return []byte{0x00, 0x00}, nil
			}
		}
		n, ok := toInt64Value(value)
		if !ok {
			return nil, fmt.Errorf("ab bool tag needs a boolean value, got %T", value)
		}
		if n != 0 {
			return []byte{0x01, 0x00}, nil
		}
		return []byte{0x00, 0x00}, nil
	}

	f, ok := toFloat64(value)
	if !ok {
		return nil, fmt.Errorf("ab tag of CIP type 0x%04x needs a numeric value, got %T", cipType, value)
	}
	signed := func(bits int) error {
		lim := int64(1) << (bits - 1)
		if f < float64(-lim) || f > float64(lim-1) {
			return fmt.Errorf("ab write %.0f overflows a %d-bit tag", f, bits)
		}
		return nil
	}
	unsigned := func(bits int) error {
		if bits >= 64 {
			// 2^64 is not representable as an int64 shift; the float bound is exact.
			if f < 0 || f >= 1.8446744073709552e19 {
				return fmt.Errorf("ab write %v does not fit an unsigned %d-bit tag", f, bits)
			}
			return nil
		}
		if f < 0 || f >= float64(uint64(1)<<uint(bits)) {
			return fmt.Errorf("ab write %.0f does not fit an unsigned %d-bit tag", f, bits)
		}
		return nil
	}
	switch cipType {
	case CipDataTypeSInt, CipDataTypeUSInt, CipDataTypeByte:
		if err := wholeNumber(f, cipType); err != nil {
			return nil, err
		}
		if cipType == CipDataTypeSInt {
			if err := signed(8); err != nil {
				return nil, err
			}
		} else if err := unsigned(8); err != nil {
			return nil, err
		}
		// 8-bit elements travel in the low byte of a CIP word.
		return []byte{byte(int64(f)), 0x00}, nil
	case CipDataTypeInt:
		if err := wholeNumber(f, cipType); err != nil {
			return nil, err
		}
		if err := signed(16); err != nil {
			return nil, err
		}
		n := int16(f)
		return []byte{byte(n), byte(n >> 8)}, nil
	case CipDataTypeWord, CipDataTypeUInt:
		if err := wholeNumber(f, cipType); err != nil {
			return nil, err
		}
		if err := unsigned(16); err != nil {
			return nil, err
		}
		n := uint16(f)
		return []byte{byte(n), byte(n >> 8)}, nil
	case CipDataTypeDInt:
		if err := wholeNumber(f, cipType); err != nil {
			return nil, err
		}
		if err := signed(32); err != nil {
			return nil, err
		}
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, uint32(int32(f)))
		return buf, nil
	case CipDataTypeDWord, CipDataTypeUDInt:
		if err := wholeNumber(f, cipType); err != nil {
			return nil, err
		}
		if err := unsigned(32); err != nil {
			return nil, err
		}
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, uint32(f))
		return buf, nil
	case CipDataTypeLInt:
		if err := wholeNumber(f, cipType); err != nil {
			return nil, err
		}
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, uint64(int64(f)))
		return buf, nil
	case CipDataTypeLWord, CipDataTypeULInt:
		if err := wholeNumber(f, cipType); err != nil {
			return nil, err
		}
		if err := unsigned(64); err != nil {
			return nil, err
		}
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, uint64(f))
		return buf, nil
	case CipDataTypeReal:
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, math.Float32bits(float32(f)))
		return buf, nil
	case CipDataTypeLReal:
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, math.Float64bits(f))
		return buf, nil
	default:
		return nil, fmt.Errorf("ab write: unsupported CIP data type 0x%04x", cipType)
	}
}

// wholeNumber rejects a fractional value aimed at an integer tag rather than
// rounding it away from what the operator typed.
func wholeNumber(f float64, cipType uint16) error {
	if f != math.Trunc(f) {
		return fmt.Errorf("ab tag of CIP type 0x%04x holds whole numbers, got %v", cipType, f)
	}
	return nil
}

func (d *ABDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *ABDriver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("not connected")
	}
	// Read a known tag as health check. The tag is configurable because the
	// set of served tags is device specific; without one, connection liveness
	// (verified by the read round trip below being skipped) is reported.
	healthTag := ""
	if cfg := d.GetConfig(); cfg != nil {
		healthTag = GetConfigString(cfg, "health_check_tag", "")
	}
	if healthTag == "" {
		// Nothing was read from the device, so there is no round trip to record.
		d.RecordReadSuccess(LatencyNotMeasured)
		return nil
	}
	start := time.Now()
	_, _, err := d.readTagListRaw(healthTag, 1)
	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		d.RecordReadFailure()
		return err
	}
	d.RecordReadSuccess(latencyMs)
	return nil
}
