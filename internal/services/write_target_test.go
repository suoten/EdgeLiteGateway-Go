package services

import (
	"context"
	"strings"
	"testing"

	"edgelite/internal/drivers"
)

// The service is the only place that holds both the operator's point name and
// the wire address, so it is also the only place that can hand each driver the
// one it understands. Getting this wrong is not a rejection but a misdirected
// write: a Modbus driver handed the name "pf_mb" answers "invalid point address",
// while an OPC UA driver parses the name as a NodeID and writes a node that has
// nothing to do with the point.

// addrDriver speaks the wire-address contract on top of the name-keyed fake.
type addrDriver struct {
	policyDriver
	lastAddress string
	lastType    string
}

func (d *addrDriver) WritePointAtAddress(ctx context.Context, address string, value interface{}, dataType string) error {
	d.lastAddress = address
	d.lastType = dataType
	return d.WritePoint(ctx, address, value)
}

func TestWriteDriverPointRoutesByAddress(t *testing.T) {
	t.Run("an address-speaking driver gets the address", func(t *testing.T) {
		drv := &addrDriver{}
		drv.SetDeviceID("addr")
		drv.SetConnected(true)
		if err := writeDriverPoint(context.Background(), drv, "pf_mb", 42.5, "float32", "HR620"); err != nil {
			t.Fatalf("write returned %v", err)
		}
		if drv.lastAddress != "HR620" {
			t.Errorf("driver addressed %q, want the point's wire address HR620", drv.lastAddress)
		}
		if drv.lastType != "float32" {
			t.Errorf("data type = %q, want float32 to reach the driver too", drv.lastType)
		}
	})

	// A point whose address was never filled in has to keep working the way it
	// did before: the name is the only handle there is.
	t.Run("no address falls back to the name", func(t *testing.T) {
		drv := &addrDriver{}
		drv.SetDeviceID("addr")
		drv.SetConnected(true)
		if err := writeDriverPoint(context.Background(), drv, "pf_mb", 1.0, "int32", ""); err != nil {
			t.Fatalf("write returned %v", err)
		}
		if len(drv.written) != 1 {
			t.Fatalf("driver writes = %v, want the fallback to reach the driver", drv.written)
		}
		if drv.lastAddress != "" {
			t.Errorf("address = %q, want the name path with an empty address", drv.lastAddress)
		}
	})

	// The simulator holds written values under the point name and reads them
	// back by name, so handing it an address would park the value where nothing
	// looks for it. It must keep receiving the name.
	t.Run("a name-keyed driver still gets the name", func(t *testing.T) {
		drv := newPolicyDriver()
		drv.SetConnected(true)
		if _, speaks := any(drv).(drivers.WireAddressWriter); speaks {
			t.Fatal("the name-keyed fake must not implement WireAddressWriter")
		}
		if err := writeDriverPoint(context.Background(), drv, "setpoint", 7.0, "float32", "HR620"); err != nil {
			t.Fatalf("write returned %v", err)
		}
		if len(drv.written) != 1 || drv.written[0] != 7.0 {
			t.Fatalf("driver writes = %v, want the value written by name", drv.written)
		}
	})
}

// Every driver that claims the contract has to reach the same code path its
// plain write uses; a wrapper that silently ignored the address would turn the
// fix into a write to nowhere.
func TestWireAddressWritersAddressTheWire(t *testing.T) {
	// parseS7Address / parseModbusAddress / parseFINSAddress / parseMCAddress all
	// reject a bare name, so writing by name would error out. The point of this
	// test is that the drivers that implement the interface accept an address.
	cases := []struct {
		protocol string
		address  string
		value    interface{}
		build    func() (drivers.Driver, error)
	}{
		{"modbus_tcp", "HR620", uint16(7), func() (drivers.Driver, error) {
			return drivers.NewModbusTCPDriver("addr-modbus", map[string]interface{}{"host": "127.0.0.1", "port": 1})
		}},
		{"siemens_s7", "DB1.DBW0", int16(7), func() (drivers.Driver, error) {
			return drivers.NewS7Driver("addr-s7", map[string]interface{}{"ip": "127.0.0.1"})
		}},
		{"omron_fins", "D100", int16(7), func() (drivers.Driver, error) {
			return drivers.NewFINSDriver("addr-fins", map[string]interface{}{"host": "127.0.0.1"})
		}},
		{"mitsubishi_mc", "D100", int16(7), func() (drivers.Driver, error) {
			return drivers.NewMCDriver("addr-mc", map[string]interface{}{"host": "127.0.0.1"})
		}},
		{"allen_bradley", "Program:Main.Tag", int16(7), func() (drivers.Driver, error) {
			return drivers.NewABDriver("addr-ab", map[string]interface{}{"host": "127.0.0.1"})
		}},
		{"modbus_rtu", "HR620", uint16(7), func() (drivers.Driver, error) {
			return drivers.NewModbusRTUDriver("addr-rtu", map[string]interface{}{"port": "COM1"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			drv, err := tc.build()
			if err != nil {
				t.Fatalf("create driver: %v", err)
			}
			aw, ok := drv.(drivers.WireAddressWriter)
			if !ok {
				t.Fatalf("%s does not implement WireAddressWriter, so writes arrive as a point name", tc.protocol)
			}
			// Not connected: the driver has to fail at the connection check, not
			// at address parsing, which is what proves the address was accepted.
			err = aw.WritePointAtAddress(context.Background(), tc.address, tc.value, "")
			if err == nil {
				t.Fatalf("writing while disconnected returned nil, want a connection error")
			}
			msg := err.Error()
			for _, bad := range []string{"invalid address", "invalid point address", "parse"} {
				if containsFold(msg, bad) {
					t.Errorf("%s rejected the address %q (%s); the interface must receive what the parser accepts", tc.protocol, tc.address, msg)
				}
			}
			if !containsFold(msg, "not connected") && !containsFold(msg, "circuit breaker") {
				t.Errorf("%s error = %q, want the write to stop at the missing connection", tc.protocol, msg)
			}
		})
	}
}

// opc_ua parses any string as a NodeID, so it cannot be tested with a
// disconnected driver; this pins the address straight into the NodeID it dials.
func TestOPCUAAddressWriteUsesTheNodeGiven(t *testing.T) {
	drv, err := drivers.NewOPCUADriver("addr-opcua", map[string]interface{}{
		"endpoint": "opc.tcp://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("create driver: %v", err)
	}
	aw, ok := drv.(drivers.WireAddressWriter)
	if !ok {
		t.Fatal("opc_ua must implement WireAddressWriter: a point name is not a NodeID")
	}
	if err := aw.WritePointAtAddress(context.Background(), "ns=2;s=pf-opcua.temp", 1.0, "float32"); err == nil {
		t.Fatal("writing to an unreachable server returned nil, want an error")
	} else if msg := err.Error(); !containsFold(msg, "not connected") && !containsFold(msg, "connect") {
		t.Errorf("error = %q, want a connection failure rather than a parse one", msg)
	}
	// The name-keyed entry point has to stay intact for configs that do carry a
	// point list: the interface is additive, not a replacement.
	if err := drv.WritePoint(context.Background(), "temp", 1.0); err == nil {
		t.Error("disconnected WritePoint returned nil")
	}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
