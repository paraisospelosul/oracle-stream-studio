package main

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// PipelineStats holds runtime info for a pipeline
type PipelineStats struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Active   bool   `json:"active"`
	Ready    bool   `json:"ready"`
	LastData string `json:"last_data,omitempty"`
}

// Pipeline wraps a data source (SRT or fallback scene) with its own data channel
type Pipeline struct {
	ID     string
	Source string // "srt" or scene ID
	DataCh chan []byte
	Active bool
	Ready  bool // true once first data received

	// Cached MPEG-TS stream info for instant switching
	CachedPAT []byte
	CachedPMT []byte
	pmtPID    uint16

	LastData time.Time
	mu       sync.Mutex
}

// UpdateCaches parses TS data and caches PAT/PMT packets for later injection
func (p *Pipeline) UpdateCaches(data []byte) {
	for offset := 0; offset+tsPacketSize <= len(data); offset += tsPacketSize {
		pkt := data[offset : offset+tsPacketSize]
		if pkt[0] != tsSyncByte {
			continue
		}
		info, ok := ParseTSPacket(pkt)
		if !ok {
			continue
		}
		// Cache PAT
		if info.PID == patPID && info.PUSI && info.HasPayload {
			p.mu.Lock()
			p.CachedPAT = make([]byte, tsPacketSize)
			copy(p.CachedPAT, pkt)
			if pmtPID := ParsePAT(pkt[info.PayloadOffset:]); pmtPID != 0 {
				p.pmtPID = pmtPID
			}
			p.mu.Unlock()
		}
		// Cache PMT
		if p.pmtPID != 0 && info.PID == p.pmtPID && info.PUSI && info.HasPayload {
			p.mu.Lock()
			p.CachedPMT = make([]byte, tsPacketSize)
			copy(p.CachedPMT, pkt)
			p.mu.Unlock()
		}
	}
}

// PipelineRouter manages multiple data pipelines and switches between them instantly
type PipelineRouter struct {
	pipelines  map[string]*Pipeline
	activePipe atomic.Value // stores *Pipeline
	mu         sync.RWMutex
	maxPreWarm int
}

// NewPipelineRouter creates a router with a limit on concurrent pre-warmed pipelines
func NewPipelineRouter(maxPreWarm int) *PipelineRouter {
	if maxPreWarm <= 0 {
		maxPreWarm = 3
	}
	return &PipelineRouter{
		pipelines:  make(map[string]*Pipeline),
		maxPreWarm: maxPreWarm,
	}
}

// AddPipeline registers a new pipeline with the router
func (pr *PipelineRouter) AddPipeline(id, source string, dataCh chan []byte) *Pipeline {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	p := &Pipeline{
		ID:     id,
		Source: source,
		DataCh: dataCh,
		Active: false,
		Ready:  false,
	}
	pr.pipelines[id] = p
	log.Printf("[router] Pipeline added: %s (source=%s)", id, source)
	return p
}

// RemovePipeline unregisters a pipeline
func (pr *PipelineRouter) RemovePipeline(id string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	if _, ok := pr.pipelines[id]; ok {
		delete(pr.pipelines, id)
		log.Printf("[router] Pipeline removed: %s", id)
	}
}

// SwitchTo atomically switches the active pipeline. This is the hot path —
// designed to be as fast as possible with zero allocation.
func (pr *PipelineRouter) SwitchTo(id string) error {
	pr.mu.RLock()
	newPipe, ok := pr.pipelines[id]
	pr.mu.RUnlock()
	if !ok {
		return &pipelineNotFoundError{id: id}
	}

	// Deactivate old pipeline
	if old := pr.GetActive(); old != nil {
		old.mu.Lock()
		old.Active = false
		old.mu.Unlock()
	}

	// Activate new pipeline
	newPipe.mu.Lock()
	newPipe.Active = true
	newPipe.mu.Unlock()

	// Atomic swap — this is the instant switch point
	pr.activePipe.Store(newPipe)

	log.Printf("[router] Switched to pipeline: %s (source=%s)", id, newPipe.Source)
	return nil
}

type pipelineNotFoundError struct {
	id string
}

func (e *pipelineNotFoundError) Error() string {
	return "pipeline not found: " + e.id
}

// GetActive returns the currently active pipeline via lock-free atomic load
func (pr *PipelineRouter) GetActive() *Pipeline {
	v := pr.activePipe.Load()
	if v == nil {
		return nil
	}
	return v.(*Pipeline)
}

// GetPipeline returns a pipeline by ID
func (pr *PipelineRouter) GetPipeline(id string) *Pipeline {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	return pr.pipelines[id]
}

// ListPipelines returns all registered pipelines
func (pr *PipelineRouter) ListPipelines() []*Pipeline {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	result := make([]*Pipeline, 0, len(pr.pipelines))
	for _, p := range pr.pipelines {
		result = append(result, p)
	}
	return result
}

// DrainInactive runs continuously, reading and discarding data from inactive
// pipeline channels to prevent backpressure. Also caches PAT/PMT from each
// stream and tracks readiness.
func (pr *PipelineRouter) DrainInactive(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pr.mu.RLock()
			for _, p := range pr.pipelines {
				if p.Active || p.ID == "srt" || p.ID == "fallback" {
					continue
				}
				// Non-blocking drain of inactive pipeline data
				for {
					select {
					case data := <-p.DataCh:
						p.mu.Lock()
						p.Ready = true
						p.LastData = time.Now()
						p.mu.Unlock()
						// Cache PAT/PMT from the stream while draining
						p.UpdateCaches(data)
					default:
						goto nextPipeline
					}
				}
			nextPipeline:
			}
			pr.mu.RUnlock()
		}
	}
}

// GetPipelineStats returns stats for all pipelines
func (pr *PipelineRouter) GetPipelineStats() []PipelineStats {
	pr.mu.RLock()
	defer pr.mu.RUnlock()

	stats := make([]PipelineStats, 0, len(pr.pipelines))
	for _, p := range pr.pipelines {
		p.mu.Lock()
		lastData := ""
		if !p.LastData.IsZero() {
			lastData = p.LastData.Format(time.RFC3339)
		}
		stats = append(stats, PipelineStats{
			ID:       p.ID,
			Source:   p.Source,
			Active:   p.Active,
			Ready:    p.Ready,
			LastData: lastData,
		})
		p.mu.Unlock()
	}
	return stats
}

// PreWarmCount returns the number of currently pre-warmed (inactive but registered) pipelines
func (pr *PipelineRouter) PreWarmCount() int {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	count := 0
	for _, p := range pr.pipelines {
		if !p.Active {
			count++
		}
	}
	return count
}

// CanPreWarm returns true if we haven't hit the max pre-warm limit
func (pr *PipelineRouter) CanPreWarm() bool {
	return pr.PreWarmCount() < pr.maxPreWarm
}
