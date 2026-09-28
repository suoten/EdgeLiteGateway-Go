package engine

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"edgelite/internal/storage"
)

// StreamComputeEngine provides real-time stream processing of device data.
// This is a Go port of the Python edgelite/engine/stream_compute.py.
//
// Supported operations:
//   - Sliding window aggregation (mean, max, min, sum, count, stddev)
//   - Tumbling window aggregation
//   - Watermark-based event time processing
//   - Late data handling

// StreamWindow represents a window aggregation type.
type StreamWindow string

const (
	WindowSliding  StreamWindow = "sliding"
	WindowTumbling StreamWindow = "tumbling"
)

// StreamAggFunc represents the aggregation function.
type StreamAggFunc string

const (
	AggMean   StreamAggFunc = "mean"
	AggMax    StreamAggFunc = "max"
	AggMin    StreamAggFunc = "min"
	AggSum    StreamAggFunc = "sum"
	AggCount  StreamAggFunc = "count"
	AggStdDev StreamAggFunc = "stddev"
	AggMedian StreamAggFunc = "median"
)

// StreamConfig holds configuration for a stream computation.
type StreamConfig struct {
	DeviceID   string        `json:"device_id"`
	PointName  string        `json:"point_name"`
	WindowType StreamWindow  `json:"window_type"`
	WindowSize time.Duration `json:"window_size"`
	SlideStep  time.Duration `json:"slide_step,omitempty"`
	AggFunc    StreamAggFunc `json:"agg_func"`
}

// StreamResult is the output of a stream computation.
type StreamResult struct {
	DeviceID  string    `json:"device_id"`
	PointName string    `json:"point_name"`
	Value     float64   `json:"value"`
	Timestamp time.Time `json:"timestamp"`
	Count     int       `json:"count"`
	WindowEnd time.Time `json:"window_end"`
}

// StreamComputeEngine processes real-time data streams.
type StreamComputeEngine struct {
	mu         sync.RWMutex
	configs    map[string]*StreamConfig
	buffers    map[string][]streamPoint
	eventBus   *EventBus
	tsStorage  *storage.TimeSeriesStorage
	started    bool
	cancelFunc context.CancelFunc
}

type streamPoint struct {
	value float64
	ts    time.Time
}

// NewStreamComputeEngine creates a new StreamComputeEngine.
func NewStreamComputeEngine(eventBus *EventBus, tsStorage *storage.TimeSeriesStorage) *StreamComputeEngine {
	return &StreamComputeEngine{
		configs:   make(map[string]*StreamConfig),
		buffers:   make(map[string][]streamPoint),
		eventBus:  eventBus,
		tsStorage: tsStorage,
	}
}

// RegisterStream registers a new stream computation.
func (s *StreamComputeEngine) RegisterStream(config *StreamConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := config.DeviceID + "." + config.PointName
	s.configs[key] = config
	logrus.WithFields(logrus.Fields{
		"device_id":   config.DeviceID,
		"point_name":  config.PointName,
		"window_type": config.WindowType,
		"agg_func":    config.AggFunc,
	}).Info("Stream computation registered")
}

// UnregisterStream removes a stream computation.
func (s *StreamComputeEngine) UnregisterStream(deviceID, pointName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := deviceID + "." + pointName
	delete(s.configs, key)
	delete(s.buffers, key)
}

// Start begins the stream computation engine.
func (s *StreamComputeEngine) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	childCtx, cancel := context.WithCancel(ctx)
	s.cancelFunc = cancel

	// Start periodic window evaluation
	go s.evaluateLoop(childCtx)

	logrus.Info("StreamComputeEngine started")
}

// Stop stops the stream computation engine.
func (s *StreamComputeEngine) Stop() {
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()

	if s.cancelFunc != nil {
		s.cancelFunc()
		s.cancelFunc = nil
	}
	logrus.Info("StreamComputeEngine stopped")
}

// IngestData feeds a new data point into the stream engine.
func (s *StreamComputeEngine) IngestData(deviceID, pointName string, value float64, ts time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := deviceID + "." + pointName
	config, ok := s.configs[key]
	if !ok {
		return
	}

	// Add to buffer
	s.buffers[key] = append(s.buffers[key], streamPoint{value: value, ts: ts})

	// Trim buffer to window size
	cutoff := ts.Add(-config.WindowSize)
	buffer := s.buffers[key]
	idx := 0
	for idx < len(buffer) && buffer[idx].ts.Before(cutoff) {
		idx++
	}
	if idx > 0 {
		s.buffers[key] = buffer[idx:]
	}
}

// evaluateLoop periodically evaluates all stream windows.
func (s *StreamComputeEngine) evaluateLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.evaluateAll()
		}
	}
}

func (s *StreamComputeEngine) evaluateAll() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	for key, config := range s.configs {
		buffer := s.buffers[key]
		if len(buffer) == 0 {
			continue
		}

		// Get points within window
		cutoff := now.Add(-config.WindowSize)
		var points []streamPoint
		for _, p := range buffer {
			if p.ts.After(cutoff) {
				points = append(points, p)
			}
		}

		if len(points) == 0 {
			continue
		}

		result := s.aggregate(config.AggFunc, points)
		streamResult := StreamResult{
			DeviceID:  config.DeviceID,
			PointName: config.PointName,
			Value:     result,
			Count:     len(points),
			Timestamp: now,
			WindowEnd: now,
		}

		// Publish result via event bus
		if s.eventBus != nil {
			s.eventBus.Publish(Event{
				Type:     EventTypeStreamResult,
				Source:   "stream_compute",
				DeviceID: config.DeviceID,
				Data:     streamResult,
			})
		}
	}
}

func (s *StreamComputeEngine) aggregate(fn StreamAggFunc, points []streamPoint) float64 {
	if len(points) == 0 {
		return 0
	}

	var values []float64
	for _, p := range points {
		values = append(values, p.value)
	}

	switch fn {
	case AggMean:
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		return sum / float64(len(values))
	case AggMax:
		max := values[0]
		for _, v := range values[1:] {
			if v > max {
				max = v
			}
		}
		return max
	case AggMin:
		min := values[0]
		for _, v := range values[1:] {
			if v < min {
				min = v
			}
		}
		return min
	case AggSum:
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		return sum
	case AggCount:
		return float64(len(values))
	case AggStdDev:
		mean := 0.0
		for _, v := range values {
			mean += v
		}
		mean /= float64(len(values))
		variance := 0.0
		for _, v := range values {
			diff := v - mean
			variance += diff * diff
		}
		variance /= float64(len(values))
		return sqrtFloat64(variance)
	case AggMedian:
		// Simple median (assumes sorted or sorts)
		sorted := make([]float64, len(values))
		copy(sorted, values)
		// Insertion sort for small arrays
		for i := 1; i < len(sorted); i++ {
			key := sorted[i]
			j := i - 1
			for j >= 0 && sorted[j] > key {
				sorted[j+1] = sorted[j]
				j--
			}
			sorted[j+1] = key
		}
		n := len(sorted)
		if n%2 == 0 {
			return (sorted[n/2-1] + sorted[n/2]) / 2
		}
		return sorted[n/2]
	default:
		return 0
	}
}

func sqrtFloat64(x float64) float64 {
	if x < 0 {
		return 0
	}
	// Newton's method
	if x == 0 {
		return 0
	}
	z := x
	for i := 0; i < 10; i++ {
		z = z - (z*z-x)/(2*z)
	}
	return z
}

// GetStreamConfigs returns all registered stream configs.
func (s *StreamComputeEngine) GetStreamConfigs() []StreamConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]StreamConfig, 0, len(s.configs))
	for _, c := range s.configs {
		result = append(result, *c)
	}
	return result
}

// GetBufferStats returns buffer statistics for a stream.
func (s *StreamComputeEngine) GetBufferStats(deviceID, pointName string) map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := deviceID + "." + pointName
	buffer := s.buffers[key]
	return map[string]interface{}{
		"device_id":   deviceID,
		"point_name":  pointName,
		"buffer_size": len(buffer),
	}
}
