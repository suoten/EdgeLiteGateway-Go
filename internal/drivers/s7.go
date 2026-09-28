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

// S7 area codes
const (
	s7AreaPE = 0x81 // Process inputs (I/E)
	s7AreaPA = 0x82 // Process outputs (Q/A)
	s7AreaMK = 0x83 // Merker (M)
	s7AreaDB = 0x84 // Data blocks (DB)
	s7AreaCT = 0x1C // Counters (C)
	s7AreaTM = 0x1D // Timers (T)
)

// S7 data types
const (
	s7TypeBit   = 'X'
	s7TypeByte  = 'B'
	s7TypeWord  = 'W'
	s7TypeDWord = 'D'
)

// S7Driver implements a Siemens S7 protocol driver over ISO-on-TCP (RFC1006).
// Supports S7-200/300/400/1200/1500 PLCs via raw TCP socket.
type S7Driver struct {
	BaseDriver
	host    string
	port    int
	rack    int
	slot    int
	timeout time.Duration
	conn    net.Conn
	mu      sync.Mutex
	// local TSAP bytes for ISO-on-TCP handshake
	localTSAP  []byte
	remoteTSAP []byte
	// connection state for ISO-on-TCP
	isoConnected bool
}

// NewS7Driver creates a new S7Driver.
func NewS7Driver(deviceID string, config map[string]interface{}) (Driver, error) {
	// S7 tooling (and the ProtoForge integration) spells the endpoint "ip";
	// accept both keys so a pushed device config targets the right host.
	host := GetConfigString(config, "host", "")
	if host == "" {
		host = GetConfigString(config, "ip", "127.0.0.1")
	}
	d := &S7Driver{
		host:    host,
		port:    GetConfigInt(config, "port", 102),
		rack:    GetConfigInt(config, "rack", 0),
		slot:    GetConfigInt(config, "slot", 2),
		timeout: time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *S7Driver) Name() string { return "siemens_s7" }

// Connect establishes the ISO-on-TCP + S7 connection.
func (d *S7Driver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		return nil
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.SetConnectionState(StateConnecting, "s7 connecting")

	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	dialer := net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("s7 connect %s: %w", addr, err)
	}
	d.conn = conn

	// Perform ISO-on-TCP COTP handshake
	if err := d.isoHandshake(); err != nil {
		conn.Close()
		d.conn = nil
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("s7 iso handshake: %w", err)
	}

	// Perform S7 communication setup
	if err := d.s7Setup(); err != nil {
		conn.Close()
		d.conn = nil
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("s7 setup: %w", err)
	}

	d.isoConnected = true
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "s7 connected")
	logrus.WithField("device_id", d.DeviceID()).Debug("S7 connected")
	return nil
}

// isoHandshake performs the ISO-on-TCP (RFC1006) COTP connection request.
func (d *S7Driver) isoHandshake() error {
	// Calculate TSAPs
	// Local (calling) TSAP: 0x0100
	// Remote (called) TSAP: 0x01 + (rack*0x20 + slot)
	d.localTSAP = []byte{0x01, 0x00}
	d.remoteTSAP = []byte{0x01, byte(d.rack*0x20 + d.slot)}

	// COTP CR options: 0xC1 Calling TSAP, 0xC2 Called TSAP.
	opts := []byte{0xC1, byte(len(d.localTSAP))}
	opts = append(opts, d.localTSAP...)
	opts = append(opts, 0xC2, byte(len(d.remoteTSAP)))
	opts = append(opts, d.remoteTSAP...)

	// Fixed CR part counted by the length indicator: PDU type(0xE0) +
	// dst-ref(2) + src-ref(2) + class(1) = 6 bytes, plus the options.
	cotp := []byte{0xE0, 0x00, 0x00, 0x00, 0x00, 0x00}
	cotp = append(cotp, opts...)
	li := byte(len(cotp)) // LI = bytes from PDU type to end of COTP

	total := 4 + 1 + len(cotp) // TPKT header + LI + COTP
	tpkt := []byte{0x03, 0x00, byte(total >> 8), byte(total & 0xFF), li}
	tpkt = append(tpkt, cotp...)

	if err := tcpWrite(d.conn, tpkt, d.timeout); err != nil {
		return fmt.Errorf("iso write: %w", err)
	}

	// Read CC (Connection Confirm) response
	resp, err := tcpReadExact(d.conn, 4, d.timeout) // TPKT header
	if err != nil {
		return fmt.Errorf("iso read tpkt: %w", err)
	}
	if resp[0] != 0x03 || resp[1] != 0x00 {
		return fmt.Errorf("invalid TPKT header: %x %x", resp[0], resp[1])
	}
	tpktLen := int(binary.BigEndian.Uint16(resp[2:4]))
	if tpktLen < 4 {
		return fmt.Errorf("invalid TPKT length: %d", tpktLen)
	}
	body, err := tcpReadExact(d.conn, tpktLen-4, d.timeout)
	if err != nil {
		return fmt.Errorf("iso read body: %w", err)
	}
	// body[0] = COTP LI, body[1] = PDU type (0xD0 for CC)
	if len(body) < 2 || body[1] != 0xD0 {
		return fmt.Errorf("unexpected COTP response type: %x", body[1])
	}
	return nil
}

// s7Setup performs the S7 communication setup (negotiate PDU size).
func (d *S7Driver) s7Setup() error {
	// Build S7 Setup request
	// S7 header: protocol id=0x32, ROSCTR=0x01 (Job)
	// Parameters: function=0xF0 (setup), reserved=0x00, 2 parameter blocks
	pdu := buildS7SetupPDU()
	if err := d.sendS7PDU(pdu); err != nil {
		return err
	}
	// Read response
	resp, err := d.recvS7PDU()
	if err != nil {
		return err
	}
	if len(resp) < 18 || resp[1] != 0x03 {
		return fmt.Errorf("s7 setup: unexpected response type: %x", resp[1])
	}
	return nil
}

// buildS7SetupPDU builds the S7 communication setup PDU.
func buildS7SetupPDU() []byte {
	// S7 header (10 bytes)
	pdu := []byte{
		0x32,       // protocol id
		0x01,       // ROSCTR: Job
		0x00, 0x00, // Redundancy
		0x00, 0x00, // PDU reference
		0x00, 0x08, // Parameter field length
		0x00, 0x00, // Data field length
	}
	// Parameters
	pdu = append(pdu,
		0xF0,       // function: setup communication
		0x00,       // reserved
		0x01, 0x03, // Max AMQ pending: 3
		0x01, 0x03, // Max AMQ outstanding: 3
		0x01, 0x03, // PDU size: 0x0103 = 259
	)
	return pdu
}

// sendS7PDU wraps a S7 PDU in a TPKT+COTP DT frame and sends it.
func (d *S7Driver) sendS7PDU(s7pdu []byte) error {
	// COTP DT: LI(0x02) + PDU type(0xF0) + EOT(0x80) = 3 bytes, then the S7 PDU.
	// TPKT length covers the whole packet including its own 4-byte header.
	cotpDT := []byte{0x02, 0xF0, 0x80}
	total := 4 + len(cotpDT) + len(s7pdu)
	frame := make([]byte, 0, total)
	frame = append(frame,
		0x03, 0x00, // TPKT version + reserved
		byte(total>>8), byte(total&0xFF), // TPKT length
	)
	frame = append(frame, cotpDT...)
	frame = append(frame, s7pdu...)
	return tcpWrite(d.conn, frame, d.timeout)
}

// recvS7PDU reads a S7 PDU from the connection.
func (d *S7Driver) recvS7PDU() ([]byte, error) {
	// Read TPKT header (4 bytes)
	tpktHeader, err := tcpReadExact(d.conn, 4, d.timeout)
	if err != nil {
		return nil, fmt.Errorf("read tpkt header: %w", err)
	}
	if tpktHeader[0] != 0x03 || tpktHeader[1] != 0x00 {
		return nil, fmt.Errorf("invalid TPKT header: %x %x", tpktHeader[0], tpktHeader[1])
	}
	tpktLen := int(binary.BigEndian.Uint16(tpktHeader[2:4]))
	if tpktLen < 4 {
		return nil, fmt.Errorf("invalid TPKT length: %d", tpktLen)
	}
	body, err := tcpReadExact(d.conn, tpktLen-4, d.timeout)
	if err != nil {
		return nil, fmt.Errorf("read tpkt body: %w", err)
	}
	// body[0] is the COTP length indicator; the COTP header (type + any options)
	// occupies 1+LI bytes, after which the S7 PDU begins.
	if len(body) < 2 {
		return nil, fmt.Errorf("short COTP frame")
	}
	cotpLen := int(body[0])
	offset := 1 + cotpLen
	if offset > len(body) {
		return nil, fmt.Errorf("malformed COTP frame")
	}
	return body[offset:], nil
}

// Disconnect closes the S7 connection.
func (d *S7Driver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		err := d.conn.Close()
		d.conn = nil
		d.isoConnected = false
		d.SetConnected(false)
		return err
	}
	return nil
}

// ReadPoints reads all configured points from the S7 PLC.
func (d *S7Driver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("s7 not connected")
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
				WithField("error", err.Error()).
				Warn("S7 read failed")
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

// readPoint reads a single S7 point by parsing the address.
// Supported address formats:
//   - "DB1.DBX0.0"  -> DB 1, bit 0.0 (bool)
//   - "DB1.DBB0"    -> DB 1, byte 0 (uint8)
//   - "DB1.DBW0"    -> DB 1, word 0 (int16/uint16)
//   - "DB1.DBD0"    -> DB 1, dword 0 (int32/uint32/float32)
//   - "M10.0"       -> Merker bit 10.0
//   - "MB10"        -> Merker byte 10
//   - "MW10"        -> Merker word 10
//   - "MD10"        -> Merker dword 10
//   - "I0.0"/"Q0.0" -> I/O bits
func (d *S7Driver) readPoint(pt models.PointDef) (interface{}, error) {
	area, dbNum, dataType, byteOffset, bitOffset, err := parseS7Address(pt.Address)
	if err != nil {
		return nil, err
	}

	// Determine read size based on data type
	var readSize int
	switch dataType {
	case s7TypeBit:
		readSize = 1
	case s7TypeByte:
		readSize = 1
	case s7TypeWord:
		readSize = 2
	case s7TypeDWord:
		readSize = 4
	default:
		readSize = 1
	}

	data, err := d.s7Read(area, dbNum, byteOffset, readSize)
	if err != nil {
		return nil, err
	}
	// Guard every branch below: an empty payload must be an error, not a panic.
	if len(data) == 0 {
		return nil, fmt.Errorf("s7 read returned no data for %s", pt.Address)
	}

	// Parse value based on data type and point's DataType field
	dataTypeStr := strings.ToLower(pt.DataType)
	if dataTypeStr == "" {
		dataTypeStr = "int16"
	}

	switch dataType {
	case s7TypeBit:
		if len(data) < 1 {
			return nil, fmt.Errorf("no data for bit")
		}
		return (data[0]>>uint(bitOffset))&0x01 != 0, nil
	case s7TypeByte:
		return data[0], nil
	case s7TypeWord:
		if len(data) < 2 {
			return nil, fmt.Errorf("insufficient data for word")
		}
		val := binary.BigEndian.Uint16(data[:2])
		switch dataTypeStr {
		case "int16", "short":
			return int16(val), nil
		case "uint16", "word", "uint":
			return val, nil
		default:
			return int16(val), nil
		}
	case s7TypeDWord:
		if len(data) < 4 {
			return nil, fmt.Errorf("insufficient data for dword")
		}
		val := binary.BigEndian.Uint32(data[:4])
		switch dataTypeStr {
		case "int32", "long":
			return int32(val), nil
		case "uint32", "dword":
			return val, nil
		case "float32", "float", "real":
			return float32FromBits(val), nil
		case "float64", "double":
			// Read 4 more bytes for double
			data2, err := d.s7Read(area, dbNum, byteOffset+4, 4)
			if err != nil {
				return nil, err
			}
			full := append(data, data2...)
			if len(full) >= 8 {
				return float64FromBits(binary.BigEndian.Uint64(full[:8])), nil
			}
			return float32FromBits(val), nil
		default:
			return int32(val), nil
		}
	}
	return nil, fmt.Errorf("unsupported data type: %c", dataType)
}

// s7Read reads data from the S7 PLC using a Read Var PDU.
func (d *S7Driver) s7Read(area int, dbNum int, offset int, size int) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	// Build the S7 Read Var parameter item (S7ANY, 12 bytes after func+count):
	//
	//	[0]  0x12 variable spec
	//	[1]  0x0A length of following
	//	[2]  0x10 syntax id (S7ANY)
	//	[3]  transport size (0x02 = BYTE)
	//	[4:6] element count (byte count for BYTE transport)
	//	[6:8] DB number (big-endian)
	//	[8]   area code
	//	[9:12] start address (3 bytes, byte offset * 8)
	paramField := []byte{
		0x04,                               // function: Read Var
		0x01,                               // item count: 1
		0x12,                               // variable spec
		0x0A,                               // length of following: 10
		0x10,                               // syntax id: S7ANY
		0x02,                               // transport size: BYTE
		byte(size >> 8), byte(size & 0xFF), // element count (bytes)
		byte(dbNum >> 8), byte(dbNum & 0xFF), // DB number
		byte(area), // area code
	}
	bitAddr := offset * 8
	paramField = append(paramField, byte(bitAddr>>16), byte(bitAddr>>8), byte(bitAddr&0xFF))

	paramLen := len(paramField)
	s7pdu := make([]byte, 0, 10+paramLen)
	// S7 header (Job request = 10 bytes)
	s7pdu = append(s7pdu,
		0x32,       // protocol id
		0x01,       // ROSCTR: Job
		0x00, 0x00, // Redundancy
		0x00, 0x01, // PDU reference
		byte(paramLen>>8), byte(paramLen&0xFF), // Parameter field length
		0x00, 0x00, // Data field length
	)
	s7pdu = append(s7pdu, paramField...)

	if err := d.sendS7PDU(s7pdu); err != nil {
		return nil, fmt.Errorf("send read: %w", err)
	}

	resp, err := d.recvS7PDU()
	if err != nil {
		return nil, fmt.Errorf("recv read: %w", err)
	}

	// S7 Ack_Data header is 12 bytes (includes error class + error code).
	if len(resp) < 12 {
		return nil, fmt.Errorf("short S7 response: %d", len(resp))
	}
	if resp[0] != 0x32 {
		return nil, fmt.Errorf("invalid S7 protocol id: %x", resp[0])
	}
	if resp[1] != 0x03 {
		return nil, fmt.Errorf("unexpected S7 response type: %x", resp[1])
	}
	respParamLen := int(binary.BigEndian.Uint16(resp[6:8]))
	dataStart := 12 + respParamLen
	if len(resp) < dataStart+4 {
		return nil, fmt.Errorf("response too short: need %d, got %d", dataStart+4, len(resp))
	}
	data := resp[dataStart:]

	// Data item: ReturnCode(1) + TransportSize(1) + Length(2) + Data(N).
	// Per the S7comm spec 0xFF is the ONLY data-item return code that means the
	// read succeeded and the following bytes are valid payload. 0x00 is reserved
	// ("no data"), so accepting it would let a peer return zero bytes that the
	// driver would decode as a fabricated "good" value.
	retCode := data[0]
	if retCode != 0xFF {
		return nil, fmt.Errorf("s7 read error, return code: 0x%02x", retCode)
	}
	respTransport := data[1]
	lengthField := int(binary.BigEndian.Uint16(data[2:4]))
	byteLen := lengthField
	// WORD/DWORD responses encode the length in bits; octet-string (0x09) and
	// BIT (0x03) responses do not.
	if respTransport != 0x09 && respTransport != 0x03 {
		byteLen = (lengthField + 7) / 8
	}
	if byteLen > size {
		byteLen = size
	}
	if len(data) < 4+byteLen {
		return nil, fmt.Errorf("data content too short: expected %d, got %d", byteLen, len(data)-4)
	}
	return data[4 : 4+byteLen], nil
}

// WritePointAtAddress writes to the point's S7 address ("DB1.DBW0"), which is
// what this driver parses; the operator's point name is not an address.
func (d *S7Driver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	return d.WritePoint(ctx, address, value)
}

// WritePoint writes a value to an S7 point.
func (d *S7Driver) WritePoint(ctx context.Context, point string, value interface{}) error {
	if !d.IsConnected() {
		return fmt.Errorf("s7 not connected")
	}

	// Check circuit breaker before write
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	area, dbNum, dataType, byteOffset, bitOffset, err := parseS7Address(point)
	if err != nil {
		return err
	}

	var writeData []byte
	switch dataType {
	case s7TypeBit:
		bv := false
		switch v := value.(type) {
		case bool:
			bv = v
		case float64:
			bv = v != 0
		case int64:
			bv = v != 0
		case int:
			bv = v != 0
		}
		// Read current byte first, then modify the bit
		curData, err := d.s7Read(area, dbNum, byteOffset, 1)
		if err != nil {
			return err
		}
		b := curData[0]
		if bv {
			b |= 1 << uint(bitOffset)
		} else {
			b &^= 1 << uint(bitOffset)
		}
		writeData = []byte{b}
	case s7TypeByte:
		if err := checkS7WriteRange(value, 8); err != nil {
			return err
		}
		writeData = []byte{byte(toInt64(value))}
	case s7TypeWord:
		if err := checkS7WriteRange(value, 16); err != nil {
			return err
		}
		writeData = make([]byte, 2)
		binary.BigEndian.PutUint16(writeData, uint16(toInt64(value)))
	case s7TypeDWord:
		if err := checkS7WriteRange(value, 32); err != nil {
			return err
		}
		writeData = make([]byte, 4)
		binary.BigEndian.PutUint32(writeData, s7DWordBits(value))
	default:
		return fmt.Errorf("unsupported write data type: %c", dataType)
	}

	if err := d.s7Write(area, dbNum, byteOffset, writeData); err != nil {
		d.RecordWriteFailure()
		return err
	}
	d.RecordWriteSuccess()
	return nil
}

// checkS7WriteRange refuses a number the addressed width cannot represent.
//
// The S7 address says how many bytes a value occupies (DBB, DBW, DBD) but not
// whether the point is signed or unsigned, so the accepted span is the union of
// both. Without it Go's narrowing wrapped the input: 70000 into a DBW arrived as
// 4464 and the write reported success. A DBD holding a fractional value is a REAL,
// whose bits the encoder lays out itself, so it is not width-checked as an integer.
func checkS7WriteRange(value interface{}, bits uint) error {
	if bits == 32 {
		switch n := value.(type) {
		case float32:
			return nil
		case float64:
			if n != math.Trunc(n) {
				return nil
			}
		}
	}
	f, ok := toFloat64(value)
	if !ok {
		return nil
	}
	min := -math.Pow(2, float64(bits-1))
	max := math.Pow(2, float64(bits)) - 1
	if f < min || f > max {
		return fmt.Errorf("value %v does not fit a %d-bit S7 target (%v to %v)", f, bits, min, max)
	}
	return nil
}

// s7DWordBits encodes a value for a 4-byte (DBD/MD/DW) S7 target.
//
// REAL and DINT share the same width, and the write path carries no data type,
// so the Go value type decides the wire format: an explicit float32, or a float
// carrying a fractional part, is written as IEEE-754 bits (previously it was
// truncated to an integer, silently writing 12 to a REAL holding 12.5). Integral
// values keep the two's-complement integer encoding used by DINT/UDINT points.
func s7DWordBits(v interface{}) uint32 {
	switch n := v.(type) {
	case float32:
		return math.Float32bits(n)
	case float64:
		if n != math.Trunc(n) {
			return math.Float32bits(float32(n))
		}
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil && f != math.Trunc(f) {
			return math.Float32bits(float32(f))
		}
	}
	return uint32(toInt64(v))
}

// s7Write writes data to the S7 PLC using a Write Var PDU.
func (d *S7Driver) s7Write(area int, dbNum int, offset int, data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("not connected")
	}

	size := len(data)
	bitAddr := offset * 8

	// Parameter field (item): transport size BYTE, element count = byte count.
	paramField := []byte{
		0x05,                               // function: Write Var
		0x01,                               // item count: 1
		0x12,                               // variable spec
		0x0A,                               // length: 10
		0x10,                               // syntax id: S7ANY
		0x02,                               // transport size: BYTE
		byte(size >> 8), byte(size & 0xFF), // element count (bytes)
		byte(dbNum >> 8), byte(dbNum & 0xFF), // DB number
		byte(area),                                                    // area code
		byte(bitAddr >> 16), byte(bitAddr >> 8), byte(bitAddr & 0xFF), // address
	}

	// Data field: return code + transport size + length (bytes) + data
	dataField := make([]byte, 0, 4+size)
	dataField = append(dataField,
		0x00,                           // return code (0 = success)
		0x02,                           // transport size: BYTE
		byte(size>>8), byte(size&0xFF), // data length (bytes)
	)
	dataField = append(dataField, data...)

	paramLen := len(paramField)
	dataLen := len(dataField)
	s7pdu := make([]byte, 0, 10+paramLen+dataLen)
	s7pdu = append(s7pdu,
		0x32,       // protocol id
		0x01,       // ROSCTR: Job
		0x00, 0x00, // Redundancy
		0x00, 0x02, // PDU reference
		byte(paramLen>>8), byte(paramLen&0xFF), // Parameter field length
		byte(dataLen>>8), byte(dataLen&0xFF), // Data field length
	)
	s7pdu = append(s7pdu, paramField...)
	s7pdu = append(s7pdu, dataField...)

	if err := d.sendS7PDU(s7pdu); err != nil {
		return fmt.Errorf("send write: %w", err)
	}

	resp, err := d.recvS7PDU()
	if err != nil {
		return fmt.Errorf("recv write: %w", err)
	}

	if len(resp) < 12 {
		return fmt.Errorf("short write response")
	}
	if resp[1] != 0x03 {
		return fmt.Errorf("unexpected write response type: %x", resp[1])
	}

	// Ack_Data header is 12 bytes; the per-item return code is the first byte
	// of the data section (after the parameter field).
	respParamLen := int(binary.BigEndian.Uint16(resp[6:8]))
	dataStart := 12 + respParamLen
	if dataStart >= len(resp) {
		return fmt.Errorf("write response too short")
	}
	retCode := resp[dataStart]
	if retCode != 0xFF {
		return fmt.Errorf("s7 write error, return code: 0x%02x", retCode)
	}
	return nil
}

func (d *S7Driver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrDiscoveryUnsupported
}

func (d *S7Driver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("not connected")
	}
	// Read a single byte from Merker area 0 as health check
	start := time.Now()
	_, err := d.s7Read(s7AreaMK, 0, 0, 1)
	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		d.RecordReadFailure()
		return err
	}
	d.RecordReadSuccess(latencyMs)
	return nil
}

// --- S7 Address Parsing ---

// parseS7Address parses an S7 address string.
// Examples: "DB1.DBX0.0", "DB1.DBB10", "DB1.DBW10", "DB1.DBD10",
//
//	"M10.0", "MB10", "MW10", "MD10",
//	"I0.0", "IB0", "IW0", "ID0",
//	"Q0.0", "QB0", "QW0", "QD0"
func parseS7Address(addr string) (area int, dbNum int, dataType byte, byteOffset int, bitOffset int, err error) {
	addr = strings.TrimSpace(strings.ToUpper(addr))
	if addr == "" {
		return 0, 0, 0, 0, 0, fmt.Errorf("empty s7 address")
	}

	// Parse DB addressing: DB<n>.DB<t><offset> or DB<n>.DB<t><offset>.<bit>
	if strings.HasPrefix(addr, "DB") {
		parts := strings.SplitN(addr, ".", 2)
		if len(parts) < 2 {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid DB address: %s", addr)
		}
		dbNum, err = strconv.Atoi(strings.TrimPrefix(parts[0], "DB"))
		if err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid DB number: %s", parts[0])
		}

		typeAndOffset := strings.TrimPrefix(parts[1], "DB")
		return parseS7TypeAndOffset(typeAndOffset, s7AreaDB, dbNum)
	}

	// Parse Merker (M) addressing
	if strings.HasPrefix(addr, "M") {
		return parseS7TypeAndOffset(strings.TrimPrefix(addr, "M"), s7AreaMK, 0)
	}

	// Parse Input (I/E) addressing
	if strings.HasPrefix(addr, "I") || strings.HasPrefix(addr, "E") {
		return parseS7TypeAndOffset(strings.TrimPrefix(strings.TrimPrefix(addr, "I"), "E"), s7AreaPE, 0)
	}

	// Parse Output (Q/A) addressing
	if strings.HasPrefix(addr, "Q") || strings.HasPrefix(addr, "A") {
		return parseS7TypeAndOffset(strings.TrimPrefix(strings.TrimPrefix(addr, "Q"), "A"), s7AreaPA, 0)
	}

	return 0, 0, 0, 0, 0, fmt.Errorf("unsupported s7 address format: %s", addr)
}

// parseS7TypeAndOffset parses the type and offset from an address suffix.
func parseS7TypeAndOffset(s string, area int, dbNum int) (int, int, byte, int, int, error) {
	if len(s) == 0 {
		return 0, 0, 0, 0, 0, fmt.Errorf("empty type and offset")
	}

	// Check for bit addressing: X<offset>.<bit>, B<offset>.<bit>, or <offset>.<bit>
	if s[0] == 'X' || (s[0] == 'B' && strings.Contains(s, ".")) || strings.Contains(s, ".") {
		// Bit addressing
		parts := strings.SplitN(s, ".", 2)
		offsetStr := strings.TrimPrefix(parts[0], "X")
		offsetStr = strings.TrimPrefix(offsetStr, "B")
		byteOffset, err := strconv.Atoi(offsetStr)
		if err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid bit offset: %s", offsetStr)
		}
		bitOffset := 0
		if len(parts) > 1 {
			bitOffset, err = strconv.Atoi(parts[1])
			if err != nil || bitOffset < 0 || bitOffset > 7 {
				return 0, 0, 0, 0, 0, fmt.Errorf("invalid bit number: %s", parts[1])
			}
		}
		return area, dbNum, s7TypeBit, byteOffset, bitOffset, nil
	}

	// Check for typed addressing: B<offset>, W<offset>, D<offset>
	switch s[0] {
	case 'B':
		offset, err := strconv.Atoi(s[1:])
		if err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid byte offset: %s", s[1:])
		}
		return area, dbNum, s7TypeByte, offset, 0, nil
	case 'W':
		offset, err := strconv.Atoi(s[1:])
		if err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid word offset: %s", s[1:])
		}
		return area, dbNum, s7TypeWord, offset, 0, nil
	case 'D':
		offset, err := strconv.Atoi(s[1:])
		if err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("invalid dword offset: %s", s[1:])
		}
		return area, dbNum, s7TypeDWord, offset, 0, nil
	}

	// Default: treat as byte offset with word type
	offset, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("invalid offset: %s", s)
	}
	return area, dbNum, s7TypeWord, offset, 0, nil
}

// tcpReadExact reads exactly n bytes from a TCP connection with timeout.
func tcpReadExact(conn net.Conn, n int, timeout time.Duration) ([]byte, error) {
	buf := make([]byte, n)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	total := 0
	for total < n {
		nread, err := conn.Read(buf[total:])
		if nread > 0 {
			total += nread
		}
		if err != nil {
			return buf[:total], err
		}
	}
	return buf, nil
}

// toInt64 converts various numeric types to int64.
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int8:
		return int64(n)
	case int16:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint:
		return int64(n)
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	case bool:
		if n {
			return 1
		}
		return 0
	default:
		return 0
	}
}
