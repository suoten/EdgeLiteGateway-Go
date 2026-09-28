package drivers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/sirupsen/logrus"

	"edgelite/internal/constants"
	"edgelite/internal/models"
	"edgelite/internal/storage"
)

// OPCUADriver implements an OPC UA client driver.
// It uses the gopcua/opcua library, which performs the full binary-transport
// handshake (HEL/ACK, OpenSecureChannel, CreateSession, ActivateSession) so
// reads/writes succeed against compliant OPC UA servers (e.g. freeopcua, Kepware).
// When the server is unreachable, Connect returns an error and the collector
// surfaces bad quality rather than fabricating values.
type OPCUADriver struct {
	BaseDriver
	endpoint        string
	security        string
	securityMode    ua.MessageSecurityMode
	securityPolicy  string
	certificateFile string
	privateKeyFile  string
	username        string
	password        string
	timeout         time.Duration
	mu              sync.Mutex
	connected       bool
	client          *opcua.Client     // gopcua client (holds secure channel + session)
	nodeIDs         map[string]string // point name -> configured OPC UA NodeID
	nsIndex         uint16            // namespace used for synthesised "<deviceID>.<point>" node ids
	resolved        map[string]string // point name -> NodeId text the server accepted
	dataTypes       map[string]string // point name -> declared data type, used to type writes
}

func NewOPCUADriver(deviceID string, config map[string]interface{}) (Driver, error) {
	// ProtoForge's EdgeLite integration sends `endpoint`/`server_url` plus
	// `security_mode`; Kepware-style gateways use `security`. Accept all of them.
	endpoint := GetConfigString(config, "endpoint", "")
	if endpoint == "" {
		endpoint = GetConfigString(config, "server_url", "opc.tcp://127.0.0.1:4840")
	}
	security := GetConfigString(config, "security_mode", "")
	if security == "" {
		security = GetConfigString(config, "security", "None")
	}
	mode, err := parseOPCUASecurityMode(security)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second))
	// The device form exposes this as session_timeout in milliseconds and never
	// sends `timeout`, so without the alias the connect deadline is always the
	// default no matter what the operator set.
	if ms := GetConfigFloat(config, "session_timeout", 0); ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}
	d := &OPCUADriver{
		endpoint:        endpoint,
		security:        security,
		securityMode:    mode,
		securityPolicy:  GetConfigString(config, "security_policy", ""),
		certificateFile: opcuaConfigFile(config, "client_cert_path", "certificate_file"),
		privateKeyFile:  opcuaConfigFile(config, "client_key_path", "private_key_file"),
		username:        GetConfigString(config, "username", ""),
		password:        GetConfigString(config, "password", ""),
		timeout:         timeout,
		nodeIDs:         make(map[string]string),
		nsIndex:         uint16(GetConfigFloat(config, "namespace_index", 2)),
		resolved:        make(map[string]string),
		dataTypes:       make(map[string]string),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	// Parse point-to-NodeID mapping from config
	if points, ok := config["points"].([]interface{}); ok {
		for _, p := range points {
			if pm, ok := p.(map[string]interface{}); ok {
				name := GetConfigString(pm, "name", "")
				nodeID := GetConfigString(pm, "address", "")
				if nodeID == "" {
					nodeID = GetConfigString(pm, "node_id", "")
				}
				if name != "" && nodeID != "" {
					d.nodeIDs[name] = nodeID
				}
				if name != "" {
					if dt := GetConfigString(pm, "data_type", ""); dt != "" {
						d.dataTypes[name] = dt
					}
				}
			}
		}
	}
	if ids, ok := config["node_ids"].(map[string]interface{}); ok {
		for name, v := range ids {
			if s, ok := v.(string); ok && s != "" {
				d.nodeIDs[name] = s
			}
		}
	}
	return d, nil
}

func (d *OPCUADriver) Name() string { return "opc_ua" }

func (d *OPCUADriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}
	if d.client != nil && d.connected {
		return nil
	}

	d.SetConnectionState(StateConnecting, "opc ua connecting")

	// Sign and SignAndEncrypt need a client X.509 identity: gopcua cannot build a
	// secure channel without one, and the failure it reports ("no certificate")
	// reads like a server problem. Say what is actually missing instead.
	if d.securityMode != ua.MessageSecurityModeNone && (d.certificateFile == "" || d.privateKeyFile == "") {
		err := fmt.Errorf("opc ua security mode %q requires certificate_file and private_key_file in the device config", d.security)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return err
	}

	opts := []opcua.Option{
		opcua.SecurityMode(d.securityMode),
		opcua.RequestTimeout(d.timeout),
		opcua.SessionTimeout(d.timeout),
		opcua.CertificateFile(d.certificateFile),
		opcua.PrivateKeyFile(d.privateKeyFile),
	}
	if d.securityPolicy != "" {
		opts = append(opts, opcua.SecurityPolicy(d.securityPolicy))
	}
	if d.username != "" {
		opts = append(opts, opcua.AuthUsername(d.username, d.password))
	} else {
		opts = append(opts, opcua.AuthAnonymous())
	}

	cl, err := opcua.NewClient(d.endpoint, opts...)
	if err != nil {
		d.SetConnected(false)
		d.RecordReadFailure()
		return fmt.Errorf("opc ua client init: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, d.timeout+2*time.Second)
	defer cancel()
	if err := cl.Connect(cctx); err != nil {
		_ = cl.Close(context.Background())
		d.SetConnected(false)
		// The collect loop stops at Connect, so this is the only place an
		// unreachable server enters the health counters the device panels read.
		d.RecordReadFailure()
		logrus.WithField("device_id", d.DeviceID()).
			WithField("endpoint", d.endpoint).
			WithField("error", err.Error()).
			Warn("OPC UA connect failed")
		return fmt.Errorf("opc ua connect failed: %w", err)
	}

	d.client = cl
	d.connected = true
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "opc ua connected")
	logrus.WithField("device_id", d.DeviceID()).
		WithField("endpoint", d.endpoint).Debug("OPC UA connected")
	return nil
}

func (d *OPCUADriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout+2*time.Second)
		defer cancel()
		_ = d.client.Close(ctx)
		d.client = nil
	}
	d.connected = false
	d.SetConnected(false)
	return nil
}

func (d *OPCUADriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	d.mu.Lock()
	cl := d.client
	connected := d.connected
	d.mu.Unlock()

	if !connected || cl == nil {
		return nil, fmt.Errorf("opc ua not connected")
	}
	if d.IsCircuitOpen() {
		return nil, fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	now := time.Now()
	start := time.Now()
	result := make([]storage.PointData, 0, len(points))
	anyFailed := false

	for _, pt := range points {
		nodeIDStr := pt.Address
		if nodeIDStr == "" {
			nodeIDStr = d.nodeIDs[pt.Name]
		}
		if nodeIDStr == "" {
			nodeIDStr = "ns=2;s=" + pt.Name
		}
		pd := storage.PointData{
			DeviceID:  d.DeviceID(),
			PointName: pt.Name,
			Timestamp: now,
			Quality:   "bad",
		}
		nid, err := ua.ParseNodeID(nodeIDStr)
		if err != nil {
			anyFailed = true
			logrus.WithField("device_id", d.DeviceID()).
				WithField("node_id", nodeIDStr).
				WithError(err).Warn("OPC UA invalid node id")
			result = append(result, pd)
			continue
		}
		dvs, err := cl.Node(nid).Attributes(ctx, ua.AttributeIDValue)
		if err != nil {
			anyFailed = true
			logrus.WithField("device_id", d.DeviceID()).
				WithField("node_id", nodeIDStr).
				WithError(err).Warn("OPC UA read failed")
			result = append(result, pd)
			continue
		}
		if len(dvs) == 0 || dvs[0] == nil || dvs[0].Value == nil {
			anyFailed = true
			logrus.WithField("device_id", d.DeviceID()).
				WithField("node_id", nodeIDStr).
				Warn("OPC UA read returned no data value")
			result = append(result, pd)
			continue
		}
		dv := dvs[0]
		// Honor the server's StatusCode: OPC UA severity lives in the top two bits
		// (00=Good, 01=Uncertain, 10/11=Bad). A Variant accompanied by an
		// Uncertain/Bad status is NOT a trustworthy reading, so it must degrade to
		// "bad" rather than be reported "good" — otherwise the driver fabricates
		// data quality the server explicitly declined to guarantee.
		if uint32(dv.Status)&0xC0000000 != 0 {
			anyFailed = true
			logrus.WithField("device_id", d.DeviceID()).
				WithField("node_id", nodeIDStr).
				WithField("status", dv.Status.Error()).
				Warn("OPC UA node returned non-good status")
			result = append(result, pd)
			continue
		}
		pd.Value = dv.Value.Value()
		pd.Quality = "good"
		result = append(result, pd)
	}

	if anyFailed {
		d.RecordReadFailure()
	} else {
		d.RecordReadSuccess(float64(time.Since(start).Microseconds()) / 1000.0)
	}
	return result, nil
}

// WritePoint applies one node write and records the outcome in the driver health
// counters, which is the only source the device statistics panel reads. The
// wrapper exists so no early return below can skip the bookkeeping.
// WritePointAtAddress writes to an OPC UA NodeID ("ns=2;s=pf-opcua.temp"), the
// only identifier this protocol can address a node with. The plain WritePoint
// takes the operator's point name and resolves it through the config's point
// list, which the collector never populates, so a name that is not a NodeID
// parses as some other node and the server answers for the wrong one.
func (d *OPCUADriver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	return d.writeTyped(ctx, address, value, dataType)
}

func (d *OPCUADriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	d.mu.Lock()
	dataType := d.dataTypes[point]
	d.mu.Unlock()
	return d.writeTyped(ctx, point, value, dataType)
}

// writeTyped is the single write entry so every path, including the address one
// used by the UI, records the same health counters the statistics panel reads.
func (d *OPCUADriver) writeTyped(ctx context.Context, point string, value interface{}, dataType string) error {
	if err := d.writeNode(ctx, point, value, dataType); err != nil {
		d.RecordWriteFailure()
		return err
	}
	d.RecordWriteSuccess()
	return nil
}

func (d *OPCUADriver) writeNode(ctx context.Context, point string, value interface{}, dataType string) error {
	d.mu.Lock()
	cl := d.client
	nodeIDStr := d.nodeIDs[point]
	d.mu.Unlock()

	if cl == nil {
		return fmt.Errorf("opc ua not connected")
	}
	if nodeIDStr == "" {
		nodeIDStr = point
	}
	nid, err := ua.ParseNodeID(nodeIDStr)
	if err != nil {
		return fmt.Errorf("invalid node id %q: %w", nodeIDStr, err)
	}
	typed, err := coerceOPCUAWriteValue(value, dataType)
	if err != nil {
		return fmt.Errorf("opc ua write of %q: %w", dataType, err)
	}
	v, err := ua.NewVariant(typed)
	if err != nil {
		return fmt.Errorf("opc ua encode value: %w", err)
	}
	req := &ua.WriteRequest{
		NodesToWrite: []*ua.WriteValue{{
			NodeID:      nid,
			AttributeID: ua.AttributeIDValue,
			Value:       &ua.DataValue{EncodingMask: ua.DataValueValue, Value: v},
		}},
	}
	resp, err := cl.Write(ctx, req)
	if err != nil {
		return err
	}
	if len(resp.Results) > 0 && resp.Results[0] != ua.StatusOK {
		return fmt.Errorf("opc ua write rejected: %v", resp.Results[0])
	}
	logrus.WithField("device_id", d.DeviceID()).
		WithField("point", point).
		WithField("value", value).
		Info("OPC UA write applied")
	return nil
}

// coerceOPCUAWriteValue retypes a decoded JSON value to the node's declared data
// type. A JSON number always arrives as float64, which encodes as OPC UA Double,
// so writing 66.5 to a Float node answered StatusBadTypeMismatch and no value
// ever reached the PLC. An unknown or empty data type is passed through untouched:
// the server knows its own node better than this config does. A value that does
// not fit the declared width is refused rather than wrapped, since silently
// writing 4464 where the operator typed 70000 is worse than failing the write.
func coerceOPCUAWriteValue(value interface{}, dataType string) (interface{}, error) {
	if dataType == "" {
		return value, nil
	}
	key := strings.ToLower(strings.TrimSpace(dataType))

	num, isNum := jsonNumber(value)
	switch key {
	case "bool", "boolean":
		if b, ok := value.(bool); ok {
			return b, nil
		}
		if isNum {
			return num != 0, nil
		}
		return value, nil
	case "string", "str":
		if _, ok := value.(string); ok {
			return value, nil
		}
		return fmt.Sprintf("%v", value), nil
	}
	if !isNum {
		return value, nil
	}

	if min, max, ok := dataTypeIntRange(key); ok {
		if num < min || num > max {
			return nil, fmt.Errorf("value %v does not fit the declared %s range [%v, %v]", num, key, min, max)
		}
	}
	switch key {
	case "float32", "float", "real", "single":
		return float32(num), nil
	case "sbyte", "int8":
		return int8(num), nil
	case "byte", "uint8":
		return uint8(num), nil
	case "int16", "short":
		return int16(num), nil
	case "uint16", "word":
		return uint16(num), nil
	case "int32", "long", "dint":
		return int32(num), nil
	case "uint32", "dword":
		return uint32(num), nil
	case "int64":
		return int64(num), nil
	case "uint64":
		return uint64(num), nil
	case "double", "float64":
		return num, nil
	}
	return value, nil
}

// jsonNumber reports the numeric form of the types encoding/json and this
// driver's own reads produce for a number.
func jsonNumber(value interface{}) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint16:
		return float64(n), true
	default:
		return 0, false
	}
}

// Discover asks an endpoint whether a real OPC UA server stands behind it.
//
// It used to answer with a bare TCP dial, so any open port was listed as a server,
// and the entry it produced carried "endpoint" but no "ip" - the key the device
// table and the "add selected" flow read - so the row rendered empty and the device
// it created pointed nowhere. The probe now opens an anonymous, unsecured session
// and only reports what completed the handshake.
func (d *OPCUADriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	// What to ask, in order of explicitness: the config the caller passed, a host and
	// port from the discovery dialog, and finally this device's own endpoint.
	endpoint := strings.TrimSpace(GetConfigString(config, "endpoint", ""))
	if endpoint == "" {
		if host := strings.TrimSpace(GetConfigString(config, "host", "")); host != "" {
			endpoint = fmt.Sprintf("opc.tcp://%s:%d", host, GetConfigInt(config, "port", 4840))
		}
	}
	if endpoint == "" {
		endpoint = strings.TrimSpace(d.endpoint)
	}
	if endpoint == "" {
		return nil, fmt.Errorf("opc ua discovery needs an endpoint or host to ask")
	}
	host, port := parseOPCUAEndpoint(endpoint)
	if host == "" {
		return nil, fmt.Errorf("opc ua discovery cannot read a host from endpoint %q", endpoint)
	}

	timeout := d.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	opts := []opcua.Option{
		opcua.SecurityMode(ua.MessageSecurityModeNone),
		opcua.AuthAnonymous(),
		opcua.RequestTimeout(timeout),
	}
	cl, err := opcua.NewClient(endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("opc ua discovery client: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	if err := cl.Connect(cctx); err != nil {
		_ = cl.Close(context.Background())
		// A refused handshake is the answer to the question, not a gateway fault: the
		// endpoint simply is not an OPC UA server. Say which error decided that.
		logrus.WithField("endpoint", endpoint).
			WithError(err).
			Debug("OPC UA discovery probe rejected")
		return nil, nil
	}
	defer func() { _ = cl.Close(context.Background()) }()

	return []map[string]interface{}{{
		"ip":        host,
		"port":      port,
		"protocol":  "opc_ua",
		"endpoint":  endpoint,
		"device_id": fmt.Sprintf("opcua-%s-%d", host, port),
		"name":      endpoint,
	}}, nil
}

func (d *OPCUADriver) HealthCheck(ctx context.Context) error {
	d.mu.Lock()
	cl := d.client
	connected := d.connected
	d.mu.Unlock()

	if !connected || cl == nil {
		return fmt.Errorf("not connected")
	}
	if cl.State() != opcua.Connected {
		d.SetConnected(false)
		return fmt.Errorf("opc ua connection lost")
	}
	return nil
}

// maxBrowseReferences bounds one address-space listing so a browse of a huge
// server cannot make the gateway buffer an unbounded response.
const maxBrowseReferences = 500

// BrowseChildren lists the direct children of an OPC UA node through the Browse
// service. An empty nodeID starts at the Objects folder, which is where a client
// is meant to begin.
func (d *OPCUADriver) BrowseChildren(ctx context.Context, nodeID string) ([]BrowseEntry, error) {
	d.mu.Lock()
	cl := d.client
	connected := d.connected
	d.mu.Unlock()

	if !connected || cl == nil {
		return nil, fmt.Errorf("opc ua not connected")
	}

	start := ua.NewNumericNodeID(0, id.ObjectsFolder)
	if nodeID != "" {
		parsed, err := ua.ParseNodeID(nodeID)
		if err != nil {
			return nil, fmt.Errorf("invalid node id %q: %w", nodeID, err)
		}
		start = parsed
	}

	bctx, cancel := context.WithTimeout(ctx, d.timeout+5*time.Second)
	defer cancel()

	entries := make([]BrowseEntry, 0, 64)
	var continuation [][]byte
	for {
		var results []*ua.BrowseResult
		var err error
		if len(continuation) == 0 {
			var resp *ua.BrowseResponse
			resp, err = cl.Browse(bctx, &ua.BrowseRequest{
				RequestedMaxReferencesPerNode: maxBrowseReferences,
				NodesToBrowse: []*ua.BrowseDescription{{
					NodeID:          start,
					BrowseDirection: ua.BrowseDirectionForward,
					ReferenceTypeID: ua.NewNumericNodeID(0, id.HierarchicalReferences),
					IncludeSubtypes: true,
					NodeClassMask:   uint32(ua.NodeClassObject | ua.NodeClassVariable),
					ResultMask:      uint32(ua.BrowseResultMaskAll),
				}},
			})
			if resp != nil {
				results = resp.Results
			}
		} else {
			var resp *ua.BrowseNextResponse
			resp, err = cl.BrowseNext(bctx, &ua.BrowseNextRequest{ContinuationPoints: continuation})
			if resp != nil {
				results = resp.Results
			}
		}
		if err != nil {
			return nil, fmt.Errorf("opc ua browse: %w", err)
		}
		continuation = nil
		for _, res := range results {
			if res == nil {
				continue
			}
			// A per-node browse status is a server answer, not a transport error:
			// reporting the node list anyway would hide "this node cannot be
			// browsed" behind an empty page.
			if res.StatusCode != ua.StatusOK {
				return nil, fmt.Errorf("opc ua browse rejected: %v", res.StatusCode)
			}
			if len(res.ContinuationPoint) > 0 {
				continuation = append(continuation, res.ContinuationPoint)
			}
			for _, ref := range res.References {
				e, ok := browseEntryFromRef(ref)
				if ok {
					entries = append(entries, e)
				}
				if len(entries) >= maxBrowseReferences {
					break
				}
			}
		}
		if len(continuation) == 0 || len(entries) >= maxBrowseReferences {
			break
		}
	}

	d.readBrowseMetadata(bctx, cl, entries)
	return entries, nil
}

func browseEntryFromRef(ref *ua.ReferenceDescription) (BrowseEntry, bool) {
	if ref == nil || ref.NodeID == nil || ref.NodeID.NodeID == nil {
		return BrowseEntry{}, false
	}
	// ServerIndex != 0 points at another server's address space; this session
	// cannot read it, so listing it would hand the user a node that always reads bad.
	if ref.NodeID.HasServerIndex() {
		return BrowseEntry{}, false
	}
	e := BrowseEntry{
		NodeID:      ref.NodeID.NodeID.String(),
		NodeClass:   opcuaNodeClassText(ref.NodeClass),
		IsContainer: ref.NodeClass == ua.NodeClassObject,
	}
	if ref.BrowseName != nil {
		e.BrowseName = fmt.Sprintf("%d:%s", ref.BrowseName.NamespaceIndex, ref.BrowseName.Name)
	}
	if ref.DisplayName != nil {
		e.DisplayName = ref.DisplayName.Text
	}
	return e, true
}

func opcuaNodeClassText(c ua.NodeClass) string {
	switch c {
	case ua.NodeClassVariable:
		return "variable"
	case ua.NodeClassObject:
		return "object"
	default:
		return "other"
	}
}

// readBrowseMetadata adds the data type and write permission of each variable,
// which is what the point editor needs to offer a node. It is best-effort on
// purpose: a server that denies the attribute read should still yield a node
// list, not fail the whole browse.
func (d *OPCUADriver) readBrowseMetadata(ctx context.Context, cl *opcua.Client, entries []BrowseEntry) {
	nodes := make([]*ua.ReadValueID, 0, len(entries)*2)
	targets := make([]int, 0, len(entries)*2)
	for i := range entries {
		if entries[i].NodeClass != "variable" {
			continue
		}
		nid, err := ua.ParseNodeID(entries[i].NodeID)
		if err != nil {
			continue
		}
		nodes = append(nodes,
			&ua.ReadValueID{NodeID: nid, AttributeID: ua.AttributeIDDataType},
			&ua.ReadValueID{NodeID: nid, AttributeID: ua.AttributeIDAccessLevel},
		)
		targets = append(targets, i, i)
	}
	if len(nodes) == 0 {
		return
	}

	resp, err := cl.Read(ctx, &ua.ReadRequest{
		NodesToRead:        nodes,
		TimestampsToReturn: ua.TimestampsToReturnNeither,
	})
	if err != nil || len(resp.Results) != len(nodes) {
		logrus.WithField("device_id", d.DeviceID()).
			WithError(err).
			Debug("OPC UA browse metadata read failed")
		return
	}
	for k, dv := range resp.Results {
		if dv == nil || dv.Status != ua.StatusOK || dv.Value == nil {
			continue
		}
		i := targets[k]
		if k%2 == 0 {
			entries[i].DataType = opcuaDataTypeText(dv.Value.Value())
			continue
		}
		// AccessLevel bit 1 (0x02) is "Writable" in the OPC UA spec.
		if al, ok := accessLevelByte(dv.Value.Value()); ok {
			entries[i].Writable = al&0x02 != 0
		}
	}
}

func opcuaDataTypeText(v interface{}) string {
	switch t := v.(type) {
	case *ua.NodeID:
		if t != nil && t.Namespace() == 0 {
			if name := id.Name(t.IntID()); name != "" {
				return name
			}
		}
		return t.String()
	default:
		return ""
	}
}

func accessLevelByte(v interface{}) (byte, bool) {
	switch t := v.(type) {
	case uint8:
		return t, true
	case int64:
		return byte(t), true
	default:
		return 0, false
	}
}

// parseOPCUASecurityMode normalises the spellings a device config carries for the
// message security mode. ua.MessageSecurityModeFromString returns Invalid for
// anything it does not match exactly, and gopcua would put mode 0 on the wire --
// the session then fails with an opaque error while the UI still shows the mode
// the user picked.
func parseOPCUASecurityMode(s string) (ua.MessageSecurityMode, error) {
	normalized := strings.ToLower(strings.TrimSpace(s))
	for _, sep := range []string{" ", "-", "_", "&", "/"} {
		normalized = strings.ReplaceAll(normalized, sep, "")
	}
	switch normalized {
	case "", "none":
		return ua.MessageSecurityModeNone, nil
	case "sign":
		return ua.MessageSecurityModeSign, nil
	case "signandencrypt", "signencrypt":
		return ua.MessageSecurityModeSignAndEncrypt, nil
	default:
		return ua.MessageSecurityModeInvalid, fmt.Errorf("unknown opc ua security mode %q (want None, Sign or SignAndEncrypt)", s)
	}
}

// opcuaConfigFile reads a certificate or key path under either spelling: the
// device form has always sent client_cert_path/client_key_path, while the keys
// documented for this driver are certificate_file/private_key_file.
//
// Whitespace is stripped and a blank value counts as unset: Connect refuses a
// secured session that has no client identity, and a path field left as " "
// would clear that check and then fail inside gopcua with an error naming
// neither file.
func opcuaConfigFile(config map[string]interface{}, uiKey, canonicalKey string) string {
	if v := strings.TrimSpace(GetConfigString(config, uiKey, "")); v != "" {
		return v
	}
	return strings.TrimSpace(GetConfigString(config, canonicalKey, ""))
}

// ErrOPCDAUnsupported is returned by every OPC DA operation. OPC DA is a
// Windows COM/DCOM protocol and this build carries no COM client, so a nil from
// WritePoint or a "healthy" HealthCheck would report a missing capability as a
// delivered one.
var ErrOPCDAUnsupported = errors.New("opc_da requires a Windows COM/DCOM bridge, which this build does not include")

// ErrONVIFWriteUnsupported is returned by every ONVIF write. The driver only
// implements ONVIF's read operations, so a successful-looking write must not be
// reported for a control command that was never sent.
var ErrONVIFWriteUnsupported = errors.New("onvif is read-only in this build: no PTZ or media control commands are implemented")

// OPCDADriver stands in for the OPC DA protocol so the protocol is recognised
// by the registry and reports one clear reason for failing, instead of surfacing
// "unsupported protocol" from every layer that touches it.
type OPCDADriver struct {
	BaseDriver
	server  string
	node    string
	timeout time.Duration
	mu      sync.Mutex
}

func NewOPCDADriver(deviceID string, config map[string]interface{}) (Driver, error) {
	d := &OPCDADriver{
		server:  GetConfigString(config, "server", "Matrikon.OPC.Simulation"),
		node:    GetConfigString(config, "node", "localhost"),
		timeout: time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *OPCDADriver) Name() string { return "opc_da" }

func (d *OPCDADriver) Connect(ctx context.Context) error {
	// Reporting "connected" here made every OPC DA device look healthy in the UI
	// while no read or write could ever reach a server.
	d.SetConnectionState(StateDisconnected, ErrOPCDAUnsupported.Error())
	return ErrOPCDAUnsupported
}

func (d *OPCDADriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.SetConnected(false)
	return nil
}

// ReadPoints, WritePoint, Discover and HealthCheck all report the capability
// gap directly. A per-call "not connected" error would imply that retrying, or
// fixing the device config, could succeed.
func (d *OPCDADriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	return nil, ErrOPCDAUnsupported
}

func (d *OPCDADriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	return ErrOPCDAUnsupported
}

func (d *OPCDADriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	return nil, ErrOPCDAUnsupported
}

func (d *OPCDADriver) HealthCheck(ctx context.Context) error {
	return ErrOPCDAUnsupported
}

// ONVIFDriver implements an ONVIF protocol driver for network cameras and NVTs.
// Uses SOAP/WS-Discovery for device discovery and media profile management.
type ONVIFDriver struct {
	BaseDriver
	host     string
	port     int
	username string
	password string
	timeout  time.Duration
	mu       sync.Mutex
}

func NewONVIFDriver(deviceID string, config map[string]interface{}) (Driver, error) {
	d := &ONVIFDriver{
		host:     GetConfigString(config, "host", "127.0.0.1"),
		port:     GetConfigInt(config, "port", 80),
		username: GetConfigString(config, "username", "admin"),
		password: GetConfigString(config, "password", ""),
		timeout:  time.Duration(GetConfigFloat(config, "timeout", float64(constants.DeviceConnectTimeout)) * float64(time.Second)),
	}
	d.SetDeviceID(deviceID)
	d.SetConfig(config)
	return d, nil
}

func (d *ONVIFDriver) Name() string { return "onvif" }

func (d *ONVIFDriver) Connect(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	d.SetConnectionState(StateConnecting, "onvif connecting")

	// Try to connect to the ONVIF device HTTP endpoint
	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	conn, err := net.DialTimeout("tcp", addr, d.timeout)
	if err != nil {
		// Reporting "connected" for an unreachable camera made the device look
		// healthy forever; every read then failed with a confusing HTTP error.
		d.SetConnected(false)
		d.SetConnectionState(StateDisconnected, err.Error())
		d.RecordReadFailure()
		return fmt.Errorf("onvif dial %s: %w", addr, err)
	}
	conn.Close()
	d.SetConnected(true)
	d.SetConnectionState(StateConnected, "onvif connected")
	logrus.WithField("device_id", d.DeviceID()).
		WithField("host", d.host).
		WithField("port", d.port).Debug("ONVIF connected")
	return nil
}

func (d *ONVIFDriver) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.SetConnected(false)
	return nil
}

func (d *ONVIFDriver) ReadPoints(ctx context.Context, points []models.PointDef) ([]storage.PointData, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("onvif not connected")
	}

	// Check circuit breaker
	if d.IsCircuitOpen() {
		return nil, fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	now := time.Now()
	result := make([]storage.PointData, 0, len(points))
	anyFailed := false
	var lastLatencyMs float64

	for _, pt := range points {
		start := time.Now()
		// Attempt to read real ONVIF data via SOAP
		val, err := d.readONVIFPoint(pt)
		lastLatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
		quality := "good"
		if err != nil {
			anyFailed = true
			logrus.WithField("device_id", d.DeviceID()).
				WithField("point", pt.Name).
				WithField("error", err.Error()).
				Warn("ONVIF read failed")
			val = nil
			quality = "bad"
		}
		result = append(result, storage.PointData{
			DeviceID: d.DeviceID(), PointName: pt.Name, Value: val, Quality: quality, Timestamp: now,
		})
	}

	if anyFailed {
		d.RecordReadFailure()
	} else {
		d.RecordReadSuccess(lastLatencyMs)
	}

	return result, nil
}

// readONVIFPoint reads a single point from an ONVIF device via SOAP/HTTP.
// Supported point types: video_status, ptz_status, motion_detection, io_state, device_info
func (d *ONVIFDriver) readONVIFPoint(pt models.PointDef) (interface{}, error) {
	// Build SOAP request based on point type/address
	pointType := pt.Address
	if pointType == "" {
		pointType = pt.Name
	}

	// Build SOAP action URL
	var action string
	soapBody := ""

	switch {
	case indexOf(pointType, "device_info") >= 0 || indexOf(pointType, "DeviceInfo") >= 0:
		action = "http://www.onvif.org/ver10/device/wsdl/GetDeviceInformation"
		soapBody = `<tds:GetDeviceInformation xmlns:tds="http://www.onvif.org/ver10/device/wsdl"/>`
	case indexOf(pointType, "video") >= 0:
		action = "http://www.onvif.org/ver10/media/wsdl/GetProfiles"
		soapBody = `<trt:GetProfiles xmlns:trt="http://www.onvif.org/ver10/media/wsdl"/>`
	case indexOf(pointType, "ptz") >= 0:
		action = "http://www.onvif.org/ver10/ptz/wsdl/GetStatus"
		soapBody = `<tptz:GetStatus xmlns:tptz="http://www.onvif.org/ver10/ptz/wsdl">
		<tptz:ProfileToken>MainProfileToken</tptz:ProfileToken>
	</tptz:GetStatus>`
	case indexOf(pointType, "motion") >= 0:
		action = "http://www.onvif.org/ver10/events/wsdl/GetEventProperties"
		soapBody = `<tev:GetEventProperties xmlns:tev="http://www.onvif.org/ver10/events/wsdl"/>`
	case indexOf(pointType, "io") >= 0:
		action = "http://www.onvif.org/ver10/device/wsdl/Get"
		soapBody = `<tds:Get xmlns:tds="http://www.onvif.org/ver10/device/wsdl"/>`
	default:
		return nil, fmt.Errorf("unknown onvif point type: %s", pointType)
	}

	// Build full SOAP envelope
	soapEnvelope := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
	<s:Header>
		<wsa:Action xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing">%s</wsa:Action>
	</s:Header>
	<s:Body>
		%s
	</s:Body>
</s:Envelope>`, action, soapBody)

	// Build HTTP request
	url := fmt.Sprintf("http://%s/onvif/device_service", net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port)))
	req, err := http.NewRequest("POST", url, bytes.NewBufferString(soapEnvelope))
	if err != nil {
		return nil, fmt.Errorf("onvif request: %w", err)
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	req.Header.Set("SOAPAction", action)

	// Add WS-Security if credentials are provided
	if d.username != "" && d.password != "" {
		nonce := generateNonce()
		created := time.Now().UTC().Format(time.RFC3339)
		digest := computeSoapDigest(nonce, created, d.password)
		req.Header.Set("Authorization", "") // Clear any existing auth
		envelopeWithAuth := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
	<s:Header>
		<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">
			<wsse:UsernameToken>
				<wsse:Username>%s</wsse:Username>
				<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</wsse:Password>
				<wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">%s</wsse:Nonce>
				<wsu:Created xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">%s</wsu:Created>
			</wsse:UsernameToken>
		</wsse:Security>
		<wsa:Action xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing">%s</wsa:Action>
	</s:Header>
	<s:Body>
		%s
	</s:Body>
</s:Envelope>`, d.username, digest, nonce, created, action, soapBody)
		req = mustNewRequest("POST", url, bytes.NewBufferString(envelopeWithAuth))
		req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
		req.Header.Set("SOAPAction", action)
	}

	// Send HTTP request
	client := &http.Client{Timeout: d.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onvif http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("onvif HTTP status: %d", resp.StatusCode)
	}

	// Read and parse SOAP response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("onvif read response: %w", err)
	}

	// Extract value from SOAP response based on point type
	return parseONVIFResponse(string(body), pointType), nil
}

// readONVIFPoint reads a single point from an ONVIF device.
// This is a placeholder for the full SOAP implementation.
func (d *ONVIFDriver) HealthCheck(ctx context.Context) error {
	if !d.IsConnected() {
		return fmt.Errorf("not connected")
	}
	// Simple health check: verify device is reachable via TCP
	addr := net.JoinHostPort(d.host, fmt.Sprintf("%d", d.port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		d.RecordReadFailure()
		return err
	}
	conn.Close()
	d.RecordReadSuccess(latencyMs)
	return nil
}

func (d *ONVIFDriver) WritePoint(ctx context.Context, point string, value interface{}) error {
	if !d.IsConnected() {
		return fmt.Errorf("onvif not connected")
	}

	// Check circuit breaker before write
	if d.IsCircuitOpen() {
		return fmt.Errorf("circuit breaker open for device %s", d.DeviceID())
	}

	// The driver builds read requests only (GetDeviceInformation, GetProfiles,
	// GetStatus, GetEventProperties). Logging a write and returning success made
	// a PTZ command appear to move a camera that never received anything.
	logrus.WithField("device_id", d.DeviceID()).
		WithField("point", point).
		WithField("value", value).
		Warn("ONVIF write rejected: no control commands are implemented")
	d.RecordWriteFailure()
	return ErrONVIFWriteUnsupported
}

// onvifProbeMessage is the WS-Discovery Probe a camera is asked to answer.
//
// The Types element uses the "dn" prefix, so that prefix has to be declared like
// the others: without the declaration the probe is undefined XML, cameras fault it,
// and every scan came back empty while reporting success.
const onvifProbeMessage = `<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope"
               xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing"
               xmlns:wsd="http://schemas.xmlsoap.org/ws/2005/04/discovery"
               xmlns:dn="http://www.onvif.org/ver10/network/wsdl">
  <soap:Header>
    <wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</wsa:Action>
    <wsa:MessageID>urn:uuid:edgelite-probe</wsa:MessageID>
    <wsa:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</wsa:To>
  </soap:Header>
  <soap:Body>
    <wsd:Probe>
      <wsd:Types>dn:NetworkVideoTransmitter</wsd:Types>
    </wsd:Probe>
  </soap:Body>
</soap:Envelope>`

func (d *ONVIFDriver) Discover(ctx context.Context, config map[string]interface{}) ([]map[string]interface{}, error) {
	// WS-Discovery via UDP multicast to 239.255.255.250:3702
	multicastAddr := net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 3702}

	// Create a UDP socket for sending multicast and receiving responses
	localAddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("onvif discovery listen: %w", err)
	}
	defer conn.Close()

	// Send probe to multicast address
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, fmt.Errorf("onvif probe deadline: %w", err)
	}
	if _, err := conn.WriteToUDP([]byte(onvifProbeMessage), &multicastAddr); err != nil {
		return nil, fmt.Errorf("onvif probe write: %w", err)
	}

	// Collect responses for the rest of the discovery window. A camera may answer
	// more than once, so entries are keyed by service address.
	window := onvifDiscoveryWindow
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining < window {
			window = remaining
		}
	}
	if window <= 0 {
		return []map[string]interface{}{}, nil
	}
	devices := make([]map[string]interface{}, 0)
	seen := map[string]bool{}
	buf := make([]byte, 8192)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(window))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // the window elapsed, or a socket error ended the scan
		}
		response := string(buf[:n])
		for _, raw := range parseONVIFDeviceAddresses(response) {
			entry, ok := onvifAddressEntry(raw)
			if !ok || seen[raw] {
				continue
			}
			seen[raw] = true
			devices = append(devices, entry)
		}
	}
	return devices, nil
}

// onvifDiscoveryWindow bounds one WS-Discovery scan.
const onvifDiscoveryWindow = 5 * time.Second

// onvifAddressEntry turns an XAddr URL into the row the device table shows. The
// table and the "add selected device" action read ip/port, so an address without a
// host is dropped rather than listed as a blank row the operator cannot fix.
func onvifAddressEntry(xaddr string) (map[string]interface{}, bool) {
	u, err := url.Parse(strings.TrimSpace(xaddr))
	if err != nil {
		return nil, false
	}
	host := u.Hostname()
	if host == "" {
		return nil, false
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			port = n
		}
	}
	return map[string]interface{}{
		"ip":        host,
		"port":      port,
		"address":   strings.TrimSpace(xaddr),
		"protocol":  "onvif",
		"device_id": fmt.Sprintf("onvif-%s-%d", host, port),
		"name":      fmt.Sprintf("%s:%d", host, port),
	}, true
}

// parseONVIFDeviceAddresses extracts every service address from a WS-Discovery
// response. XAddrs may carry attributes and may list several URLs separated by
// whitespace, both of which the previous single-tag search silently dropped.
func parseONVIFDeviceAddresses(soapResponse string) []string {
	var out []string
	rest := soapResponse
	for {
		startIdx := indexOf(rest, "<XAddrs")
		if startIdx < 0 {
			return out
		}
		gt := indexOf(rest[startIdx:], ">")
		if gt < 0 {
			return out
		}
		startIdx += gt + 1
		endIdx := indexOf(rest[startIdx:], "</XAddrs>")
		if endIdx <= 0 {
			rest = rest[startIdx:]
			continue
		}
		out = append(out, strings.Fields(rest[startIdx:startIdx+endIdx])...)
		rest = rest[startIdx+endIdx:]
	}
}

// parseONVIFDeviceAddress extracts the first service address from a WS-Discovery
// response. It is the single-address view of parseONVIFDeviceAddresses.
func parseONVIFDeviceAddress(soapResponse string) (string, bool) {
	addrs := parseONVIFDeviceAddresses(soapResponse)
	if len(addrs) == 0 {
		return "", false
	}
	return addrs[0], true
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if s[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// parseOPCUAEndpoint parses an OPC UA endpoint URL to extract host and port.
// Example: "opc.tcp://192.168.1.100:4840" -> ("192.168.1.100", 4840)
func parseOPCUAEndpoint(endpoint string) (string, int) {
	// Remove protocol prefix
	host := endpoint
	if idx := indexOf(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	// Split path
	if idx := indexOf(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	// Parse host:port
	port := 4840
	if idx := indexOf(host, ":"); idx >= 0 {
		portStr := host[idx+1:]
		host = host[:idx]
		if p, err := strconv.Atoi(portStr); err == nil {
			port = p
		}
	}
	return host, port
}

// generateSimulatedValue generates a realistic simulated value based on point configuration.
// Uses the point's min/max/scale/offset and data type to produce appropriate values.
func generateSimulatedValue(pt models.PointDef) interface{} {
	minVal := 0.0
	maxVal := 100.0
	if pt.Min != nil {
		minVal = *pt.Min
	}
	if pt.Max != nil {
		maxVal = *pt.Max
	}

	// Generate a value in the middle of the range with some variation
	base := (minVal + maxVal) / 2
	amplitude := (maxVal - minVal) / 4
	if amplitude == 0 {
		amplitude = 1
	}
	// Use time-based pseudo-random for deterministic but varying output
	t := time.Now().UnixNano()
	sineVal := base + amplitude*mathSin(float64(t)/1e9)

	// Apply scale and offset
	if pt.Scale != nil {
		sineVal *= *pt.Scale
	}
	if pt.Offset != nil {
		sineVal += *pt.Offset
	}

	// Convert to appropriate type based on data type
	dataType := pt.DataType
	if dataType == "" {
		dataType = "float64"
	}
	switch dataType {
	case "int16", "short":
		return int16(sineVal)
	case "uint16", "word":
		return uint16(sineVal)
	case "int32", "long":
		return int32(sineVal)
	case "uint32", "dword":
		return uint32(sineVal)
	case "float32", "float", "real":
		return float32(sineVal)
	case "bool":
		return int(sineVal)%2 == 0
	default:
		return sineVal
	}
}

// mathSin computes sine using math package (wrapper for testability).
func mathSin(x float64) float64 {
	return math.Sin(x)
}

// generateNonce generates a random nonce for WS-Security.
func generateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// computeSoapDigest computes the WS-Security password digest.
// Digest = Base64(SHA1(nonce + created + password))
func computeSoapDigest(nonce, created, password string) string {
	// Decode nonce from base64
	nonceBytes, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		nonceBytes = []byte(nonce)
	}
	// Concatenate nonce + created + password
	raw := append(nonceBytes, []byte(created)...)
	raw = append(raw, []byte(password)...)
	// Compute SHA-1
	hash := sha1.Sum(raw)
	return base64.StdEncoding.EncodeToString(hash[:])
}

// mustNewRequest creates a new HTTP request and panics on error.
// Used for re-creating request with WS-Security header.
func mustNewRequest(method, url string, body io.Reader) *http.Request {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		panic(fmt.Sprintf("failed to create request: %v", err))
	}
	return req
}

// parseONVIFResponse extracts a value from an ONVIF SOAP response.
// Returns a string representation of the relevant data.
func parseONVIFResponse(soapResponse, pointType string) interface{} {
	// For device_info: extract Manufacturer, Model, SerialNumber
	// For video: extract profile count
	// For ptz: extract Pan/Tilt/Zoom status
	// For motion: extract motion alarm state
	// For io: extract digital input state

	switch {
	case indexOf(pointType, "device_info") >= 0 || indexOf(pointType, "DeviceInfo") >= 0:
		// Extract manufacturer
		if startIdx := indexOf(soapResponse, "<tds:Manufacturer>"); startIdx >= 0 {
			startIdx += len("<tds:Manufacturer>")
			endIdx := indexOf(soapResponse[startIdx:], "</tds:Manufacturer>")
			if endIdx > 0 {
				return soapResponse[startIdx : startIdx+endIdx]
			}
		}
		return "unknown"
	case indexOf(pointType, "video") >= 0:
		// Count profile elements
		count := 0
		searchStr := "<trt:Profiles "
		idx := 0
		for {
			found := indexOf(soapResponse[idx:], searchStr)
			if found < 0 {
				break
			}
			count++
			idx += found + len(searchStr)
		}
		return count
	case indexOf(pointType, "ptz") >= 0:
		// Extract PTZ status - return "idle" if no movement
		if indexOf(soapResponse, "Status") >= 0 {
			return "idle"
		}
		return "unknown"
	case indexOf(pointType, "motion") >= 0:
		// Check for motion alarm
		if indexOf(soapResponse, "Motion") >= 0 || indexOf(soapResponse, "motion") >= 0 {
			return true
		}
		return false
	case indexOf(pointType, "io") >= 0:
		// Extract I/O state
		if indexOf(soapResponse, "idle") >= 0 {
			return false
		}
		return true
	default:
		return soapResponse
	}
}
