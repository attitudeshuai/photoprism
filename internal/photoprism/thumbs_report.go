package photoprism

import (
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/thumb"
	"github.com/photoprism/photoprism/pkg/clean"
)

// thumbResultSink records the per-size generation outcome for one media file.
type thumbResultSink interface {
	// Start registers the sizes this run is about to attempt as pending.
	Start(sizes []thumb.Name)
	// Succeeded records a size whose thumbnail was cached already or rendered in this run.
	Succeeded(name thumb.Name)
	// Skipped records a size the generation strategy intentionally does not render for this source.
	Skipped(name thumb.Name, reason string)
	// Failed records a size that could not be rendered and may be retried later.
	Failed(name thumb.Name, cause error)
}

// eligibleThumbNames returns the eager-generation sizes within the configured cache limit.
func eligibleThumbNames() []thumb.Name {
	result := make([]thumb.Name, 0, len(thumb.Names))

	for _, name := range thumb.Names {
		if !thumb.Sizes[name].Uncached() {
			result = append(result, name)
		}
	}

	return result
}

// registerPendingThumbs pre-registers the file's eligible size results as pending before a
// generation job starts, so a canceled run leaves its discovered files in the retry list.
func registerPendingThumbs(m *MediaFile) {
	if m == nil {
		return
	}

	newDbThumbResultSink(m).Start(eligibleThumbNames())
}

// dbThumbResultSink persists per-size outcomes in the thumbs_status table.
type dbThumbResultSink struct {
	root     string
	fileName string
	hash     string
}

// newDbThumbResultSink returns a sink that records results for the given media file.
func newDbThumbResultSink(m *MediaFile) *dbThumbResultSink {
	return &dbThumbResultSink{root: m.Root(), fileName: m.RootRelName(), hash: m.Hash()}
}

// Start inserts pending rows for the attempted sizes without touching existing results.
func (s *dbThumbResultSink) Start(sizes []thumb.Name) {
	if s == nil || len(sizes) == 0 {
		return
	}

	rows := make([]entity.ThumbStatus, 0, len(sizes))

	for _, name := range sizes {
		rows = append(rows, entity.ThumbStatus{
			FileRoot: s.root,
			FileName: s.fileName,
			SizeName: name.String(),
			FileHash: s.hash,
			Status:   entity.ThumbStatusPending,
		})
	}

	if err := entity.RegisterPendingThumbSizes(rows); err != nil {
		log.Warnf("thumbs: %s while registering pending sizes for %s", clean.Error(err), clean.Log(s.fileName))
	}
}

// Succeeded stores a successful size result.
func (s *dbThumbResultSink) Succeeded(name thumb.Name) {
	s.report(name, entity.ThumbStatusSucceeded, "")
}

// Skipped stores a strategy-declared size skip so it is not mistaken for a failure.
func (s *dbThumbResultSink) Skipped(name thumb.Name, reason string) {
	s.report(name, entity.ThumbStatusSkipped, reason)
}

// Failed stores a failed size result with its sanitized error reason.
func (s *dbThumbResultSink) Failed(name thumb.Name, cause error) {
	if cause == nil {
		return
	}

	s.report(name, entity.ThumbStatusFailed, clean.Error(cause))
}

// report writes one terminal size result and logs database problems without failing generation.
func (s *dbThumbResultSink) report(name thumb.Name, status, reason string) {
	if s == nil {
		return
	}

	row := entity.ThumbStatus{
		FileRoot: s.root,
		FileName: s.fileName,
		SizeName: name.String(),
		FileHash: s.hash,
		Status:   status,
		Error:    reason,
	}

	if err := entity.ReportThumbSize(row); err != nil {
		log.Warnf("thumbs: %s while recording %s result for %s", clean.Error(err), name, clean.Log(s.fileName))
	}
}

// nopThumbResultSink discards all size outcomes, e.g. for avatar images outside the library roots.
type nopThumbResultSink struct{}

// Start does nothing.
func (nopThumbResultSink) Start(sizes []thumb.Name) {}

// Succeeded does nothing.
func (nopThumbResultSink) Succeeded(name thumb.Name) {}

// Skipped does nothing.
func (nopThumbResultSink) Skipped(name thumb.Name, reason string) {}

// Failed does nothing.
func (nopThumbResultSink) Failed(name thumb.Name, cause error) {}
