package smartimport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pionerus/freefall/internal/studio/ffmpeg"
	"github.com/pionerus/freefall/internal/studio/ffprobe"
	"github.com/pionerus/freefall/internal/studio/state"
)

// Handlers wires the smart-import HTTP endpoints. Constructed once at
// boot in cmd/studio/main.go and shared.
type Handlers struct {
	StateDB  *state.DB
	JobsDir  string // base directory for project uploads (~/.freefall-studio/jobs)
	Registry *JobRegistry
}

// Start handles POST /projects/{id}/clips/smart-import.
//
// 1. Parses the multipart form.
// 2. Refuses if the project already has clips (V1 limitation; V2 = merge).
// 3. Saves every uploaded file to <JobsDir>/<projectID>/uploads/<safeName>.
// 4. Starts a background goroutine running the analyze + classify + insert
//    pipeline, returns 202 with the job ID immediately.
//
// Caller polls /smart-import/{job_id} for progress.
func (h *Handlers) Start(w http.ResponseWriter, r *http.Request) {
	projectID, err := parseInt64Param(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_ID", "message": err.Error()})
		return
	}

	// Verify project exists locally.
	if _, err := h.StateDB.GetProject(r.Context(), projectID); err != nil {
		if errors.Is(err, state.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"code": "PROJECT_NOT_FOUND", "message": "Project not found in local state.db.",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "DB_ERROR", "message": err.Error()})
		return
	}

	// V1: refuse if project already has clips. V2 will merge.
	if existing, err := h.StateDB.ListClips(r.Context(), projectID); err == nil && len(existing) > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"code":    "PROJECT_NOT_EMPTY",
			"message": "Smart import only works on empty projects. Remove existing clips first or create a new project.",
		})
		return
	}

	// Parse multipart with a generous in-memory threshold. Files past 64MB
	// stream to a temp file via the multipart reader implementation.
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "INVALID_FORM", "message": err.Error()})
		return
	}

	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"code":    "NO_FILES",
			"message": "No files in the upload — drop a folder containing video files.",
		})
		return
	}
	if len(files) > 50 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"code":    "TOO_MANY_FILES",
			"message": "Smart import handles up to 50 files per folder. Split into smaller batches.",
		})
		return
	}

	// Stage 1: save every file to disk synchronously. We do this BEFORE
	// returning 202 so the operator's browser keeps the upload connection
	// open until bytes are persisted; otherwise an early disconnect would
	// truncate uploads. Audio-analysis + classify + insert run in the
	// background goroutine.
	uploadDir := filepath.Join(h.JobsDir, strconv.FormatInt(projectID, 10), "uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "FS_ERROR", "message": err.Error()})
		return
	}

	saved := make([]savedFile, 0, len(files))
	for _, fh := range files {
		sf, err := saveUpload(fh, uploadDir)
		if err != nil {
			// Cleanup any successful saves so a partial run doesn't leak.
			for _, s := range saved {
				_ = os.Remove(s.Path)
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"code":    "WRITE_ERROR",
				"message": fmt.Sprintf("failed to save %s: %v", fh.Filename, err),
			})
			return
		}
		saved = append(saved, sf)
	}

	jobID := newJobID()
	job := h.Registry.Start(jobID, projectID, len(saved))

	// Spawn the analysis pipeline. context.Background so a closed browser
	// tab doesn't kill the run mid-flight.
	go h.runJob(context.Background(), job, projectID, saved)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":      jobID,
		"total_files": len(saved),
	})
}

// Status handles GET /projects/{id}/clips/smart-import/{job_id}. Lock-free
// snapshot read from the registry.
func (h *Handlers) Status(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "job_id")
	snap, ok := h.Registry.Get(jobID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"code": "JOB_NOT_FOUND", "message": "Unknown job id (or expired after 30 min).",
		})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// runJob is the background pipeline: per-file ffprobe + AudioRMS, classifier,
// then DB inserts. Updates the job's phase + counter as it goes so the UI
// poll can render progress.
func (h *Handlers) runJob(ctx context.Context, job *Job, projectID int64, saved []savedFile) {
	defer func() {
		if r := recover(); r != nil {
			job.Fail(fmt.Sprintf("internal error: %v", r))
			log.Printf("smart-import panic: project=%d job=%s err=%v", projectID, job.ID, r)
		}
	}()

	// Phase: analyzing_audio. ffprobe each file, then ffmpeg AudioRMS.
	job.SetPhase(PhaseAnalyzingAudio, 0, "")
	metrics := make([]FileMetrics, 0, len(saved))
	for i, sf := range saved {
		job.SetPhase(PhaseAnalyzingAudio, i, sf.Filename)

		fm := FileMetrics{
			Path:     sf.Path,
			Filename: sf.Filename,
			Mtime:    sf.Mtime,
		}

		// ffprobe → duration. Best-effort: clip with bad metadata still
		// gets classified by mtime alone.
		if md, err := ffprobe.Probe(ctx, sf.Path); err == nil && md != nil {
			fm.DurationSeconds = md.DurationSeconds
		} else if err != nil {
			log.Printf("smart-import: ffprobe %s: %v", sf.Filename, err)
		}

		// AudioRMS → 1-second RMS frames; AnalyzeRMS reduces to 2 scalars.
		if frames, err := ffmpeg.AudioRMS(ctx, sf.Path); err == nil {
			a := ffmpeg.AnalyzeRMS(frames)
			fm.RMS90thPercentile = a.RMS90thPercentile
			fm.SustainedHighSeconds = a.SustainedHighSeconds
		} else {
			// No audio stream / ffmpeg failed: leave both at 0; classifier
			// treats this as "very quiet" and won't pick it as freefall.
			log.Printf("smart-import: AudioRMS %s: %v", sf.Filename, err)
		}

		metrics = append(metrics, fm)
	}
	// Mark all files done in the analysis phase.
	job.SetPhase(PhaseAnalyzingAudio, len(saved), "")

	// Phase: classifying.
	job.SetPhase(PhaseClassifying, 0, "")
	result := Classify(metrics)
	if len(result.Assignments) == 0 {
		job.Fail("Could not assign any clips — make sure the folder has video files with audio.")
		return
	}

	// Phase: creating_clips. Run ffprobe-based metadata-fill in the same
	// loop so the clip board renders complete on first reload (audio codec,
	// resolution, fps, etc).
	job.SetPhase(PhaseCreatingClips, 0, "")
	for i, a := range result.Assignments {
		row := state.Clip{
			ProjectID:       projectID,
			Kind:            a.Kind,
			SourcePath:      a.Path,
			SourceFilename:  a.Filename,
		}
		if fi, err := os.Stat(a.Path); err == nil {
			row.SourceSizeBytes = fi.Size()
		}
		if md, err := ffprobe.Probe(ctx, a.Path); err == nil && md != nil {
			row.DurationSeconds = md.DurationSeconds
			row.Codec = md.Codec
			row.Width = md.Width
			row.Height = md.Height
			row.FPS = md.FPS
			row.HasAudio = md.HasAudio
			row.AudioCodec = md.AudioCodec
			row.TrimInSeconds = 0
			row.TrimOutSeconds = md.DurationSeconds
		}

		clipID, err := h.StateDB.UpsertClip(ctx, row)
		if err != nil {
			job.Fail(fmt.Sprintf("failed inserting clip %s: %v", a.Filename, err))
			return
		}
		// Override position to the classifier's value (UpsertClip uses
		// canonical defaults for known kinds; we want our specific
		// numbering so custom slots interleave correctly).
		if err := h.StateDB.SetClipPosition(ctx, clipID, a.Position); err != nil {
			log.Printf("smart-import: SetClipPosition %d: %v", clipID, err)
		}
		job.SetPhase(PhaseCreatingClips, i+1, a.Filename)
	}

	job.Finish(result.Assignments, result.FreefallConfidence)
}

// savedFile records one disk-saved upload for the background goroutine.
type savedFile struct {
	Path     string
	Filename string
	Mtime    time.Time
}

func saveUpload(fh *multipart.FileHeader, dstDir string) (savedFile, error) {
	src, err := fh.Open()
	if err != nil {
		return savedFile{}, err
	}
	defer src.Close()

	// Sanitize filename: strip directory components, replace path separators
	// (webkitGetAsEntry sometimes preserves "subdir/file.mp4").
	name := strings.ReplaceAll(filepath.Base(fh.Filename), `\`, "_")
	name = strings.ReplaceAll(name, `/`, "_")
	if name == "" || name == "." || name == ".." {
		name = fmt.Sprintf("upload-%d.mp4", time.Now().UnixNano())
	}
	// Avoid collisions: prefix with epoch nanos. Keeps original extension.
	dst := filepath.Join(dstDir, fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))

	df, err := os.Create(dst)
	if err != nil {
		return savedFile{}, err
	}
	if _, err := io.Copy(df, src); err != nil {
		_ = df.Close()
		_ = os.Remove(dst)
		return savedFile{}, err
	}
	if err := df.Close(); err != nil {
		return savedFile{}, err
	}

	// Mtime: use the file's stated modtime if the multipart form supplied
	// one (browser does for file inputs), fall back to "now". ParseMTime
	// returns time.Time{} when no header.
	mtime := time.Now()
	if fi, err := os.Stat(dst); err == nil {
		mtime = fi.ModTime()
	}

	return savedFile{Path: dst, Filename: fh.Filename, Mtime: mtime}, nil
}

func newJobID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "imp-" + hex.EncodeToString(b[:])
}

func parseInt64Param(r *http.Request, name string) (int64, error) {
	s := chi.URLParam(r, name)
	if s == "" {
		return 0, fmt.Errorf("missing url param %q", name)
	}
	return strconv.ParseInt(s, 10, 64)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
