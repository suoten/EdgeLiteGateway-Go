package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The point editor browses an endpoint that is still only in the form, so the
// config-only branch of /drivers/opcua/browse is the one that carries the
// feature. These pin down that it really opens the session it was asked for,
// rather than answering with the empty list a stub would return.

func TestHandleOPCUABrowseUnsavedEndpointDialsWhatItIsTold(t *testing.T) {
	withDeviceService(t)

	// Nothing to browse with: neither a saved device nor an endpoint.
	none, noneRec := setupWithAdmin("POST", "/api/v1/drivers/opcua/browse", `{"node_id":"i=84"}`)
	if err := handleOPCUABrowse(none); err != nil {
		t.Fatalf("error: %v", err)
	}
	if noneRec.Code != http.StatusBadRequest {
		t.Fatalf("400 expected with no device and no config, got %d (%s)", noneRec.Code, noneRec.Body.String())
	}

	// A refused endpoint must be a gateway error. An empty 200 here is what a
	// handler that never connected would look like, and the UI cannot tell the
	// two apart from a node list.
	dead, deadRec := setupWithAdmin("POST", "/api/v1/drivers/opcua/browse",
		`{"config":{"endpoint":"opc.tcp://127.0.0.1:1","session_timeout":1500},"node_id":"i=84"}`)
	if err := handleOPCUABrowse(dead); err != nil {
		t.Fatalf("error: %v", err)
	}
	if deadRec.Code != http.StatusBadGateway {
		t.Fatalf("502 expected when the ad-hoc session cannot open, got %d (%s)", deadRec.Code, deadRec.Body.String())
	}
	if !strings.Contains(deadRec.Body.String(), "ERR_DRIVER_BROWSE_FAILED") {
		t.Fatalf("response must carry ERR_DRIVER_BROWSE_FAILED, got %s", deadRec.Body.String())
	}
	// The message has to be the connection failure itself. Swallowing the Connect
	// error still yields a 502, but from the next call, which reports "opc ua not
	// connected" - an operator reading that would look for a device that went
	// down, not for the endpoint they just mistyped.
	if !strings.Contains(deadRec.Body.String(), "connect failed") {
		t.Fatalf("response must name the connect failure, got %s", deadRec.Body.String())
	}
	if !strings.Contains(deadRec.Body.String(), "127.0.0.1:1") {
		t.Fatalf("response must name the endpoint it could not reach, got %s", deadRec.Body.String())
	}
}

func TestHandleOPCUABrowseUnsavedEndpointAgainstProtoForge(t *testing.T) {
	if !protoForgeLive() {
		t.Skip("ProtoForge is not running (management API :8000 or OPC-UA :4840 closed)")
	}
	withDeviceService(t)

	// This is the create-device flow: the operator has typed an endpoint and not
	// saved anything, and Browse has to answer with that server's real nodes.
	top := browseUnsaved(t, `{"config":{"endpoint":"opc.tcp://127.0.0.1:4840/protoforge","session_timeout":5000}}`)
	if len(top) == 0 {
		t.Fatalf("ProtoForge exposes nodes but the root browse returned an empty list")
	}
	// The Objects folder holds folders, so the first response proves the walk can
	// start; descending into the server's own (non-zero namespace) folder is what
	// yields tags. Namespace 0 is the standard address space, never process data.
	var device *browseEntry
	for i := range top {
		e := &top[i]
		if e.IsContainer && strings.HasPrefix(e.NodeID, "ns=") && !strings.HasPrefix(e.NodeID, "ns=0;") {
			device = e
			break
		}
	}
	if device == nil {
		t.Fatalf("root browse returned %d entries and none of them a vendor folder: %v", len(top), top)
	}

	children := browseUnsaved(t, fmt.Sprintf(
		`{"config":{"endpoint":"opc.tcp://127.0.0.1:4840/protoforge","session_timeout":5000},"node_id":%q}`,
		device.NodeID))
	variables, typed, writable := 0, 0, 0
	for _, e := range children {
		if e.NodeClass == "variable" {
			variables++
		}
		if e.DataType != "" {
			typed++
		}
		if e.Writable {
			writable++
		}
	}
	// A variable with no data type cannot be added as a point of the right type,
	// and a writable flag the server never granted would let the UI offer a write
	// that the device refuses. Both are read from the device here, not stored.
	if variables == 0 || typed != variables {
		t.Fatalf("browse of %s returned %d entries, %d variables, %d typed - the picker needs all of them",
			device.NodeID, len(children), variables, typed)
	}
	if writable == 0 {
		t.Errorf("browse of %s reported no writable node, so the point editor cannot offer a write tag",
			device.NodeID)
	}
	t.Logf("browse %s: %d entries, %d variables, %d typed, %d writable -> %v",
		device.NodeID, len(children), variables, typed, writable, children)
}

// browseEntry mirrors the wire shape of one drivers.BrowseEntry, and is declared
// here so a change to the API contract fails this test rather than the UI.
type browseEntry struct {
	NodeID      string `json:"node_id"`
	BrowseName  string `json:"browse_name"`
	DisplayName string `json:"display_name"`
	NodeClass   string `json:"node_class"`
	DataType    string `json:"data_type"`
	Writable    bool   `json:"writable"`
	IsContainer bool   `json:"is_container"`
}

func browseUnsaved(t *testing.T, body string) []browseEntry {
	t.Helper()
	c, rec := setupWithAdmin("POST", "/api/v1/drivers/opcua/browse", body)
	if err := handleOPCUABrowse(c); err != nil {
		t.Fatalf("error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("200 expected browsing ProtoForge, got %d (%s)", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data []browseEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not the {code,message,data} envelope: %v (%s)", err, rec.Body.String())
	}
	return envelope.Data
}

// protoForgeLive requires the management API port, which EdgeLite's gateway never
// binds, so an open 8000 cannot be the gateway masquerading as ProtoForge.
func protoForgeLive() bool {
	return portOpen("127.0.0.1:8000") && portOpen("127.0.0.1:4840")
}

func portOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
