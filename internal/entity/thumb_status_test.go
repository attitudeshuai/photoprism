package entity

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/clause"
)

// onConflictDoNothing marks an insert as a no-op on primary key conflicts.
func onConflictDoNothing() clause.OnConflict {
	return clause.OnConflict{DoNothing: true}
}

// thumbStatusRow returns a status row for a unique test file and size.
func thumbStatusRow(t *testing.T, id, sizeName, status string) ThumbStatus {
	t.Helper()

	name := fmt.Sprintf("/%s/%s.jpg", t.Name(), id)

	row := NewThumbStatus(RootOriginals, name, sizeName, "hash-"+id, status)
	t.Cleanup(func() {
		_ = UnscopedDb().Delete(&ThumbStatus{}, "file_name = ?", name).Error
	})

	return *row
}

func TestThumbStatus_TableName(t *testing.T) {
	assert.Equal(t, "thumbs_status", ThumbStatus{}.TableName())
}

func TestThumbStatus_Validate(t *testing.T) {
	t.Run("Valid", func(t *testing.T) {
		for _, status := range []string{ThumbStatusPending, ThumbStatusSucceeded, ThumbStatusSkipped, ThumbStatusFailed} {
			row := NewThumbStatus(RootOriginals, "/a.jpg", "fit_720", "h", status)
			assert.NoError(t, row.Validate(), status)
		}
	})
	t.Run("MissingIdentity", func(t *testing.T) {
		assert.Error(t, ThumbStatus{Status: ThumbStatusPending}.Validate())
		assert.Error(t, NewThumbStatus(RootOriginals, "/a.jpg", "", "h", ThumbStatusPending).Validate())
	})
	t.Run("InvalidStatus", func(t *testing.T) {
		assert.Error(t, NewThumbStatus(RootOriginals, "/a.jpg", "fit_720", "h", "bogus").Validate())
	})
}

// findThumbStatus reads the current row state.
func findThumbStatus(t *testing.T, row ThumbStatus) ThumbStatus {
	t.Helper()

	var found ThumbStatus
	require.NoError(t, Db().Where("file_root = ? AND file_name = ? AND size_name = ?",
		row.FileRoot, row.FileName, row.SizeName).First(&found).Error)

	return found
}

func TestRegisterPendingThumbSizes(t *testing.T) {
	t.Run("InsertsNewRowsAsPending", func(t *testing.T) {
		a := thumbStatusRow(t, "a", "fit_720", ThumbStatusPending)
		b := thumbStatusRow(t, "a", "tile_50", ThumbStatusPending)
		require.NoError(t, RegisterPendingThumbSizes([]ThumbStatus{a, b}))

		assert.Equal(t, ThumbStatusPending, findThumbStatus(t, a).Status)
		assert.Equal(t, ThumbStatusPending, findThumbStatus(t, b).Status)
	})

	t.Run("DoesNotOverwriteExistingFailure", func(t *testing.T) {
		a := thumbStatusRow(t, "b", "fit_720", ThumbStatusFailed)
		a.Error = "boom"
		a.Attempts = 3
		require.NoError(t, UnscopedDb().Create(&a).Error)

		pending := NewThumbStatus(a.FileRoot, a.FileName, a.SizeName, a.FileHash, ThumbStatusPending)
		require.NoError(t, RegisterPendingThumbSizes([]ThumbStatus{*pending}))

		found := findThumbStatus(t, a)
		assert.Equal(t, ThumbStatusFailed, found.Status)
		assert.Equal(t, "boom", found.Error)
		assert.Equal(t, 3, found.Attempts)
	})

	t.Run("EmptyAndBatching", func(t *testing.T) {
		assert.NoError(t, RegisterPendingThumbSizes(nil))

		var rows []ThumbStatus
		for i := 0; i < 205; i++ {
			r := thumbStatusRow(t, fmt.Sprintf("c%03d", i), "fit_720", ThumbStatusPending)
			rows = append(rows, r)
			// Keep cleanup list unique: all rows share distinct names already.
		}

		require.NoError(t, RegisterPendingThumbSizes(rows))

		var count int64
		require.NoError(t, Db().Model(&ThumbStatus{}).
			Where("file_name LIKE ?", "/%/c%").Count(&count).Error)
		assert.GreaterOrEqual(t, count, int64(205))
	})
}

func TestReportThumbSize(t *testing.T) {
	t.Run("PendingToFailureAndBackToSuccess", func(t *testing.T) {
		row := thumbStatusRow(t, "d", "fit_720", ThumbStatusPending)
		require.NoError(t, RegisterPendingThumbSizes([]ThumbStatus{row}))

		report := NewThumbStatus(row.FileRoot, row.FileName, row.SizeName, row.FileHash, ThumbStatusFailed)
		report.Error = "render failed"
		require.NoError(t, ReportThumbSize(*report))
		found := findThumbStatus(t, row)
		assert.Equal(t, ThumbStatusFailed, found.Status)
		assert.Equal(t, "render failed", found.Error)
		assert.Equal(t, 1, found.Attempts)

		// A second failure increments the attempt counter.
		report.Error = "still failing"
		require.NoError(t, ReportThumbSize(*report))
		found = findThumbStatus(t, row)
		assert.Equal(t, 2, found.Attempts)
		assert.Equal(t, "still failing", found.Error)

		// Success clears the error and counter.
		success := NewThumbStatus(row.FileRoot, row.FileName, row.SizeName, row.FileHash, ThumbStatusSucceeded)
		require.NoError(t, ReportThumbSize(*success))
		found = findThumbStatus(t, row)
		assert.Equal(t, ThumbStatusSucceeded, found.Status)
		assert.Empty(t, found.Error)
		assert.Equal(t, 0, found.Attempts)
	})

	t.Run("SucceededIsNotDowngradedWithSameHash", func(t *testing.T) {
		row := thumbStatusRow(t, "e", "fit_720", ThumbStatusSucceeded)
		require.NoError(t, UnscopedDb().Clauses(onConflictDoNothing()).Create(&row).Error)

		for _, status := range []string{ThumbStatusPending, ThumbStatusFailed, ThumbStatusSkipped} {
			stale := NewThumbStatus(row.FileRoot, row.FileName, row.SizeName, row.FileHash, status)
			stale.Error = "stale"
			if status == ThumbStatusPending {
				// ReportThumbSize rejects pending; emulate the registration path instead.
				require.NoError(t, RegisterPendingThumbSizes([]ThumbStatus{*stale}))
			} else {
				require.NoError(t, ReportThumbSize(*stale))
			}
		}

		found := findThumbStatus(t, row)
		assert.Equal(t, ThumbStatusSucceeded, found.Status)
		assert.Empty(t, found.Error)
		assert.Equal(t, 0, found.Attempts)
	})

	t.Run("ChangedHashTakesOverSucceededRow", func(t *testing.T) {
		row := thumbStatusRow(t, "f", "fit_720", ThumbStatusSucceeded)
		require.NoError(t, UnscopedDb().Clauses(onConflictDoNothing()).Create(&row).Error)

		failed := NewThumbStatus(row.FileRoot, row.FileName, row.SizeName, "hash-f-new", ThumbStatusFailed)
		failed.Error = "new render failed"
		require.NoError(t, ReportThumbSize(*failed))

		found := findThumbStatus(t, row)
		assert.Equal(t, ThumbStatusFailed, found.Status)
		assert.Equal(t, "hash-f-new", found.FileHash)
		assert.Equal(t, 1, found.Attempts)
	})

	t.Run("FailureOverridesSkippedButSkippedKeepsSucceeded", func(t *testing.T) {
		skipped := thumbStatusRow(t, "g", "tile_100", ThumbStatusSkipped)
		skipped.Error = "larger than source"
		require.NoError(t, UnscopedDb().Clauses(onConflictDoNothing()).Create(&skipped).Error)

		failed := NewThumbStatus(skipped.FileRoot, skipped.FileName, skipped.SizeName, skipped.FileHash, ThumbStatusFailed)
		failed.Error = "actual error"
		require.NoError(t, ReportThumbSize(*failed))
		assert.Equal(t, ThumbStatusFailed, findThumbStatus(t, skipped).Status)

		succeededRow := thumbStatusRow(t, "h", "tile_100", ThumbStatusSucceeded)
		require.NoError(t, UnscopedDb().Clauses(onConflictDoNothing()).Create(&succeededRow).Error)
		skip := NewThumbStatus(succeededRow.FileRoot, succeededRow.FileName, succeededRow.SizeName, succeededRow.FileHash, ThumbStatusSkipped)
		require.NoError(t, ReportThumbSize(*skip))
		assert.Equal(t, ThumbStatusSucceeded, findThumbStatus(t, succeededRow).Status)
	})

	t.Run("RejectsPendingAndInvalid", func(t *testing.T) {
		row := thumbStatusRow(t, "i", "fit_720", ThumbStatusPending)
		assert.Error(t, ReportThumbSize(row))

		bad := NewThumbStatus(row.FileRoot, row.FileName, row.SizeName, row.FileHash, "nope")
		assert.Error(t, ReportThumbSize(*bad))
	})
}

func TestThumbStatus_Delete(t *testing.T) {
	a1 := thumbStatusRow(t, "j", "fit_720", ThumbStatusPending)
	a2 := thumbStatusRow(t, "j", "tile_50", ThumbStatusFailed)
	b1 := thumbStatusRow(t, "k", "fit_720", ThumbStatusPending)
	require.NoError(t, RegisterPendingThumbSizes([]ThumbStatus{a1, a2, b1}))

	require.NoError(t, DeleteThumbStatuses(a1.FileRoot, a1.FileName))

	var count int64
	require.NoError(t, Db().Model(&ThumbStatus{}).
		Where("file_name = ?", a1.FileName).Count(&count).Error)
	assert.EqualValues(t, 0, count)

	assert.Equal(t, ThumbStatusPending, findThumbStatus(t, b1).Status)
}

func TestReportThumbSize_Concurrent(t *testing.T) {
	row := thumbStatusRow(t, "m", "fit_720", ThumbStatusPending)
	require.NoError(t, RegisterPendingThumbSizes([]ThumbStatus{row}))

	var wg sync.WaitGroup
	statuses := []string{ThumbStatusFailed, ThumbStatusSkipped, ThumbStatusSucceeded, ThumbStatusFailed, ThumbStatusSucceeded}

	for range 20 {
		for _, status := range statuses {
			wg.Add(1)

			go func(status string) {
				defer wg.Done()

				r := NewThumbStatus(row.FileRoot, row.FileName, row.SizeName, row.FileHash, status)
				r.Error = "concurrent"
				_ = ReportThumbSize(*r)
			}(status)
		}
	}

	wg.Wait()

	// At least one succeeded report ran, and succeeded can never be downgraded.
	found := findThumbStatus(t, row)
	assert.Equal(t, ThumbStatusSucceeded, found.Status)
	assert.Empty(t, found.Error)

	// Another size of the same file is written independently.
	other := thumbStatusRow(t, "m", "tile_50", ThumbStatusFailed)
	other.Error = "independent"
	require.NoError(t, ReportThumbSize(other))
	assert.Equal(t, ThumbStatusFailed, findThumbStatus(t, other).Status)
}
