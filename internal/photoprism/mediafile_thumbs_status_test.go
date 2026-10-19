package photoprism

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/thumb"
	"github.com/photoprism/photoprism/pkg/fs"
	"github.com/photoprism/photoprism/pkg/log/status"
)

// uncachedThumbNames returns the eager names excluded by the configured cache limit.
func uncachedThumbNames() []thumb.Name {
	var result []thumb.Name

	for _, name := range thumb.Names {
		if thumb.Sizes[name].Uncached() {
			result = append(result, name)
		}
	}

	return result
}

func TestGenerateThumbnails_StatusResults(t *testing.T) {
	cfg := newThumbsStatusTestConfig(t, "thumbstatushappy")

	srcName := copyFixtureToOriginals(t, cfg, "testdata/2018-04-12 19_24_49.jpg", "happy.jpg")

	m, err := NewMediaFile(srcName)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = entity.UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name = ?", m.RootRelName()).Error
	})

	cache := filepath.Join(t.TempDir(), "thumbs")

	require.NoError(t, m.GenerateThumbnails(cache, true))

	rows := thumbStatusRows(t, m.Root(), m.RootRelName())
	assert.Len(t, rows, len(eligibleThumbNames()))

	bySize := statusBySize(rows)

	for _, name := range eligibleThumbNames() {
		size := thumb.Sizes[name]
		row, ok := bySize[name.String()]
		require.True(t, ok, string(name))

		if thumb.Skip(size, m.Bounds()) {
			assert.Equal(t, entity.ThumbStatusSkipped, row.Status, string(name))
		} else {
			assert.Equal(t, entity.ThumbStatusSucceeded, row.Status, string(name))
		}
	}

	// Sizes outside the configured cache limit are not registered at all.
	all := statusBySize(rows)
	for _, name := range uncachedThumbNames() {
		_, ok := all[name.String()]
		assert.False(t, ok, string(name))
	}

	// At least one cache file exists and a second run stays successful without forcing.
	tile50, err := thumb.SizeTile50.FileName(m.Hash(), cache)
	require.NoError(t, err)
	assert.True(t, fs.FileExistsNotEmpty(tile50))
	assert.NoError(t, m.GenerateThumbnails(cache, false))
}

// blockTile50Target turns the tile_50 cache output path into a directory, so rendering just
// that one size fails while every other size can be generated normally.
func blockTile50Target(t *testing.T, m *MediaFile, cache string) string {
	t.Helper()

	target, err := thumb.SizeTile50.FileName(m.Hash(), cache)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(target, fs.ModeDir))

	return target
}

func TestGenerateThumbnails_PerSizeFailureIsolation(t *testing.T) {
	cfg := newThumbsStatusTestConfig(t, "thumbstatusfail")

	srcName := copyFixtureToOriginals(t, cfg, "testdata/2018-04-12 19_24_49.jpg", "partial.jpg")

	m, err := NewMediaFile(srcName)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = entity.UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name = ?", m.RootRelName()).Error
	})

	cache := filepath.Join(t.TempDir(), "thumbs")
	blocked := blockTile50Target(t, m, cache)
	_ = blocked

	// A per-size render error does not fail the file; other sizes still succeed.
	require.NoError(t, m.GenerateThumbnails(cache, false))

	bySize := statusBySize(thumbStatusRows(t, m.Root(), m.RootRelName()))

	failedRow := bySize[thumb.Tile50.String()]
	require.Equal(t, entity.ThumbStatusFailed, failedRow.Status)
	assert.NotEmpty(t, failedRow.Error)
	assert.Equal(t, 1, failedRow.Attempts)

	if skipped := thumb.Skip(thumb.Sizes[thumb.Tile100], m.Bounds()); !skipped {
		assert.Equal(t, entity.ThumbStatusSucceeded, bySize[thumb.Tile100.String()].Status)
	}

	assert.Equal(t, entity.ThumbStatusSucceeded, bySize[thumb.Colors.String()].Status)
}

func TestGenerateThumbnails_InsufficientStorageAbort(t *testing.T) {
	cfg := newThumbsStatusTestConfig(t, "thumbstatusnospace")

	srcName := copyFixtureToOriginals(t, cfg, "testdata/2018-04-12 19_24_49.jpg", "nospace.jpg")

	m, err := NewMediaFile(srcName)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = entity.UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name = ?", m.RootRelName()).Error
	})

	cache := filepath.Join(t.TempDir(), "thumbs")
	blockTile50Target(t, m, cache)

	// Treat the per-size write error as an out-of-space condition, like a full disk would surface.
	original := noSpaceError
	noSpaceError = func(err error) bool { return err != nil }
	t.Cleanup(func() { noSpaceError = original })

	genErr := m.GenerateThumbnails(cache, false)
	require.Error(t, genErr)
	assert.True(t, errors.Is(genErr, status.ErrInsufficientStorage))

	bySize := statusBySize(thumbStatusRows(t, m.Root(), m.RootRelName()))

	// The failing size is recorded, sizes before it succeeded, sizes after it stay pending.
	assert.Equal(t, entity.ThumbStatusFailed, bySize[thumb.Tile50.String()].Status)
	assert.Equal(t, entity.ThumbStatusPending, bySize[thumb.Colors.String()].Status)

	if skipped := thumb.Skip(thumb.Sizes[thumb.Tile100], m.Bounds()); !skipped {
		assert.Equal(t, entity.ThumbStatusSucceeded, bySize[thumb.Tile100.String()].Status)
	}
}

func TestGenerateAvatarThumbnails_NoStatusRows(t *testing.T) {
	cfg := newThumbsStatusTestConfig(t, "thumbstatusavatar")

	src := filepath.Join(t.TempDir(), "avatar-source.jpg")

	data, err := os.ReadFile("testdata/2018-04-12 19_24_49.jpg")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(src, data, fs.ModeFile))

	m, err := NewMediaFile(src)
	require.NoError(t, err)

	cache := filepath.Join(t.TempDir(), "thumbs")
	require.NoError(t, m.GenerateAvatarThumbnails(cache, true))

	var count int64
	require.NoError(t, entity.Db().Model(&entity.ThumbStatus{}).
		Where("file_hash = ?", m.Hash()).Count(&count).Error)
	assert.Equal(t, int64(0), count)
}
