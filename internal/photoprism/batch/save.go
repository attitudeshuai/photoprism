package batch

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/photoprism/photoprism/internal/batchresult"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/entity/query"
	"github.com/photoprism/photoprism/internal/entity/search"
	"github.com/photoprism/photoprism/internal/form"
	"github.com/photoprism/photoprism/internal/mutex"
	"github.com/photoprism/photoprism/pkg/txt"
)

// PhotoSaveRequest bundles the context required to persist a batch update for a single photo.
type PhotoSaveRequest struct {
	Photo  *entity.Photo
	Form   *form.Photo
	Values *PhotosForm
}

// SaveBatchResult describes the outcome of PrepareAndSavePhotos so callers can publish events
// and build responses without re-walking the selection.
type SaveBatchResult struct {
	Requests     []*PhotoSaveRequest
	Results      []bool
	ItemErrs     []error
	Failures     map[string]error
	Stopped      bool
	Preloaded    map[string]*entity.Photo
	UpdatedCount int
	SavedAny     bool
	Stats        MutationStats
}

// MutationStats captures how many photos received supporting mutations and
// how many errors occurred while applying them.
type MutationStats struct {
	AlbumMutations int
	LabelMutations int
	AlbumErrors    int
	LabelErrors    int
}

// ErrBatchEditBusy is returned when another batch edit is already running.
var ErrBatchEditBusy = errors.New("edit already running")

// ErrBatchEditCanceled is returned when a batch edit was canceled (e.g. during shutdown).
var ErrBatchEditCanceled = errors.New("edit canceled")

// NewPhotoSaveRequest converts the batch values into a form.Photo and bundles it with the
// target entity so callers outside this package do not have to depend on ConvertToPhotoForm.
func NewPhotoSaveRequest(photo *entity.Photo, values *PhotosForm) (*PhotoSaveRequest, error) {
	if values == nil {
		return nil, fmt.Errorf("batch: values are required")
	}

	frm, err := ConvertToPhotoForm(photo, values)
	if err != nil {
		return nil, err
	}

	return &PhotoSaveRequest{Photo: photo, Form: frm, Values: values}, nil
}

// PreparePhotoSaveRequests converts the given selection into save requests, ensuring each photo
// entity is hydrated and album/label actions are applied once per selection. The returned map is
// the (possibly newly populated) preloaded set so callers can reuse it for response rendering.
// Failures returns the per-UID errors for photos that could not be loaded or whose album/label
// mutations could not be applied; entries stay in selection order via the caller's request list.
func PreparePhotoSaveRequests(photos search.PhotoResults, preloaded map[string]*entity.Photo, values *PhotosForm) (requests []*PhotoSaveRequest, result map[string]*entity.Photo, stats MutationStats, failures map[string]error) {
	if values == nil {
		return nil, preloaded, MutationStats{}, nil
	}

	// Pre-create any album/label add targets so Apply* can reuse resolved UIDs per photo.
	resolveBatchItemValues(values)

	if preloaded == nil {
		preloaded = map[string]*entity.Photo{}
	}

	log.Infof("batch: updating %d photos", len(photos))

	requests = make([]*PhotoSaveRequest, 0, len(photos))
	stats = MutationStats{}
	failures = make(map[string]error)

	// fail records the first error for a photo; album/label errors are joined with later causes.
	fail := func(uid string, err error) {
		if err == nil {
			return
		}

		if prev, ok := failures[uid]; ok {
			failures[uid] = fmt.Errorf("%w; %s", prev, err)
		} else {
			failures[uid] = err
		}
	}

	for _, result := range photos {
		photoID := result.PhotoUID

		if photoID == "" {
			continue
		}

		fullPhoto := preloaded[photoID]

		if fullPhoto == nil {
			loaded, err := query.PhotoPreloadByUID(photoID)
			if err != nil {
				log.Errorf("batch: failed to load photo %s: %s", photoID, err)
				fail(photoID, fmt.Errorf("%s: %w", photoID, batchresult.ErrNotFound))
				continue
			}
			fullPhoto = &loaded
			preloaded[photoID] = fullPhoto
		}

		saveReq, err := NewPhotoSaveRequest(fullPhoto, values)

		if err != nil {
			log.Errorf("batch: failed to build save request for photo %s: %s", photoID, err)
			fail(photoID, err)
			continue
		}

		requests = append(requests, saveReq)

		if values.Albums.Action == ActionUpdate {
			if errs := ApplyAlbums(fullPhoto, values.Albums); errs != nil {
				log.Errorf("batch: failed to update albums for photo %s: (%s)", photoID, errs)
				stats.AlbumErrors += len(errs)
				fail(photoID, fmt.Errorf("albums: %s", joinErrors(errs)))
			}

			if len(values.Albums.Items) > 0 {
				stats.AlbumMutations++
			}
		}

		if values.Labels.Action == ActionUpdate {
			if errs := ApplyLabels(fullPhoto, values.Labels); errs != nil {
				log.Errorf("batch: failed to update labels for photo %s (%s)", photoID, errs)
				stats.LabelErrors += len(errs)
				fail(photoID, fmt.Errorf("labels: %s", joinErrors(errs)))
			}

			if len(values.Labels.Items) > 0 {
				stats.LabelMutations++
			}
		}
	}

	if len(failures) == 0 {
		failures = nil
	}

	return requests, preloaded, stats, failures
}

// joinErrors formats multiple errors as one readable failure reason.
func joinErrors(errs []error) string {
	parts := make([]string, 0, len(errs))

	for _, err := range errs {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}

	return strings.Join(parts, "; ")
}

// PrepareAndSavePhotos hydrates the photo selection, applies album/label mutations, builds save
// requests, and persists the changes in one step. It returns a SaveBatchResult so callers can run
// follow-up work (events, cache flushes) without re-querying state. When stopOnError is true, the
// first failed photo ends the batch and all following items are reported as not processed.
func PrepareAndSavePhotos(photos search.PhotoResults, preloaded map[string]*entity.Photo, values *PhotosForm, stopOnError bool) (*SaveBatchResult, error) {
	start := time.Now()
	result := &SaveBatchResult{Preloaded: preloaded}

	if values == nil {
		if result.Preloaded == nil {
			result.Preloaded = map[string]*entity.Photo{}
		}
		return result, nil
	}

	if err := mutex.BatchEdit.Start(); err != nil {
		return nil, ErrBatchEditBusy
	}

	defer mutex.BatchEdit.Stop()

	// Prevent concurrent metadata worker runs while batch updates are being applied.
	mutex.MetaWorker.Cancel()

	requests, preloaded, mutationStats, failures := PreparePhotoSaveRequests(photos, result.Preloaded, values)

	result.Requests = requests
	result.Preloaded = preloaded
	result.Stats = mutationStats
	result.Failures = failures

	if len(requests) == 0 {
		return result, nil
	}

	saveResults, itemErrs, stopped, err := SavePhotos(requests, stopOnError)

	if err != nil {
		return nil, err
	}

	result.Results = saveResults
	result.ItemErrs = itemErrs
	result.Stopped = stopped

	for i, saved := range saveResults {
		if saved {
			result.UpdatedCount++
			result.SavedAny = true
			log.Debugf("batch: successfully updated photo %s", requests[i].Photo.PhotoUID)
		}
	}

	// Regenerate the denormalized file search index for photos whose date or time zone changed,
	// so newest/oldest sorting reflects the new dates immediately; the batch save path otherwise
	// leaves files.time_index stale until the metadata worker runs. Done once per batch to keep
	// the per-photo work out of the save loop.
	if result.SavedAny && batchChangesTime(values) {
		photoIDs := make([]uint, 0, len(requests))
		for i, saved := range saveResults {
			if saved && requests[i] != nil && requests[i].Photo != nil && requests[i].Photo.ID > 0 {
				photoIDs = append(photoIDs, requests[i].Photo.ID)
			}
		}
		entity.RegenerateIndexForPhotoIDs(photoIDs)
	}

	var logFields []string

	if result.UpdatedCount > 0 {
		logFields = append(logFields, fmt.Sprintf("metadata (%d/%d)", result.UpdatedCount, len(photos)))
	}

	if result.Stats.LabelMutations > 0 {
		entry := fmt.Sprintf("labels (%d/%d)", result.Stats.LabelMutations, len(photos))
		if result.Stats.LabelErrors > 0 {
			entry = fmt.Sprintf("%s with %d errors", entry, result.Stats.LabelErrors)
		}
		logFields = append(logFields, entry)
	}

	if result.Stats.AlbumMutations > 0 {
		entry := fmt.Sprintf("albums (%d/%d)", result.Stats.AlbumMutations, len(photos))
		if result.Stats.AlbumErrors > 0 {
			entry = fmt.Sprintf("%s with %d errors", entry, result.Stats.AlbumErrors)
		}
		logFields = append(logFields, entry)
	}

	if len(logFields) > 0 {
		log.Infof("batch: updated photo %s [%s]", txt.JoinAnd(logFields), time.Since(start))
	} else {
		log.Infof("batch: no photos have been updated [%s]", time.Since(start))
	}

	// Update YAML backups for all albums referenced in the current batch request.
	if result.Stats.AlbumMutations > 0 {
		updateAlbumBackups(values)
	}

	return result, nil
}

// batchChangesDate reports whether the batch updates any calendar date field (day, month, or year).
// It is the shared source of truth for the date-mutating field set: convert.go recomputes TakenAtLocal
// from it and batchChangesTime extends it with the time zone. Keep it aligned with the fields actually
// applied in convert.go so date and sort-order handling never silently drift apart.
func batchChangesDate(values *PhotosForm) bool {
	if values == nil {
		return false
	}

	return values.PhotoDay.Action == ActionUpdate ||
		values.PhotoMonth.Action == ActionUpdate ||
		values.PhotoYear.Action == ActionUpdate
}

// batchChangesTime reports whether the batch updates any date or time zone field, which requires
// regenerating the denormalized file search index so newest/oldest sorting reflects the new dates.
// The time zone is included on top of the date fields because it shifts the derived UTC instant (and
// thus the file sort key) even when the naive TakenAtLocal is left unchanged.
func batchChangesTime(values *PhotosForm) bool {
	return batchChangesDate(values) || (values != nil && values.TimeZone.Action == ActionUpdate)
}

// ItemResults builds the per-item outcome for all requested photo UIDs in first-seen order so
// handlers can return the shared batchresult structure without re-walking the selection.
func (r *SaveBatchResult) ItemResults(requested []string) *batchresult.Result {
	builder := batchresult.NewBuilder(requested...)

	byUID := make(map[string]int, len(r.Requests))

	for i, req := range r.Requests {
		if req != nil && req.Photo != nil && req.Photo.PhotoUID != "" {
			byUID[req.Photo.PhotoUID] = i
		}
	}

	// In fail-fast mode, SavePhotos stops after the first save error; requests past it were never run.
	firstErr := -1

	if r.Stopped {
		for i, itemErr := range r.ItemErrs {
			if itemErr != nil {
				firstErr = i
				break
			}
		}
	}

	for _, uid := range builder.IDs() {
		switch i, found := byUID[uid]; {
		case found && r.ItemErrs != nil && r.ItemErrs[i] != nil:
			builder.Failure(uid, r.ItemErrs[i])
		case found && r.Stopped && firstErr >= 0 && i > firstErr:
			builder.Failure(uid, batchresult.ErrStopped)
		case found && r.Failures != nil && r.Failures[uid] != nil:
			builder.Failure(uid, r.Failures[uid])
		case found:
			// A no-op save (saved=false) means the requested state already holds.
			builder.Success(uid)
		case r.Failures != nil && r.Failures[uid] != nil:
			builder.Failure(uid, r.Failures[uid])
		case r.Stopped:
			builder.Failure(uid, batchresult.ErrStopped)
		default:
			builder.Failure(uid, batchresult.ErrNotFound)
		}
	}

	res := builder.Result()

	if r.Stopped {
		res.Stopped = true
	}

	return res
}

// SavePhotos persists the batch updates described by the provided requests while skipping the
// heavyweight per-photo maintenance performed by entity.SavePhotoForm. It only updates the
// columns that actually changed, flags the photo for background metadata refresh by clearing
// CheckedAt, and updates the shared counts once per batch instead of once per photo. When
// stopOnError is false, save errors are collected per request and the remaining photos are
// processed; when true, the first save error ends the batch and stopped reports this.
func SavePhotos(requests []*PhotoSaveRequest, stopOnError bool) (results []bool, itemErrs []error, stopped bool, err error) {
	results = make([]bool, len(requests))
	itemErrs = make([]error, len(requests))
	anySaved := false

	for i, req := range requests {
		if mutex.BatchEdit.Canceled() {
			return results, itemErrs, stopped, ErrBatchEditCanceled
		}

		saved, saveErr := savePhoto(req)

		if saveErr != nil {
			itemErrs[i] = saveErr

			if stopOnError {
				stopped = true
				break
			}

			continue
		}

		results[i] = saved

		if saved {
			anySaved = true
		}
	}

	if anySaved {
		entity.UpdateCountsAsync()
	}

	return results, itemErrs, stopped, nil
}
