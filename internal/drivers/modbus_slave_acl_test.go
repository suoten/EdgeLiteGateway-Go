package drivers

// The slave driver runs a TCP server that publishes the gateway register map.
// Before these fields were wired, every access-control knob the device form
// shows (allowed_ips / max_connections / abuse_*) was accepted by the API and
// then dropped, so a host on any network could read and write process data.
// These tests drive real sockets, because the only thing that matters is what
// the listener does with an incoming connection.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// slaveRequest performs one Modbus TCP read-holding-registers transaction and
// returns the exception-free register values, or the transport error.
func slaveRequest(conn net.Conn, qty int) ([]uint16, error) {
	req := make([]byte, 12)
	binary.BigEndian.PutUint16(req[0:2], 0x0001) // transaction id
	binary.BigEndian.PutUint16(req[2:4], 0)      // protocol id
	binary.BigEndian.PutUint16(req[4:6], 6)      // length: unit + fc + addr + qty
	req[6] = 1                                   // unit id
	req[7] = 0x03                                // read holding registers
	binary.BigEndian.PutUint16(req[8:10], 0)     // start address
	binary.BigEndian.PutUint16(req[10:12], uint16(qty))
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	head := make([]byte, 9)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, err
	}
	if head[7] != 0x03 {
		return nil, fmt.Errorf("slave answered with fc 0x%02x code %d, not a register response", head[7], head[8])
	}
	body := make([]byte, int(head[8]))
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	regs := make([]uint16, len(body)/2)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(body[i*2:])
	}
	return regs, nil
}

func startSlaveDriver(t *testing.T, config map[string]interface{}) (*ModbusSlaveDriver, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	host, portStr, _ := net.SplitHostPort(addr)
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	cfg := map[string]interface{}{"host": host, "port": port}
	for k, v := range config {
		cfg[k] = v
	}
	d, err := NewModbusSlaveDriver("slave-acl", cfg)
	if err != nil {
		t.Fatalf("create driver: %v", err)
	}
	sd := d.(*ModbusSlaveDriver)
	if err := sd.Connect(context.Background()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = sd.Disconnect() })
	return sd, addr
}

func dialSlave(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestParseSlaveAccess(t *testing.T) {
	cases := []struct {
		name    string
		config  map[string]interface{}
		wantErr string
	}{
		{name: "empty config allows everyone", config: map[string]interface{}{}},
		{name: "comma separated list", config: map[string]interface{}{"allowed_ips": "10.1.1.1, 10.2.0.0/16"}},
		{name: "array", config: map[string]interface{}{"allowed_ips": []interface{}{"10.1.1.1", "fe80::/10"}}},
		{name: "semicolon separated", config: map[string]interface{}{"allowed_ips": "10.1.1.1;10.1.1.2"}},
		{name: "bad CIDR", config: map[string]interface{}{"allowed_ips": "10.2.0.0/x"}, wantErr: "not a valid CIDR"},
		{name: "hostname", config: map[string]interface{}{"allowed_ips": "scada.local"}, wantErr: "hostnames are not supported"},
		{name: "negative max_connections", config: map[string]interface{}{"max_connections": -1}, wantErr: "max_connections"},
		{name: "negative abuse_threshold", config: map[string]interface{}{"abuse_threshold": -5}, wantErr: "abuse_threshold"},
		{name: "threshold without window", config: map[string]interface{}{"abuse_threshold": 10, "abuse_window": 0}, wantErr: "abuse_window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSlaveAccess(tc.config)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestModbusSlaveServesWhitelistedPeer(t *testing.T) {
	sd, addr := startSlaveDriver(t, map[string]interface{}{"allowed_ips": "127.0.0.1, 10.0.0.0/8"})
	sd.mu.Lock()
	sd.holdingRegs[0], sd.holdingRegs[1] = 7, 9
	sd.mu.Unlock()
	conn := dialSlave(t, addr)
	regs, err := slaveRequest(conn, 2)
	if err != nil {
		t.Fatalf("request from a whitelisted peer: %v", err)
	}
	if len(regs) != 2 || regs[0] != 7 || regs[1] != 9 {
		t.Fatalf("registers = %v, want [7 9]", regs)
	}
}

func TestModbusSlaveRejectsPeerOutsideWhitelist(t *testing.T) {
	// 10.0.0.0/8 cannot match the loopback peer, so the listener must close the
	// connection before answering a single PDU.
	sd, addr := startSlaveDriver(t, map[string]interface{}{"allowed_ips": "10.0.0.0/8"})
	if sd == nil {
		t.Fatal("nil driver")
	}
	conn := dialSlave(t, addr)
	if _, err := slaveRequest(conn, 1); err == nil {
		t.Fatal("a peer outside allowed_ips was served")
	} else if !isClosedByPeer(err) {
		t.Fatalf("error = %v, want the connection to be closed by the slave", err)
	}
}

// isClosedByPeer reports that a read ended because the other side went away.
// A connection the slave drops before answering arrives as io.EOF on Unix, while
// Windows reports whichever of WSAECONNRESET and WSAECONNABORTED its stack reaches
// first -- which one depends on the moment the listener closes the accepted
// socket, so both count. syscall.ECONNRESET cannot be used as the test: Go defines
// it as 0x20000017 whereas the socket layer reports the raw WSA number (10054).
func isClosedByPeer(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case 10053, 10054, syscall.ECONNRESET, syscall.ECONNABORTED:
			return true
		}
	}
	return false
}

func TestModbusSlaveEmptyWhitelistAllowsAnyPeer(t *testing.T) {
	_, addr := startSlaveDriver(t, nil)
	conn := dialSlave(t, addr)
	if _, err := slaveRequest(conn, 1); err != nil {
		t.Fatalf("an unset whitelist must keep serving (documented behaviour): %v", err)
	}
}

func TestModbusSlaveEnforcesMaxConnections(t *testing.T) {
	_, addr := startSlaveDriver(t, map[string]interface{}{"max_connections": 1})
	first := dialSlave(t, addr)
	if _, err := slaveRequest(first, 1); err != nil {
		t.Fatalf("first peer should be served: %v", err)
	}
	second := dialSlave(t, addr)
	if _, err := slaveRequest(second, 1); err == nil {
		t.Fatal("second peer was served past max_connections")
	}
	// The slot has to be released on disconnect, not just on new accepts.
	first.Close()
	deadline := time.Now().Add(2 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		third := dialSlave(t, addr)
		if _, err = slaveRequest(third, 1); err == nil {
			return
		}
		third.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("connection slot was never released: %v", err)
}

func TestModbusSlaveAbuseThresholdBansPeer(t *testing.T) {
	_, addr := startSlaveDriver(t, map[string]interface{}{
		"abuse_threshold": 2,
		"abuse_window":    60,
		"ban_duration":    3600,
	})
	conn := dialSlave(t, addr)
	for i := 0; i < 2; i++ {
		if _, err := slaveRequest(conn, 1); err != nil {
			t.Fatalf("request %d inside the budget failed: %v", i+1, err)
		}
	}
	// The third request crosses the threshold: the connection must be dropped.
	if _, err := slaveRequest(conn, 1); err == nil {
		t.Fatal("the peer kept serving after exceeding abuse_threshold")
	}
	banned := dialSlave(t, addr)
	if _, err := slaveRequest(banned, 1); err == nil {
		t.Fatal("a banned peer was served on a fresh connection")
	}
}

func TestModbusSlaveBanExpires(t *testing.T) {
	_, addr := startSlaveDriver(t, map[string]interface{}{
		"abuse_threshold": 1,
		"abuse_window":    60,
		"ban_duration":    1,
	})
	conn := dialSlave(t, addr)
	if _, err := slaveRequest(conn, 1); err != nil {
		t.Fatalf("first request should be served: %v", err)
	}
	if _, err := slaveRequest(conn, 1); err == nil {
		t.Fatal("second request should trip the ban")
	}
	first := dialSlave(t, addr)
	if _, err := slaveRequest(first, 1); err == nil {
		t.Fatal("peer was served while banned")
	}
	time.Sleep(1200 * time.Millisecond)
	again := dialSlave(t, addr)
	if _, err := slaveRequest(again, 1); err != nil {
		t.Fatalf("ban never expired: %v", err)
	}
}
