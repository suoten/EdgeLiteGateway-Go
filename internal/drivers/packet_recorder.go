package drivers

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// MaxPacketBuffer is the maximum number of packets to buffer per protocol.
const MaxPacketBuffer = 1000

// PacketRecord represents a captured protocol packet.
type PacketRecord struct {
	Seq       int64     `json:"seq"`
	Direction string    `json:"direction"` // "tx" or "rx"
	Protocol  string    `json:"protocol"`
	DeviceID  string    `json:"device_id"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// PacketRecorder is a neutral module for recording protocol packets.
// It maintains per-protocol ring buffers for debugging and observability.
type PacketRecorder struct {
	mu      sync.RWMutex
	buffers map[string][]*PacketRecord // protocol -> packets
	allBuf  []*PacketRecord            // combined buffer for all protocols
	// 使用 atomic.Int64 自带 8 字节对齐，避免 linux/arm 32 位下原子操作 panic
	seq     atomic.Int64
	maxSize int
}

var globalPacketRecorder = NewPacketRecorder()

// GetPacketRecorder returns the global packet recorder instance.
func GetPacketRecorder() *PacketRecorder {
	return globalPacketRecorder
}

// NewPacketRecorder creates a new PacketRecorder.
func NewPacketRecorder() *PacketRecorder {
	return &PacketRecorder{
		buffers: make(map[string][]*PacketRecord),
		maxSize: MaxPacketBuffer,
	}
}

// RecordPacket records a protocol packet.
// direction: "tx" (sent) or "rx" (received)
// protocol: protocol name (e.g., "modbus_tcp", "s7", "ab")
// deviceID: device identifier
// content: packet content as string or hex
func RecordPacket(direction, protocol, deviceID, content string) {
	globalPacketRecorder.Record(direction, protocol, deviceID, content)
}

// Record records a protocol packet.
func (pr *PacketRecorder) Record(direction, protocol, deviceID, content string) {
	seq := pr.seq.Add(1)
	record := &PacketRecord{
		Seq:       seq,
		Direction: direction,
		Protocol:  protocol,
		DeviceID:  deviceID,
		Content:   content,
		Timestamp: time.Now(),
	}

	pr.mu.Lock()
	defer pr.mu.Unlock()

	// Add to protocol-specific buffer
	buf, exists := pr.buffers[protocol]
	if !exists {
		buf = make([]*PacketRecord, 0, pr.maxSize)
	}
	buf = append(buf, record)
	if len(buf) > pr.maxSize {
		buf = buf[len(buf)-pr.maxSize:]
	}
	pr.buffers[protocol] = buf

	// Add to combined buffer
	pr.allBuf = append(pr.allBuf, record)
	if len(pr.allBuf) > pr.maxSize {
		pr.allBuf = pr.allBuf[len(pr.allBuf)-pr.maxSize:]
	}

	logrus.WithFields(logrus.Fields{
		"seq":       seq,
		"direction": direction,
		"protocol":  protocol,
		"device_id": deviceID,
	}).Trace("Packet recorded")
}

// GetBuffer returns the packet buffer for a protocol.
// If protocol is empty or "__all__", returns the combined buffer.
func (pr *PacketRecorder) GetBuffer(protocol string) []*PacketRecord {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	if protocol == "" || protocol == "__all__" {
		result := make([]*PacketRecord, len(pr.allBuf))
		copy(result, pr.allBuf)
		return result
	}

	buf := pr.buffers[protocol]
	result := make([]*PacketRecord, len(buf))
	copy(result, buf)
	return result
}

// GetBufferSince returns packets since a given sequence number.
func (pr *PacketRecorder) GetBufferSince(protocol string, sinceSeq int64) []*PacketRecord {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	var source []*PacketRecord
	if protocol == "" || protocol == "__all__" {
		source = pr.allBuf
	} else {
		source = pr.buffers[protocol]
	}

	var result []*PacketRecord
	for _, p := range source {
		if p.Seq > sinceSeq {
			result = append(result, p)
		}
	}
	return result
}

// Clear clears all packet buffers.
func (pr *PacketRecorder) Clear() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.buffers = make(map[string][]*PacketRecord)
	pr.allBuf = nil
}

// ClearProtocol clears the packet buffer for a specific protocol.
func (pr *PacketRecorder) ClearProtocol(protocol string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	delete(pr.buffers, protocol)
}

// GetStats returns statistics about recorded packets.
func (pr *PacketRecorder) GetStats() map[string]interface{} {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	byProtocol := make(map[string]int)
	for proto, buf := range pr.buffers {
		byProtocol[proto] = len(buf)
	}

	return map[string]interface{}{
		"total_packets": len(pr.allBuf),
		"by_protocol":   byProtocol,
		"max_buffer":    pr.maxSize,
	}
}
