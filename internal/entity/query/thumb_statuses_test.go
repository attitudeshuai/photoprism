package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/clause"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/form"
)

// onConflictClause keeps existing rows when inserting test fixtures.
func onConflictClause() clause.OnConflict {
	return clause.OnConflict{DoNothing: true}
}

// pendingTestRow creates a status row with a test-unique file name.
func pendingTestRow(t *testing.T, file, sizeName, status string) entity.ThumbStatus {
	t.Helper()

	name := "/pending-query-test/" + file

	t.Cleanup(func() {
		_ = UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name = ?", name).Error
	})

	return *entity.NewThumbStatus(entity.RootOriginals, name, sizeName, "hash-"+file, status)
}

// createPendingTestRows inserts the given rows and fails the test on error.
func createPendingTestRows(t *testing.T, rows ...entity.ThumbStatus) {
	t.Helper()

	require.NoError(t, UnscopedDb().Clauses(onConflictClause()).Create(&rows).Error)
}

func TestThumbStatuses(t *testing.T) {
	createPendingTestRows(t,
		pendingTestRow(t, "a.jpg", "fit_720", entity.ThumbStatusFailed),
		pendingTestRow(t, "a.jpg", "tile_50", entity.ThumbStatusSucceeded),
		pendingTestRow(t, "b.jpg", "fit_1920", entity.ThumbStatusPending),
		pendingTestRow(t, "c.jpg", "fit_4096", entity.ThumbStatusSkipped),
	)

	t.Run("DefaultSelectsActionableStates", func(t *testing.T) {
		f := form.ThumbStatusSearch{}
		require.NoError(t, f.Normalize())

		result, err := ThumbStatuses(f)
		require.NoError(t, err)

		statuses := map[string]bool{}
		for _, row := range result {
			statuses[row.Status] = true
		}

		assert.True(t, statuses[entity.ThumbStatusFailed])
		assert.True(t, statuses[entity.ThumbStatusPending])
		assert.False(t, statuses[entity.ThumbStatusSucceeded])
		assert.False(t, statuses[entity.ThumbStatusSkipped])
	})

	t.Run("StatusFilter", func(t *testing.T) {
		f := form.ThumbStatusSearch{Status: entity.ThumbStatusSkipped, Count: 100}
		result, err := ThumbStatuses(f)
		require.NoError(t, err)
		assert.NotEmpty(t, result)

		for _, row := range result {
			assert.Equal(t, entity.ThumbStatusSkipped, row.Status)
		}
	})

	t.Run("PathFilterAndPagination", func(t *testing.T) {
		f := form.ThumbStatusSearch{Path: "pending-query-test/a.jpg", Count: 1}
		require.NoError(t, f.Normalize())

		pageOne, err := ThumbStatuses(f)
		require.NoError(t, err)
		assert.Len(t, pageOne, 1)

		f.Offset = 1
		pageTwo, err := ThumbStatuses(f)
		require.NoError(t, err)
		assert.Len(t, pageTwo, 1)
		assert.NotEqual(t, pageOne[0].SizeName, pageTwo[0].SizeName)
	})
}

func TestPendingThumbFiles(t *testing.T) {
	createPendingTestRows(t,
		pendingTestRow(t, "group-a.jpg", "fit_720", entity.ThumbStatusFailed),
		pendingTestRow(t, "group-a.jpg", "tile_50", entity.ThumbStatusPending),
		pendingTestRow(t, "group-a.jpg", "tile_100", entity.ThumbStatusSucceeded),
		pendingTestRow(t, "group-b.jpg", "fit_1920", entity.ThumbStatusPending),
		pendingTestRow(t, "group-c.jpg", "fit_4096", entity.ThumbStatusSkipped),
		pendingTestRow(t, "group-c.jpg", "fit_2560", entity.ThumbStatusSucceeded),
	)

	files, err := PendingThumbFiles(50)
	require.NoError(t, err)

	var groupA, groupB, groupC *PendingThumbFile

	for i := range files {
		switch files[i].FileName {
		case "/pending-query-test/group-a.jpg":
			groupA = &files[i]
		case "/pending-query-test/group-b.jpg":
			groupB = &files[i]
		case "/pending-query-test/group-c.jpg":
			groupC = &files[i]
		}
	}

	require.NotNil(t, groupA, "group-a must be pending")
	assert.Equal(t, "hash-group-a.jpg", groupA.FileHash)
	assert.Len(t, groupA.Sizes, 2)

	for _, row := range groupA.Sizes {
		assert.Contains(t, []string{entity.ThumbStatusFailed, entity.ThumbStatusPending}, row.Status)
	}

	// Sizes within a group are ordered by name.
	assert.Equal(t, "fit_720", groupA.Sizes[0].SizeName)
	assert.Equal(t, "tile_50", groupA.Sizes[1].SizeName)

	require.NotNil(t, groupB, "group-b must be pending")
	assert.Len(t, groupB.Sizes, 1)

	assert.Nil(t, groupC, "files with only skipped/succeeded rows are not actionable")
}

func TestThumbStatusCounts(t *testing.T) {
	createPendingTestRows(t,
		pendingTestRow(t, "count-a.jpg", "fit_720", entity.ThumbStatusPending),
		pendingTestRow(t, "count-b.jpg", "fit_720", entity.ThumbStatusFailed),
		pendingTestRow(t, "count-c.jpg", "fit_720", entity.ThumbStatusSkipped),
		pendingTestRow(t, "count-d.jpg", "fit_720", entity.ThumbStatusSucceeded),
	)

	counts, err := ThumbStatusCounts()
	require.NoError(t, err)

	for _, status := range []string{
		entity.ThumbStatusPending,
		entity.ThumbStatusFailed,
		entity.ThumbStatusSkipped,
		entity.ThumbStatusSucceeded,
	} {
		count, ok := counts[status]
		assert.True(t, ok, status)
		assert.GreaterOrEqual(t, count, int64(1), status)
	}
}
