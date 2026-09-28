package drivers

import (
	"encoding/binary"
	"net"
	"time"
)

// Helper: write bytes to TCP connection with timeout
func tcpWrite(conn net.Conn, buf []byte, timeout time.Duration) error {
	conn.SetWriteDeadline(time.Now().Add(timeout))
	_, err := conn.Write(buf)
	return err
}

// Helper: read uint16 from big-endian bytes
func readUint16BE(b []byte, offset int) uint16 {
	if offset+2 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint16(b[offset:])
}

// Helper: read uint32 from big-endian bytes
func readUint32BE(b []byte, offset int) uint32 {
	if offset+4 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint32(b[offset:])
}
