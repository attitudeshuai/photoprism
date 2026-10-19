package batch

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/entity/search"
	"github.com/photoprism/photoprism/internal/mutex"
)

// TestSavePhotos covers SavePhotos scenarios.
func TestSavePhotos(t *testing.T) {
	t.Run("UpdatesTitleAndFavorite", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo01")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		originalTitle := photo.PhotoTitle
		originalTitleSrc := photo.TitleSrc
		originalFavorite := photo.PhotoFavorite
		originalChecked := photo.CheckedAt
		originalEdited := photo.EditedAt

		values := &PhotosForm{
			PhotoTitle:    String{Value: originalTitle + " (Batch)", Action: ActionUpdate},
			PhotoFavorite: Bool{Value: !originalFavorite, Action: ActionUpdate},
		}

		req, err := NewPhotoSaveRequest(photo, values)
		require.NoError(t, err)

		results, _, _, err := SavePhotos([]*PhotoSaveRequest{req}, false)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.True(t, results[0])

		updated := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, updated)
		assert.Equal(t, values.PhotoTitle.Value, updated.PhotoTitle)
		assert.Equal(t, entity.SrcBatch, updated.TitleSrc)
		assert.Equal(t, values.PhotoFavorite.Value, updated.PhotoFavorite)
		assert.Nil(t, updated.CheckedAt)
		assert.NotNil(t, updated.EditedAt)

		restorePhoto(t, fixture.PhotoUID, entity.Values{
			"photo_title":    originalTitle,
			"title_src":      originalTitleSrc,
			"photo_favorite": originalFavorite,
			"checked_at":     originalChecked,
			"edited_at":      originalEdited,
		})
	})
	t.Run("UpdatesDateFields", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo02")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		originalYear := photo.PhotoYear
		originalMonth := photo.PhotoMonth
		originalDay := photo.PhotoDay
		originalTakenAt := photo.TakenAt
		originalTakenAtLocal := photo.TakenAtLocal
		originalTimeZone := photo.TimeZone
		originalTakenSrc := photo.TakenSrc
		originalChecked := photo.CheckedAt
		originalEdited := photo.EditedAt

		values := &PhotosForm{
			PhotoYear:  Int{Value: 2020, Action: ActionUpdate},
			PhotoMonth: Int{Value: 5, Action: ActionUpdate},
			PhotoDay:   Int{Value: 15, Action: ActionUpdate},
		}

		req, err := NewPhotoSaveRequest(photo, values)
		require.NoError(t, err)

		results, _, _, err := SavePhotos([]*PhotoSaveRequest{req}, false)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.True(t, results[0])

		updated := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, updated)
		assert.Equal(t, 2020, updated.PhotoYear)
		assert.Equal(t, 5, updated.PhotoMonth)
		assert.Equal(t, 15, updated.PhotoDay)
		assert.Equal(t, entity.SrcBatch, updated.TakenSrc)
		assert.Nil(t, updated.CheckedAt)

		restorePhoto(t, fixture.PhotoUID, entity.Values{
			"photo_year":     originalYear,
			"photo_month":    originalMonth,
			"photo_day":      originalDay,
			"taken_at":       originalTakenAt,
			"taken_at_local": originalTakenAtLocal,
			"time_zone":      originalTimeZone,
			"taken_src":      originalTakenSrc,
			"checked_at":     originalChecked,
			"edited_at":      originalEdited,
		})
	})
	t.Run("RemovesStrings", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo03")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		original := entity.Values{
			"photo_title":           photo.PhotoTitle,
			"title_src":             photo.TitleSrc,
			"photo_caption":         photo.PhotoCaption,
			"caption_src":           photo.CaptionSrc,
			"details_subject":       photo.GetDetails().Subject,
			"details_subject_src":   photo.GetDetails().SubjectSrc,
			"details_artist":        photo.GetDetails().Artist,
			"details_artist_src":    photo.GetDetails().ArtistSrc,
			"details_copyright":     photo.GetDetails().Copyright,
			"details_copyright_src": photo.GetDetails().CopyrightSrc,
			"details_license":       photo.GetDetails().License,
			"details_license_src":   photo.GetDetails().LicenseSrc,
			"checked_at":            photo.CheckedAt,
			"edited_at":             photo.EditedAt,
		}

		setValues := &PhotosForm{
			PhotoTitle:       String{Value: "Batch Title", Action: ActionUpdate},
			PhotoCaption:     String{Value: "Batch Caption", Action: ActionUpdate},
			DetailsSubject:   String{Value: "Batch Subject", Action: ActionUpdate},
			DetailsArtist:    String{Value: "Batch Artist", Action: ActionUpdate},
			DetailsCopyright: String{Value: "Batch Copyright", Action: ActionUpdate},
			DetailsLicense:   String{Value: "Batch License", Action: ActionUpdate},
		}
		setReq, err := NewPhotoSaveRequest(photo, setValues)
		require.NoError(t, err)
		_, _, _, err = SavePhotos([]*PhotoSaveRequest{setReq}, false)
		require.NoError(t, err)

		removeValues := &PhotosForm{
			PhotoTitle:       String{Action: ActionRemove},
			PhotoCaption:     String{Action: ActionRemove},
			DetailsSubject:   String{Action: ActionRemove},
			DetailsArtist:    String{Action: ActionRemove},
			DetailsCopyright: String{Action: ActionRemove},
			DetailsLicense:   String{Action: ActionRemove},
			PhotoYear:        Int{Value: -1, Action: ActionUpdate},
			PhotoMonth:       Int{Value: -1, Action: ActionUpdate},
			PhotoDay:         Int{Value: -1, Action: ActionUpdate},
			PhotoAltitude:    Int{Value: 0, Action: ActionUpdate},
		}
		removeReq, err := NewPhotoSaveRequest(photo, removeValues)
		require.NoError(t, err)
		results, _, _, err := SavePhotos([]*PhotoSaveRequest{removeReq}, false)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.True(t, results[0])

		updated := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, updated)
		assert.Equal(t, "", updated.PhotoTitle)
		assert.Equal(t, "", updated.PhotoCaption)
		assert.Equal(t, "", updated.GetDetails().Subject)
		assert.Equal(t, "", updated.GetDetails().Artist)
		assert.Equal(t, "", updated.GetDetails().Copyright)
		assert.Equal(t, "", updated.GetDetails().License)

		restorePhoto(t, fixture.PhotoUID, original)
	})
}

// TestBatchChangesTime covers the date/time-change detection used to trigger index regeneration.
func TestBatchChangesTime(t *testing.T) {
	t.Run("Nil", func(t *testing.T) {
		assert.False(t, batchChangesTime(nil))
	})
	t.Run("NoTimeChange", func(t *testing.T) {
		assert.False(t, batchChangesTime(&PhotosForm{PhotoTitle: String{Value: "x", Action: ActionUpdate}}))
	})
	t.Run("Year", func(t *testing.T) {
		assert.True(t, batchChangesTime(&PhotosForm{PhotoYear: Int{Value: 2000, Action: ActionUpdate}}))
	})
	t.Run("Month", func(t *testing.T) {
		assert.True(t, batchChangesTime(&PhotosForm{PhotoMonth: Int{Value: 5, Action: ActionUpdate}}))
	})
	t.Run("Day", func(t *testing.T) {
		assert.True(t, batchChangesTime(&PhotosForm{PhotoDay: Int{Value: 5, Action: ActionUpdate}}))
	})
	t.Run("TimeZone", func(t *testing.T) {
		assert.True(t, batchChangesTime(&PhotosForm{TimeZone: String{Value: "UTC", Action: ActionUpdate}}))
	})
}

// TestBatchChangesDate covers the shared calendar-date predicate; a time-zone-only change must not
// count as a date change so convert.go recomputes TakenAtLocal only when day, month, or year moved.
func TestBatchChangesDate(t *testing.T) {
	t.Run("Nil", func(t *testing.T) {
		assert.False(t, batchChangesDate(nil))
	})
	t.Run("NoDateChange", func(t *testing.T) {
		assert.False(t, batchChangesDate(&PhotosForm{PhotoTitle: String{Value: "x", Action: ActionUpdate}}))
	})
	t.Run("TimeZoneOnly", func(t *testing.T) {
		assert.False(t, batchChangesDate(&PhotosForm{TimeZone: String{Value: "UTC", Action: ActionUpdate}}))
	})
	t.Run("Year", func(t *testing.T) {
		assert.True(t, batchChangesDate(&PhotosForm{PhotoYear: Int{Value: 2000, Action: ActionUpdate}}))
	})
	t.Run("Month", func(t *testing.T) {
		assert.True(t, batchChangesDate(&PhotosForm{PhotoMonth: Int{Value: 5, Action: ActionUpdate}}))
	})
	t.Run("Day", func(t *testing.T) {
		assert.True(t, batchChangesDate(&PhotosForm{PhotoDay: Int{Value: 5, Action: ActionUpdate}}))
	})
}

// TestNewPhotoSaveRequest ensures the helper validates inputs before building requests.
func TestNewPhotoSaveRequest(t *testing.T) {
	t.Run("NilValues", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo01")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		req, err := NewPhotoSaveRequest(photo, nil)
		assert.Nil(t, req)
		assert.Error(t, err)
	})
	t.Run("BuildsRequest", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo02")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		values := &PhotosForm{
			PhotoTitle: String{Value: "Helper", Action: ActionUpdate},
		}

		req, err := NewPhotoSaveRequest(photo, values)
		require.NoError(t, err)
		require.NotNil(t, req)
		assert.Equal(t, photo, req.Photo)
		assert.Equal(t, values, req.Values)
		assert.NotNil(t, req.Form)
		assert.Equal(t, "Helper", req.Form.PhotoTitle)
	})
}

// TestPreparePhotoSaveRequests verifies helper behavior.
func TestPreparePhotoSaveRequests(t *testing.T) {
	t.Run("NilValues", func(t *testing.T) {
		preloaded := map[string]*entity.Photo{}
		requests, updated, stats, failures := PreparePhotoSaveRequests(nil, preloaded, nil)
		assert.Nil(t, requests)
		assert.Equal(t, preloaded, updated)
		assert.Equal(t, MutationStats{}, stats)
		assert.Nil(t, failures)
	})
	t.Run("LoadsMissingPhoto", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo01")
		values := &PhotosForm{PhotoTitle: String{Value: "Prepared", Action: ActionUpdate}}
		photos := search.PhotoResults{{PhotoUID: fixture.PhotoUID}}

		requests, updated, stats, failures := PreparePhotoSaveRequests(photos, nil, values)
		require.Len(t, requests, 1)
		assert.NotNil(t, requests[0].Photo)
		assert.Equal(t, "Prepared", requests[0].Form.PhotoTitle)
		assert.Contains(t, updated, fixture.PhotoUID)
		assert.Equal(t, MutationStats{}, stats)
		assert.Nil(t, failures)
	})
	t.Run("SkipsMissing", func(t *testing.T) {
		values := &PhotosForm{PhotoTitle: String{Value: "Prepared", Action: ActionUpdate}}
		photos := search.PhotoResults{{PhotoUID: "pt_does_not_exist"}}

		requests, updated, stats, failures := PreparePhotoSaveRequests(photos, nil, values)
		assert.Len(t, requests, 0)
		assert.Empty(t, updated)
		assert.Equal(t, MutationStats{}, stats)
		require.Contains(t, failures, "pt_does_not_exist")
	})
}

// TestPrepareAndSavePhotos verifies the full helper workflow.
func TestPrepareAndSavePhotos(t *testing.T) {
	t.Run("NilValues", func(t *testing.T) {
		result, err := PrepareAndSavePhotos(nil, nil, nil, false)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.NotNil(t, result.Preloaded)
		assert.Len(t, result.Requests, 0)
		assert.Len(t, result.Results, 0)
	})
	t.Run("PersistsChanges", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo02")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		originalFavorite := photo.PhotoFavorite

		values := &PhotosForm{
			PhotoFavorite: Bool{Value: !originalFavorite, Action: ActionUpdate},
		}
		photos := search.PhotoResults{{PhotoUID: fixture.PhotoUID}}

		result, err := PrepareAndSavePhotos(photos, nil, values, false)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, result.Requests, 1)
		require.Len(t, result.Results, 1)
		assert.True(t, result.Results[0])
		assert.True(t, result.SavedAny)
		assert.Equal(t, 1, result.UpdatedCount)
		assert.Contains(t, result.Preloaded, fixture.PhotoUID)

		updated := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, updated)
		assert.Equal(t, !originalFavorite, updated.PhotoFavorite)

		restorePhoto(t, fixture.PhotoUID, entity.Values{
			"photo_favorite": originalFavorite,
		})
	})
	t.Run("RegeneratesFileIndexOnDateChange", func(t *testing.T) {
		// Find a photo whose primary file carries a search time index.
		var f entity.File
		if err := entity.UnscopedDb().Where("photo_id > 0 AND file_primary = TRUE AND time_index IS NOT NULL AND deleted_at IS NULL").First(&f).Error; err != nil {
			t.Skip("no suitable file fixture")
			return
		}

		var photo entity.Photo
		require.NoError(t, entity.UnscopedDb().First(&photo, f.PhotoID).Error)

		origIndex := ""
		if f.TimeIndex != nil {
			origIndex = *f.TimeIndex
		}
		original := entity.Values{
			"taken_at":       photo.TakenAt,
			"taken_at_local": photo.TakenAtLocal,
			"photo_year":     photo.PhotoYear,
			"photo_month":    photo.PhotoMonth,
			"photo_day":      photo.PhotoDay,
			"time_zone":      photo.TimeZone,
			"taken_src":      photo.TakenSrc,
			"checked_at":     photo.CheckedAt,
			"edited_at":      photo.EditedAt,
		}

		newYear := 1975
		if photo.TakenAtLocal.Year() == newYear {
			newYear = 1985
		}
		values := &PhotosForm{PhotoYear: Int{Value: newYear, Action: ActionUpdate}}
		photos := search.PhotoResults{{PhotoUID: photo.PhotoUID}}

		result, err := PrepareAndSavePhotos(photos, nil, values, false)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.SavedAny)

		var updated entity.File
		require.NoError(t, entity.UnscopedDb().First(&updated, f.ID).Error)
		require.NotNil(t, updated.TimeIndex)
		assert.NotEqual(t, origIndex, *updated.TimeIndex, "time_index must be regenerated after a batch date change")
		assert.Equal(t, newYear, updated.PhotoTakenAt.Year())

		restorePhoto(t, photo.PhotoUID, original)
		entity.RegenerateIndexForPhotoIDs([]uint{photo.ID})
	})
	t.Run("RejectsConcurrentBatchEdit", func(t *testing.T) {
		require.NoError(t, mutex.BatchEdit.Start())
		defer mutex.BatchEdit.Stop()

		result, err := PrepareAndSavePhotos(search.PhotoResults{}, nil, &PhotosForm{}, false)
		assert.Nil(t, result)
		assert.ErrorIs(t, err, ErrBatchEditBusy)
	})
	t.Run("CancelsMetaWorkerWhenRunning", func(t *testing.T) {
		require.NoError(t, mutex.MetaWorker.Start())
		t.Cleanup(func() { mutex.MetaWorker.Stop() })

		result, err := PrepareAndSavePhotos(search.PhotoResults{}, nil, &PhotosForm{}, false)
		require.NoError(t, err)
		require.NotNil(t, result)

		assert.True(t, mutex.MetaWorker.Canceled())
	})
	t.Run("AbortsOnCancellation", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo02")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		values := &PhotosForm{
			PhotoFavorite: Bool{Value: !photo.PhotoFavorite, Action: ActionUpdate},
		}

		req, err := NewPhotoSaveRequest(photo, values)
		require.NoError(t, err)

		require.NoError(t, mutex.BatchEdit.Start())
		t.Cleanup(func() { mutex.BatchEdit.Stop() })

		mutex.BatchEdit.Cancel()

		results, itemErrs, stopped, err := SavePhotos([]*PhotoSaveRequest{req}, false)
		assert.ErrorIs(t, err, ErrBatchEditCanceled)
		assert.Len(t, results, 1)
		assert.Len(t, itemErrs, 1)
		assert.False(t, stopped)
		assert.False(t, results[0])
	})
}

// TestSavePhotosStopOnError verifies that fail-fast mode leaves later requests untouched.
func TestSavePhotosStopOnError(t *testing.T) {
	t.Run("StopsAfterFirstError", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo01")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		values := &PhotosForm{PhotoTitle: String{Value: "Stop On Error", Action: ActionUpdate}}
		req, err := NewPhotoSaveRequest(photo, values)
		require.NoError(t, err)

		// An invalid request fails saving without touching the database.
		badReq := &PhotoSaveRequest{Photo: &entity.Photo{}, Form: nil, Values: values}

		results, itemErrs, stopped, err := SavePhotos([]*PhotoSaveRequest{badReq, req}, true)
		require.NoError(t, err)
		assert.True(t, stopped)
		require.Len(t, results, 2)
		require.Len(t, itemErrs, 2)
		assert.Error(t, itemErrs[0])
		assert.Nil(t, itemErrs[1], "remaining request must not be processed in fail-fast mode")
		assert.False(t, results[1])

		// The valid photo must remain unchanged.
		current := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, current)
		assert.NotEqual(t, "Stop On Error", current.PhotoTitle)
	})
}

// TestSaveBatchItemResults covers the per-item result assembly, including not-found items,
// no-op saves, save errors, and fail-fast leftovers.
func TestSaveBatchItemResults(t *testing.T) {
	t.Run("AllSucceededWithNoOp", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo01")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		r := &SaveBatchResult{
			Requests: []*PhotoSaveRequest{{Photo: photo}},
			Results:  []bool{false},
			ItemErrs: []error{nil},
		}

		res := r.ItemResults([]string{fixture.PhotoUID})
		assert.Equal(t, "success", res.Status)
		require.Len(t, res.Items, 1)
		assert.True(t, res.Items[0].OK)
	})
	t.Run("NotFoundAndSaveError", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo02")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		r := &SaveBatchResult{
			Requests: []*PhotoSaveRequest{{Photo: photo}},
			Results:  []bool{false},
			ItemErrs: []error{errors.New("disk full")},
		}

		res := r.ItemResults([]string{fixture.PhotoUID, "ps_missing"})
		assert.Equal(t, "failed", res.Status)
		require.Len(t, res.Items, 2)
		assert.False(t, res.Items[0].OK)
		assert.Contains(t, res.Items[0].Error, "disk full")
		assert.False(t, res.Items[1].OK)
		assert.Contains(t, res.Items[1].Error, "not found")
	})
	t.Run("PartialNotFound", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo02")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		r := &SaveBatchResult{
			Requests: []*PhotoSaveRequest{{Photo: photo}},
			Results:  []bool{false},
			ItemErrs: []error{nil},
		}

		res := r.ItemResults([]string{fixture.PhotoUID, "ps_missing"})
		assert.Equal(t, "partial", res.Status)
		assert.Equal(t, 1, res.SuccessCount)
		assert.Equal(t, 1, res.FailCount)
		require.Len(t, res.Items, 2)
		assert.True(t, res.Items[0].OK)
		assert.False(t, res.Items[1].OK)
		assert.Contains(t, res.Items[1].Error, "not found")
	})
	t.Run("PreparationFailure", func(t *testing.T) {
		fixture := entity.PhotoFixtures.Get("Photo03")
		photo := entity.FindPhoto(entity.Photo{PhotoUID: fixture.PhotoUID})
		require.NotNil(t, photo)

		r := &SaveBatchResult{
			Requests: []*PhotoSaveRequest{{Photo: photo}},
			Results:  []bool{true},
			ItemErrs: []error{nil},
			Failures: map[string]error{fixture.PhotoUID: errors.New("labels: invalid label uid format: x")},
		}

		res := r.ItemResults([]string{fixture.PhotoUID})
		assert.Equal(t, "failed", res.Status)
		require.Len(t, res.Items, 1)
		assert.False(t, res.Items[0].OK)
		assert.Contains(t, res.Items[0].Error, "labels")
	})
	t.Run("StoppedMarksRemaining", func(t *testing.T) {
		f1 := entity.PhotoFixtures.Get("Photo01")
		f2 := entity.PhotoFixtures.Get("Photo02")
		p1 := entity.FindPhoto(entity.Photo{PhotoUID: f1.PhotoUID})
		p2 := entity.FindPhoto(entity.Photo{PhotoUID: f2.PhotoUID})
		require.NotNil(t, p1)
		require.NotNil(t, p2)

		r := &SaveBatchResult{
			Requests: []*PhotoSaveRequest{{Photo: p1}, {Photo: p2}},
			Results:  []bool{false, false},
			ItemErrs: []error{errors.New("save failed"), nil},
			Stopped:  true,
		}

		res := r.ItemResults([]string{f1.PhotoUID, f2.PhotoUID, "ps_missing"})
		assert.True(t, res.Stopped)
		assert.Equal(t, "failed", res.Status)
		require.Len(t, res.Items, 3)
		assert.Contains(t, res.Items[0].Error, "save failed")
		assert.Contains(t, res.Items[1].Error, "stopped")
		assert.Contains(t, res.Items[2].Error, "stopped")
	})
}

// restorePhoto rewinds DB state for the provided fixture so tests stay isolated.
func restorePhoto(t *testing.T, photoUID string, values entity.Values) {
	t.Helper()
	if values == nil {
		return
	}
	detailUpdates := entity.Values{}
	detailKeys := []string{
		"details_subject", "details_subject_src",
		"details_artist", "details_artist_src",
		"details_copyright", "details_copyright_src",
		"details_license", "details_license_src",
	}
	for _, k := range detailKeys {
		if v, ok := values[k]; ok {
			detailUpdates[strings.TrimPrefix(k, "details_")] = v
			delete(values, k)
		}
	}
	if err := entity.Db().Model(&entity.Photo{}).Where("photo_uid = ?", photoUID).Updates(values).Error; err != nil {
		t.Fatalf("failed to restore photo %s: %v", photoUID, err)
	}
	if len(detailUpdates) > 0 {
		if photo := entity.FindPhoto(entity.Photo{PhotoUID: photoUID}); photo != nil {
			if err := entity.Db().Model(photo.GetDetails()).Updates(detailUpdates).Error; err != nil {
				t.Fatalf("failed to restore photo details %s: %v", photoUID, err)
			}
		}
	}
}
