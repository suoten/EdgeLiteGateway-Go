package drivers

import (
	"strings"
	"testing"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
)

// Hermetic coverage for the OPC UA browse mapping layer. BrowseChildren is the
// read-only service that fills the point editor's node picker, and whether a
// node is offered at all rests on these small pure functions: the AccessLevel
// bit that means "writable", the DataType text the point is typed with, and the
// NodeClass that decides whether the UI shows a folder to descend into. None of
// them had a test, so each could return the wrong answer silently and the only
// live joint test would just skip.

func varRef(ns uint16, name, nodeText string) *ua.ReferenceDescription {
	return &ua.ReferenceDescription{
		NodeID:      ua.NewExpandedNodeID(ua.NewStringNodeID(ns, nodeText), "", 0),
		BrowseName:  &ua.QualifiedName{NamespaceIndex: ns, Name: name},
		DisplayName: ua.NewLocalizedText("Temperature Sensor"),
		NodeClass:   ua.NodeClassVariable,
	}
}

func TestOPCUABrowseEntryMapsVariableAndContainerSeparately(t *testing.T) {
	v, ok := browseEntryFromRef(varRef(2, "temp", "pf-opcua.temp"))
	if !ok {
		t.Fatal("a complete variable reference must be listed")
	}
	if v.NodeID != "ns=2;s=pf-opcua.temp" {
		t.Errorf("NodeID = %q, want the text ReadPoints parses back", v.NodeID)
	}
	if v.NodeClass != "variable" {
		t.Errorf("NodeClass = %q, want %q", v.NodeClass, "variable")
	}
	// IsContainer drives "browse deeper"; a variable must never offer a drill-in.
	if v.IsContainer {
		t.Error("a variable must not be a container")
	}
	if v.BrowseName != "2:temp" {
		t.Errorf("BrowseName = %q, want %q (namespace-qualified)", v.BrowseName, "2:temp")
	}
	if v.DisplayName != "Temperature Sensor" {
		t.Errorf("DisplayName = %q, want the server's display name", v.DisplayName)
	}

	o, ok := browseEntryFromRef(&ua.ReferenceDescription{
		NodeID:    ua.NewExpandedNodeID(ua.NewNumericNodeID(2, 1), "", 0),
		NodeClass: ua.NodeClassObject,
	})
	if !ok {
		t.Fatal("a complete object reference must be listed")
	}
	if o.NodeClass != "object" || !o.IsContainer {
		t.Errorf("object entry = %+v, want NodeClass object and IsContainer true", o)
	}
	// A reference with no names still has to be listable: the picker shows the
	// NodeID, and dropping the node would hide a tag the server does expose.
	if o.BrowseName != "" || o.DisplayName != "" {
		t.Errorf("missing names must map to empty strings, got %q / %q", o.BrowseName, o.DisplayName)
	}
}

func TestOPCUABrowseEntryDropsIncompleteAndCrossServerReferences(t *testing.T) {
	cases := []struct {
		name string
		ref  *ua.ReferenceDescription
	}{
		{"nil reference", nil},
		{"no target node", &ua.ReferenceDescription{NodeClass: ua.NodeClassVariable}},
		{"expanded wrapper without a node id", &ua.ReferenceDescription{
			NodeID:    &ua.ExpandedNodeID{},
			NodeClass: ua.NodeClassVariable,
		}},
		// A server-index reference points into another server's address space,
		// which this session cannot read; listing it would hand the operator a
		// node that reads bad forever.
		{"reference on another server", &ua.ReferenceDescription{
			NodeID:    ua.NewExpandedNodeID(ua.NewStringNodeID(2, "remote"), "", 1),
			NodeClass: ua.NodeClassVariable,
		}},
	}
	for _, tc := range cases {
		if e, ok := browseEntryFromRef(tc.ref); ok {
			t.Errorf("%s: dropped, want ok=false, got %+v", tc.name, e)
		}
	}
}

func TestOPCUANodeClassTextOnlyClaimsTheTwoClassesTheUIUses(t *testing.T) {
	for _, tc := range []struct {
		in   ua.NodeClass
		want string
	}{
		{ua.NodeClassVariable, "variable"},
		{ua.NodeClassObject, "object"},
		// Method/Type/View references reach browse results on real servers; the
		// picker must not label one of those a readable tag.
		{ua.NodeClassMethod, "other"},
		{ua.NodeClass(0), "other"},
	} {
		if got := opcuaNodeClassText(tc.in); got != tc.want {
			t.Errorf("opcuaNodeClassText(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOPCUADataTypeTextNamesStandardTypesAndPassesVendorTypesThrough(t *testing.T) {
	// ns=0 numeric ids name a specification datatype, so the point editor can
	// show "Float" instead of a number the operator has to look up.
	if got := opcuaDataTypeText(ua.NewNumericNodeID(0, id.Float)); got != "Float" {
		t.Errorf("Float = %q, want %q", got, "Float")
	}
	if got := opcuaDataTypeText(ua.NewNumericNodeID(0, id.Boolean)); got != "Boolean" {
		t.Errorf("Boolean = %q, want %q", got, "Boolean")
	}
	// A vendor datatype has no spec name: pass the id through rather than
	// reporting an empty string, which would read as "type unknown".
	if got := opcuaDataTypeText(ua.NewNumericNodeID(2, 99)); got != "ns=2;i=99" {
		t.Errorf("vendor datatype = %q, want %q", got, "ns=2;i=99")
	}
	// AccessLevel arrives as a Byte and DataType as a NodeId; anything else means
	// the attribute read returned something unexpected, which is "unknown".
	if got := opcuaDataTypeText("Float"); got != "" {
		t.Errorf("non-NodeId value = %q, want the empty (unknown) marker", got)
	}
	if got := opcuaDataTypeText(nil); got != "" {
		t.Errorf("nil value = %q, want the empty (unknown) marker", got)
	}
}

func TestOPCUAAccessLevelAcceptsBothIntegerWidths(t *testing.T) {
	// gopcua decodes a Byte to uint8 but a server that sends the attribute as an
	// Int64 (or a variant decoded through the generic path) must still grant the
	// write permission rather than silently marking the node read-only.
	if al, ok := accessLevelByte(uint8(7)); !ok || al != 7 {
		t.Errorf("uint8(7) = %#x/%v, want 7/true", al, ok)
	}
	if al, ok := accessLevelByte(int64(3)); !ok || al != 3 {
		t.Errorf("int64(3) = %#x/%v, want 3/true", al, ok)
	}
	for _, v := range []interface{}{"7", int32(7), nil, 7} {
		if _, ok := accessLevelByte(v); ok {
			t.Errorf("accessLevelByte(%#v) accepted a value that is not a byte-width access level", v)
		}
	}
}

func TestParseOPCUASecurityModeAcceptsEverySpellingTheFormSends(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ua.MessageSecurityMode
	}{
		{"", ua.MessageSecurityModeNone},
		{"None", ua.MessageSecurityModeNone},
		{"  none  ", ua.MessageSecurityModeNone},
		{"Sign", ua.MessageSecurityModeSign},
		{"sign", ua.MessageSecurityModeSign},
		{"SignAndEncrypt", ua.MessageSecurityModeSignAndEncrypt},
		// The UI label and Kepware-style configs separate the words; all of these
		// must reach mode 3, because ua.MessageSecurityModeFromString would hand
		// mode 0 to the server while the UI still displayed SignAndEncrypt.
		{"Sign And Encrypt", ua.MessageSecurityModeSignAndEncrypt},
		{"sign-and-encrypt", ua.MessageSecurityModeSignAndEncrypt},
		{"SIGN_AND_ENCRYPT", ua.MessageSecurityModeSignAndEncrypt},
		{"SignEncrypt", ua.MessageSecurityModeSignAndEncrypt},
	} {
		got, err := parseOPCUASecurityMode(tc.in)
		if err != nil {
			t.Errorf("parseOPCUASecurityMode(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseOPCUASecurityMode(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// An unrecognised mode must be refused at construction: the alternative is a
	// session that fails with an opaque error on every collect tick.
	for _, bad := range []string{"encrypted", "Basic256Sha256", "SignOrEncrypt", "NoneAndSign"} {
		if m, err := parseOPCUASecurityMode(bad); err == nil {
			t.Errorf("parseOPCUASecurityMode(%q) = %d, want an error", bad, m)
		}
	}
}

func TestOPCUACertFileReadsTheFormKeyAndTheDocumentedKey(t *testing.T) {
	// The device form has always written client_cert_path while the documented
	// driver key is certificate_file; a driver that read only one of them would
	// connect with no certificate under the spelling the operator typed.
	if got := opcuaConfigFile(map[string]interface{}{"client_cert_path": "/p/form.pem"}, "client_cert_path", "certificate_file"); got != "/p/form.pem" {
		t.Errorf("form key = %q, want /p/form.pem", got)
	}
	if got := opcuaConfigFile(map[string]interface{}{"certificate_file": "/p/doc.pem"}, "client_cert_path", "certificate_file"); got != "/p/doc.pem" {
		t.Errorf("documented key = %q, want /p/doc.pem", got)
	}
	if got := opcuaConfigFile(map[string]interface{}{
		"client_cert_path": "/p/form.pem", "certificate_file": "/p/doc.pem",
	}, "client_cert_path", "certificate_file"); got != "/p/form.pem" {
		t.Errorf("both keys = %q, want the one the UI wrote", got)
	}
	// A path present but blank is not a certificate; it must not satisfy the
	// "security mode needs a cert" check with an empty string either.
	if got := opcuaConfigFile(map[string]interface{}{"client_cert_path": "  ", "certificate_file": "/p/doc.pem"}, "client_cert_path", "certificate_file"); got != "/p/doc.pem" {
		t.Errorf("blank form key = %q, want the fallback /p/doc.pem", got)
	}
	if got := opcuaConfigFile(map[string]interface{}{}, "client_cert_path", "certificate_file"); got != "" {
		t.Errorf("no key at all = %q, want empty so Connect reports the missing cert", got)
	}
}

func TestOPCUASecureModeWithoutClientIdentityFailsWithTheReason(t *testing.T) {
	// The failure this guards against is the misleading one: gopcua answers "no
	// certificate", which reads like a server fault, so Connect must refuse first
	// and name the two config keys that fix it.
	d, err := NewOPCUADriver("opc-cert", map[string]interface{}{
		"endpoint": "opc.tcp://127.0.0.1:1", "security_mode": "SignAndEncrypt",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = d.Connect(t.Context())
	if err == nil {
		t.Fatal("SignAndEncrypt with no client certificate must not report success")
	}
	for _, want := range []string{"certificate_file", "private_key_file", "SignAndEncrypt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q, so the operator cannot act on it", err.Error(), want)
		}
	}
}
