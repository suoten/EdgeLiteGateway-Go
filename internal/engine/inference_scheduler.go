package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// InferencePriority represents the priority of an inference request.
type InferencePriority int

const (
	InferencePriorityLow    InferencePriority = 1
	InferencePriorityNormal InferencePriority = 5
	InferencePriorityHigh   InferencePriority = 10
)

// inferenceRequest represents a queued inference request.
type inferenceRequest struct {
	modelID     string
	inputData   []float64
	priority    InferencePriority
	result      chan inferenceResult
	submittedAt time.Time
}

type inferenceResult struct {
	output interface{}
	err    error
}

// modelEntry tracks a registered model in the scheduler.
// 计数器统一使用 atomic.Int64 类型：该类型自带 8 字节对齐保证（align64），
// 避免在 32 位平台（linux/arm）上 atomic.AddInt64 对未对齐字段操作直接 panic。
type modelEntry struct {
	inferFn        func(ctx context.Context, input []float64) (interface{}, error)
	mu             sync.Mutex
	requests       atomic.Int64
	successes      atomic.Int64
	failures       atomic.Int64
	totalLatencyMs atomic.Int64
}

// InferenceScheduler manages AI inference requests with priority queuing.
type InferenceScheduler struct {
	mu            sync.Mutex
	models        map[string]*modelEntry
	queue         chan *inferenceRequest
	workers       int
	wg            sync.WaitGroup
	cancelFn      context.CancelFunc
	started       bool
	totalRequests atomic.Int64
}

// NewInferenceScheduler creates a new InferenceScheduler.
func NewInferenceScheduler(workers int) *InferenceScheduler {
	if workers <= 0 {
		workers = 2
	}
	return &InferenceScheduler{
		models:  make(map[string]*modelEntry),
		queue:   make(chan *inferenceRequest, 1024),
		workers: workers,
	}
}

// Start starts the scheduler workers.
func (s *InferenceScheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("scheduler already started")
	}
	s.started = true
	s.mu.Unlock()

	childCtx, cancel := context.WithCancel(ctx)
	s.cancelFn = cancel

	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.dispatchLoop(childCtx, i)
	}
	return nil
}

// Stop stops the scheduler.
func (s *InferenceScheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	s.mu.Unlock()

	if s.cancelFn != nil {
		s.cancelFn()
	}
	s.wg.Wait()
}

// RegisterModel registers a model with its inference function.
func (s *InferenceScheduler) RegisterModel(modelID string, inferFn func(ctx context.Context, input []float64) (interface{}, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models[modelID] = &modelEntry{
		inferFn: inferFn,
	}
}

// UnregisterModel removes a model from the scheduler.
func (s *InferenceScheduler) UnregisterModel(modelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.models, modelID)
}

// SubmitAndWait submits an inference request and waits for the result.
func (s *InferenceScheduler) SubmitAndWait(ctx context.Context, modelID string, input []float64, priority InferencePriority, timeout time.Duration) (interface{}, error) {
	s.mu.Lock()
	model, ok := s.models[modelID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("model %s not registered", modelID)
	}

	s.totalRequests.Add(1)
	model.requests.Add(1)

	req := &inferenceRequest{
		modelID:     modelID,
		inputData:   input,
		priority:    priority,
		result:      make(chan inferenceResult, 1),
		submittedAt: time.Now(),
	}

	select {
	case s.queue <- req:
	default:
		model.failures.Add(1)
		return nil, fmt.Errorf("inference queue is full")
	}

	select {
	case res := <-req.result:
		if res.err != nil {
			model.failures.Add(1)
			return nil, res.err
		}
		model.successes.Add(1)
		return res.output, nil
	case <-time.After(timeout):
		model.failures.Add(1)
		return nil, fmt.Errorf("inference timed out after %v", timeout)
	case <-ctx.Done():
		model.failures.Add(1)
		return nil, ctx.Err()
	}
}

func (s *InferenceScheduler) dispatchLoop(ctx context.Context, workerID int) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-s.queue:
			s.executeInference(ctx, req)
		}
	}
}

func (s *InferenceScheduler) executeInference(ctx context.Context, req *inferenceRequest) {
	s.mu.Lock()
	model, ok := s.models[req.modelID]
	s.mu.Unlock()
	if !ok {
		req.result <- inferenceResult{err: fmt.Errorf("model %s not found", req.modelID)}
		return
	}

	start := time.Now()
	output, err := model.inferFn(ctx, req.inputData)
	latencyMs := time.Since(start).Milliseconds()

	model.totalLatencyMs.Add(latencyMs)

	req.result <- inferenceResult{output: output, err: err}

	if err != nil {
		logrus.WithFields(logrus.Fields{
			"model_id":   req.modelID,
			"latency_ms": latencyMs,
			"error":      err,
		}).Error("Inference execution failed")
	}
}

// GetModelMetrics returns metrics for a specific model.
func (s *InferenceScheduler) GetModelMetrics(modelID string) map[string]interface{} {
	s.mu.Lock()
	model, ok := s.models[modelID]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	requests := model.requests.Load()
	successes := model.successes.Load()
	failures := model.failures.Load()
	totalLatency := model.totalLatencyMs.Load()
	var avgLatency float64
	if requests > 0 {
		avgLatency = float64(totalLatency) / float64(requests)
	}
	var successRate float64
	if requests > 0 {
		successRate = float64(successes) / float64(requests)
	}
	return map[string]interface{}{
		"model_id":       modelID,
		"requests":       requests,
		"successes":      successes,
		"failures":       failures,
		"avg_latency_ms": avgLatency,
		"success_rate":   successRate,
	}
}

// GetStats returns overall scheduler statistics.
func (s *InferenceScheduler) GetStats() map[string]interface{} {
	s.mu.Lock()
	modelCount := len(s.models)
	s.mu.Unlock()
	return map[string]interface{}{
		"models":         modelCount,
		"queue_size":     len(s.queue),
		"workers":        s.workers,
		"total_requests": s.totalRequests.Load(),
	}
}

// QueueSize returns the current queue size.
func (s *InferenceScheduler) QueueSize() int {
	return len(s.queue)
}
