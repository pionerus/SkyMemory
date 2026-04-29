package v1

// MusicTrack — one entry returned in catalog/suggest responses.
// Only fields the studio needs are exposed; admin-only fields stay server-side.
type MusicTrack struct {
	ID              int64    `json:"id"`
	Title           string   `json:"title"`
	Artist          string   `json:"artist,omitempty"`
	License         string   `json:"license"`
	DurationSeconds int      `json:"duration_seconds"`
	BPM             int      `json:"bpm,omitempty"`
	Mood            []string `json:"mood,omitempty"`
	SuggestedFor    []string `json:"suggested_for,omitempty"`

	// PreviewURL is a short-TTL presigned GET URL for inline playback in the studio UI.
	// Don't cache — server mints a fresh one each /music or /music/suggest call.
	PreviewURL string `json:"preview_url"`

	// FetchURL is a longer-TTL presigned GET for downloading the full track to local cache
	// just before pipeline runs. Server mints it on demand via a separate endpoint.
	// Empty in /music list responses; populated only after operator picks the track.
	FetchURL string `json:"fetch_url,omitempty"`

	// Score and Reason are populated only by the suggest endpoint, not by the
	// catalog endpoint. Score is opaque (higher = better); Reason is a human
	// sentence the UI shows under the track row.
	Score  float64 `json:"score,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

type MusicListResponse struct {
	Tracks []MusicTrack `json:"tracks"`
}

type MusicSuggestRequest struct {
	DurationSeconds int      `json:"duration_seconds"`
	Mood            []string `json:"mood,omitempty"` // optional filter
	Limit           int      `json:"limit,omitempty"`
}

type MusicSuggestResponse struct {
	Tracks []MusicTrack `json:"tracks"`
}
