package smartimport

import (
	"sync"
	"time"
)

// Phase is the human-readable stage label shown to the operator while the
// background job runs. Order: saving → analyzing_audio → classifying →
// creating_clips → done (or failed at any point).
type Phase string

const (
	PhaseSaving         Phase = "saving"
	PhaseAnalyzingAudio Phase = "analyzing_audio"
	PhaseClassifying    Phase = "classifying"
	PhaseCreatingClips  Phase = "creating_clips"
	PhaseDone           Phase = "done"
	PhaseFailed         Phase = "failed"
)

// Job is the in-memory state of one smart-import run. Read by the polling
// HTTP endpoint, written by the background goroutine. Mutex protects the
// whole struct — every field changes together.
type Job struct {
	mu sync.Mutex

	ID         string
	ProjectID  int64
	Phase      Phase
	Current    int    // files processed so far in the current phase
	Total      int    // total files in the import
	CurrentFile string // filename being analyzed RIGHT NOW
	StartedAt  time.Time
	UpdatedAt  time.Time

	// Populated once Phase == done.
	Assignments       []Assignment
	FreefallConfidence string

	// Populated once Phase == failed.
	Error string
}

// Snapshot is a lock-free copy returned to API callers. Don't share the
// internal Job pointer across goroutines.
type Snapshot struct {
	ID          string       `json:"id"`
	Phase       Phase        `json:"phase"`
	Current     int          `json:"current"`
	Total       int          `json:"total"`
	CurrentFile string       `json:"current_file,omitempty"`
	Done        bool         `json:"done"`
	Failed      bool         `json:"failed"`
	Error       string       `json:"error,omitempty"`
	Assignments []Assignment `json:"assignments,omitempty"`
	FreefallConfidence string `json:"freefall_confidence,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// JobRegistry tracks active and recently-completed smart-import jobs. Map
// keys are job IDs (random tokens issued at Start). Completed jobs linger
// for 30 minutes so the UI can poll the final state after a slow page
// reload — then a background sweep evicts them.
type JobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// NewRegistry returns an empty registry and kicks off the periodic sweep.
func NewRegistry() *JobRegistry {
	r := &JobRegistry{jobs: map[string]*Job{}}
	go r.sweepLoop()
	return r
}

// Start registers a fresh job in the saving phase with the supplied total.
// Caller is responsible for issuing a unique ID — see NewID below.
func (r *JobRegistry) Start(jobID string, projectID int64, totalFiles int) *Job {
	now := time.Now()
	j := &Job{
		ID:        jobID,
		ProjectID: projectID,
		Phase:     PhaseSaving,
		Total:     totalFiles,
		StartedAt: now,
		UpdatedAt: now,
	}
	r.mu.Lock()
	r.jobs[jobID] = j
	r.mu.Unlock()
	return j
}

// Get returns a snapshot of the named job, or false when the ID is unknown
// or has expired.
func (r *JobRegistry) Get(jobID string) (Snapshot, bool) {
	r.mu.Lock()
	j, ok := r.jobs[jobID]
	r.mu.Unlock()
	if !ok {
		return Snapshot{}, false
	}
	return j.snapshot(), true
}

// SetPhase atomically updates the phase + per-file progress.
func (j *Job) SetPhase(phase Phase, current int, currentFile string) {
	j.mu.Lock()
	j.Phase = phase
	j.Current = current
	j.CurrentFile = currentFile
	j.UpdatedAt = time.Now()
	j.mu.Unlock()
}

// Finish records a successful run; transitions phase → done.
func (j *Job) Finish(assignments []Assignment, confidence string) {
	j.mu.Lock()
	j.Phase = PhaseDone
	j.Current = j.Total
	j.CurrentFile = ""
	j.Assignments = assignments
	j.FreefallConfidence = confidence
	j.UpdatedAt = time.Now()
	j.mu.Unlock()
}

// Fail records a terminal failure with the supplied human message.
func (j *Job) Fail(msg string) {
	j.mu.Lock()
	j.Phase = PhaseFailed
	j.Error = msg
	j.UpdatedAt = time.Now()
	j.mu.Unlock()
}

func (j *Job) snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Snapshot{
		ID:                 j.ID,
		Phase:              j.Phase,
		Current:            j.Current,
		Total:              j.Total,
		CurrentFile:        j.CurrentFile,
		Done:               j.Phase == PhaseDone,
		Failed:             j.Phase == PhaseFailed,
		Error:              j.Error,
		Assignments:        j.Assignments,
		FreefallConfidence: j.FreefallConfidence,
		StartedAt:          j.StartedAt,
		UpdatedAt:          j.UpdatedAt,
	}
}

// sweepLoop evicts terminal jobs older than 30 minutes. Runs every 5
// minutes — cheap.
func (r *JobRegistry) sweepLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-30 * time.Minute)
		r.mu.Lock()
		for id, j := range r.jobs {
			j.mu.Lock()
			terminal := j.Phase == PhaseDone || j.Phase == PhaseFailed
			old := j.UpdatedAt.Before(cutoff)
			j.mu.Unlock()
			if terminal && old {
				delete(r.jobs, id)
			}
		}
		r.mu.Unlock()
	}
}
