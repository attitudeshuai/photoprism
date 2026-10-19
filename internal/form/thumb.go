package form

import "errors"

// Thumbnail status search defaults and limits.
const (
	ThumbStatusDefaultCount = 100
	ThumbStatusMaxCount     = 1000

	thumbStatusPending   = "pending"
	thumbStatusFailed    = "failed"
	thumbStatusSkipped   = "skipped"
	thumbStatusSucceeded = "succeeded"
)

// ThumbStatusSearch represents the search form for per-file thumbnail size results.
type ThumbStatusSearch struct {
	Status string `form:"status" json:"status,omitempty"`
	Path   string `form:"path" json:"path,omitempty"`
	Count  int    `form:"count" json:"count,omitempty" serialize:"-"`
	Offset int    `form:"offset" json:"offset,omitempty" serialize:"-"`
}

// Normalize applies default and maximum result counts and rejects invalid values.
// An empty status selects the actionable states (pending and failed).
func (f *ThumbStatusSearch) Normalize() error {
	switch f.Status {
	case "", thumbStatusPending, thumbStatusFailed, thumbStatusSkipped, thumbStatusSucceeded:
		// Empty or known status values are accepted.
	default:
		return errors.New("status must be one of pending, failed, skipped, or succeeded")
	}

	if f.Count == 0 {
		f.Count = ThumbStatusDefaultCount
	} else if f.Count < 0 {
		return errors.New("count must not be negative")
	} else if f.Count > ThumbStatusMaxCount {
		f.Count = ThumbStatusMaxCount
	}

	if f.Offset < 0 {
		return errors.New("offset must not be negative")
	}

	return nil
}
