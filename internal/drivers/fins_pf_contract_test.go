package drivers

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"edgelite/internal/models"
)

// mockFinsServer speaks standard FINS/TCP (the "FINS" magic + length-prefixed
// body, node negotiation, and Command=0x00000002 frame-send) so the driver's
// corrected wire framing can be verified hermetically.
type mockFinsServer struct {
	ln   net.Listener
	word map[uint16]uint16 // word address -> value for DM area
}

func newMockFins(t *testing.T) (*mockFinsServer, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &mockFinsServer{ln: ln, word: map[uint16]uint16{}}
	s.word[100] = 0x1234
	go s.serve()
	return s, ln.Addr().String()
}

func (s *mockFinsServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func finsReadFull(conn net.Conn, n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(conn, b); err != nil {
		return nil, err
	}
	return b, nil
}

func finsWrap(body []byte) []byte {
	f := make([]byte, 8+len(body))
	copy(f[0:4], "FINS")
	binary.BigEndian.PutUint32(f[4:8], uint32(len(body)))
	copy(f[8:], body)
	return f
}

func (s *mockFinsServer) handle(conn net.Conn) {
	defer conn.Close()
	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		header, err := finsReadFull(conn, 8)
		if err != nil {
			return
		}
		if string(header[0:4]) != "FINS" {
			return
		}
		bodyLen := int(binary.BigEndian.Uint32(header[4:8]))
		body, err := finsReadFull(conn, bodyLen)
		if err != nil {
			return
		}
		command := binary.BigEndian.Uint32(body[0:4])
		var respBody []byte
		switch command {
		case 0x00000000: // node address negotiation
			respBody = make([]byte, 16)
			binary.BigEndian.PutUint32(respBody[0:4], 0x00000001)
			binary.BigEndian.PutUint32(respBody[8:12], 1) // source node
			binary.BigEndian.PutUint32(respBody[12:16], 0)
		case 0x00000002: // FINS frame send (memory area read or write)
			fins := body[8:]
			if len(fins) < 18 {
				return
			}
			wordAddr := binary.BigEndian.Uint16(fins[13:15])
			count := int(binary.BigEndian.Uint16(fins[16:18]))
			if fins[10] == 0x01 && fins[11] == 0x02 {
				// Memory-area write: MRC 0x01 / SRC 0x02, data follows the count.
				data := fins[18:]
				for i := 0; i < count && len(data) >= (i+1)*2; i++ {
					s.word[wordAddr+uint16(i)] = binary.BigEndian.Uint16(data[i*2:])
				}
				fresp := make([]byte, 0, 14)
				fresp = append(fresp, fins[0:10]...)
				fresp = append(fresp, fins[10], fins[11])
				fresp = append(fresp, 0x00, 0x00)
				respBody = make([]byte, 8+len(fresp))
				binary.BigEndian.PutUint32(respBody[0:4], 0x00000002)
				copy(respBody[8:], fresp)
				if _, err := conn.Write(finsWrap(respBody)); err != nil {
					return
				}
				continue
			}
			data := make([]byte, count*2)
			for i := 0; i < count; i++ {
				binary.BigEndian.PutUint16(data[i*2:], s.word[wordAddr+uint16(i)])
			}
			fresp := make([]byte, 0, 14+len(data))
			fresp = append(fresp, fins[0:10]...)      // header (echo)
			fresp = append(fresp, fins[10], fins[11]) // MRC/SRC
			fresp = append(fresp, 0x00, 0x00)         // end code
			fresp = append(fresp, data...)
			respBody = make([]byte, 8+len(fresp))
			binary.BigEndian.PutUint32(respBody[0:4], 0x00000002)
			copy(respBody[8:], fresp)
		default:
			return
		}
		if _, err := conn.Write(finsWrap(respBody)); err != nil {
			return
		}
	}
}

// TestFINSWriteCarriesTheDeclaredDataType pins the address-aware write path:
// it used to drop the point's data type and guess from the JSON number, so a
// uint16 point holding 43690 was encoded as int16, refused by the range guard,
// and the operator's perfectly valid write never reached the PLC.
func TestFINSWriteCarriesTheDeclaredDataType(t *testing.T) {
	_, addr := newMockFins(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)

	created, err := NewFINSDriver("fins-write", map[string]interface{}{
		"host": host, "port": port, "node": 1, "unit": 0, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	d := created.(*FINSDriver)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	if err := d.WritePointAtAddress(ctx, "D100", float64(43690), "uint16"); err != nil {
		t.Fatalf("uint16 write of 43690 returned %v", err)
	}
	got, err := d.ReadPoints(ctx, []models.PointDef{{Name: "v", Address: "D100", DataType: "uint16"}})
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadPoints: %v (%d rows)", err, len(got))
	}
	if v, ok := got[0].Value.(uint16); !ok || v != 43690 {
		t.Fatalf("read back %v (%T), want uint16 43690", got[0].Value, got[0].Value)
	}

	// The same number through a point that declares int16 must be refused, not
	// written as some other value.
	err = d.WritePointAtAddress(ctx, "D100", float64(43690), "int16")
	if err == nil || !strings.Contains(err.Error(), "out of range for int16") {
		t.Fatalf("int16 write of 43690 returned %v, want a range refusal", err)
	}
	if got, err := d.ReadPoints(ctx, []models.PointDef{{Name: "v", Address: "D100", DataType: "uint16"}}); err != nil || got[0].Value.(uint16) != 43690 {
		t.Fatalf("refused write changed the word to %v (%v), want it unchanged at 43690", got[0].Value, err)
	}

	// A float32 point spans two words; the address alone does not say that.
	if err := d.WritePointAtAddress(ctx, "D200", 21.5, "float32"); err != nil {
		t.Fatalf("float32 write returned %v", err)
	}
	rows, err := d.ReadPoints(ctx, []models.PointDef{{Name: "f", Address: "D200", DataType: "float32"}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("ReadPoints after the float write: %v (%d rows)", err, len(rows))
	}
	if v, ok := rows[0].Value.(float32); !ok || v != 21.5 {
		t.Fatalf("float32 round trip gave %v (%T), want 21.5", rows[0].Value, rows[0].Value)
	}
}

func TestFINSReadAgainstMock(t *testing.T) {
	_, addr := newMockFins(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)

	d, err := NewFINSDriver("fins-mock", map[string]interface{}{
		"host": host, "port": port, "node": 1, "unit": 0, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{{Name: "D100", Address: "D100", DataType: "uint16"}}
	got, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "good" {
		t.Fatalf("bad read: %+v", got)
	}
	if v, ok := got[0].Value.(uint16); !ok || v != 0x1234 {
		t.Fatalf("FINS word decode mismatch at D100: %v (%T), want 0x1234", got[0].Value, got[0].Value)
	}
}

// TestFINS_TCPAgainstProtoForge is a live joint-debugging smoke test against a
// running ProtoForge Omron FINS simulator (127.0.0.1:9600). It verifies the
// corrected standard FINS/TCP transport (node negotiation + Command=2 memory
// area read) works end-to-end. Skips when unreachable; asserts no specific
// value since the simulator's memory contents are volatile.
func TestFINS_TCPAgainstProtoForge(t *testing.T) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:9600", 500*time.Millisecond)
	if err != nil {
		t.Skipf("ProtoForge Omron FINS simulator not reachable: %v", err)
	}
	conn.Close()

	d, err := NewFINSDriver("pf-fins", map[string]interface{}{
		"host": "127.0.0.1", "port": 9600, "node": 1, "unit": 0, "timeout": 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Connect(ctx); err != nil {
		t.Fatalf("connect to ProtoForge FINS: %v", err)
	}
	defer d.Disconnect()

	pts := []models.PointDef{{Name: "D100", Address: "D100", DataType: "int16"}}
	got, err := d.ReadPoints(ctx, pts)
	if err != nil {
		t.Fatalf("ReadPoints: %v", err)
	}
	if len(got) != 1 || got[0].Quality != "good" {
		t.Fatalf("bad read: %+v", got)
	}
}
