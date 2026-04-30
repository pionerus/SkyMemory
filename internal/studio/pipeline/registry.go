package pipeline

import (
	"context"
	"errors"
	"sync"
)

// RunRegistry serialises pipeline runs on a single iGPU. Intel UHD-class
// QuickSync ships with one encoder engine, so running two h264_qsv encodes
// simultaneously dead-locks at the driver level. The HTTP handlers consult
// this registry to reject overlapping Generate requests with a clean 409,
// and the Stop button calls Cancel on the in-flight generation.
type RunRegistry struct {
	mu      sync.Mutex
	current *runEntry
}

type runEntry struct {
	generationID int64
	cancel       context.CancelFunc
}

// ErrAnotherRunning is returned by Begin when another generation already owns
// the slot.
var ErrAnotherRunning = errors.New("another generation is already running")

// Begin claims the slot for genID. Returns a derived context the caller
// should pass to Runner.Run — calling Cancel(genID) on this registry
// cancels that context, which propagates to the running ffmpeg process.
func (r *RunRegistry) Begin(parent context.Context, genID int64) (context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil {
		return nil, ErrAnotherRunning
	}
	ctx, cancel := context.WithCancel(parent)
	r.current = &runEntry{generationID: genID, cancel: cancel}
	return ctx, nil
}

// End releases the slot. Safe to call even if Cancel was already invoked.
func (r *RunRegistry) End(genID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil && r.current.generationID == genID {
		r.current = nil
	}
}

// Cancel signals the running generation to stop, if genID matches. Returns
// true when cancellation was issued. The runner goroutine still calls End()
// to release the slot once ffmpeg has exited.
func (r *RunRegistry) Cancel(genID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.current.generationID != genID {
		return false
	}
	r.current.cancel()
	return true
}

// CurrentID returns the generation ID currently running, or 0 if idle.
func (r *RunRegistry) CurrentID() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return 0
	}
	return r.current.generationID
}

// IsBusy is a non-blocking probe used by HTTP handlers to fast-path a 409
// before bothering the DB with a generation row.
func (r *RunRegistry) IsBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current != nil
}
