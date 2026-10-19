package photoprism

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/entity/query"
	"github.com/photoprism/photoprism/internal/mutex"
	"github.com/photoprism/photoprism/internal/thumb"
)

// pendingRows builds pending status rows for a file and sizes.
func pendingRows(root, name, hash string, names ...thumb.Name) []entity.ThumbStatus {
	rows := make([]entity.ThumbStatus, 0, len(names))

	for _, n := range names {
		rows = append(rows, entity.ThumbStatus{
			FileRoot: root,
			FileName: name,
			SizeName: n.String(),
			FileHash: hash,
			Status:   entity.ThumbStatusPending,
		})
	}

	return rows
}

func TestThumbs_StartPending(t *testing.T) {
	cfg := newThumbsStatusTestConfig(t, "thumbpendingrun")

	src := copyFixtureToOriginals(t, cfg, "testdata/2018-04-12 19_24_49.jpg", "pending.jpg")

	mf, err := NewMediaFile(src)
	require.NoError(t, err)

	missingName := "/pending-missing.jpg"

	t.Cleanup(func() {
		_ = entity.UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name IN (?)",
			[]string{mf.RootRelName(), missingName}).Error
	})

	require.NoError(t, entity.RegisterPendingThumbSizes(pendingRows(
		mf.Root(), mf.RootRelName(), mf.Hash(), thumb.Tile50, thumb.Tile100)))
	require.NoError(t, entity.RegisterPendingThumbSizes(pendingRows(
		entity.RootOriginals, missingName, "deadbeef", thumb.Tile50)))

	w := NewThumbs(cfg)

	result, err := w.StartPending()
	require.NoError(t, err)
	assert.Equal(t, 1, result.Processed)
	assert.Equal(t, 1, result.Pruned)
	assert.Equal(t, 2, result.Repaired)
	assert.Equal(t, 0, result.Remaining)
	assert.Equal(t, 0, result.Failed)

	// The retried sizes reached a terminal state without a directory scan.
	bySize := statusBySize(thumbStatusRows(t, mf.Root(), mf.RootRelName()))
	assert.Equal(t, entity.ThumbStatusSucceeded, bySize[thumb.Tile50.String()].Status)
	assert.Equal(t, entity.ThumbStatusSucceeded, bySize[thumb.Tile100.String()].Status)

	// Rows whose original disappeared were pruned entirely.
	pruned, err := query.PendingThumbSizes(entity.RootOriginals, missingName)
	require.NoError(t, err)
	assert.Equal(t, 0, pruned)

	var prunedRows int64
	require.NoError(t, entity.Db().Model(&entity.ThumbStatus{}).
		Where("file_name = ?", missingName).Count(&prunedRows).Error)
	assert.Equal(t, int64(0), prunedRows)

	// A second run finds nothing to do.
	result, err = w.StartPending()
	require.NoError(t, err)
	assert.Equal(t, 0, result.Processed)
	assert.Equal(t, 0, result.Pruned)
}

func TestThumbs_StartPending_MutexGate(t *testing.T) {
	cfg := newThumbsStatusTestConfig(t, "thumbpendingmutex")
	w := NewThumbs(cfg)

	require.NoError(t, mutex.ThumbsPendingWorker.Start())
	mutex.ThumbsPendingWorker.Cancel()

	_, err := w.StartPending()
	assert.Error(t, err)

	mutex.ThumbsPendingWorker.Stop()

	result, err := w.StartPending()
	require.NoError(t, err)
	assert.Equal(t, 0, result.Processed)
}

func TestThumbs_StartPending_ConcurrentWithGeneration(t *testing.T) {
	cfg := newThumbsStatusTestConfig(t, "thumbpendingconc")

	src := copyFixtureToOriginals(t, cfg, "testdata/2018-04-12 19_24_49.jpg", "concurrent.jpg")

	mf, err := NewMediaFile(src)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = entity.UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name = ?", mf.RootRelName()).Error
	})

	require.NoError(t, entity.RegisterPendingThumbSizes(pendingRows(
		mf.Root(), mf.RootRelName(), mf.Hash(), thumb.Tile50, thumb.Tile100)))

	cache := cfg.ThumbCachePath()

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()
		_, _ = NewThumbs(cfg).StartPending()
	}()

	// A regular generation for the same file runs alongside the pending retry.
	for range 3 {
		assert.NoError(t, mf.GenerateThumbnails(cache, false))
	}

	wg.Wait()

	remaining, err := query.PendingThumbSizes(mf.Root(), mf.RootRelName())
	require.NoError(t, err)
	assert.Equal(t, 0, remaining)

	for _, row := range thumbStatusRows(t, mf.Root(), mf.RootRelName()) {
		assert.Contains(t, []string{entity.ThumbStatusSucceeded, entity.ThumbStatusSkipped}, row.Status,
			"%s must not be pending or failed", row.SizeName)
	}
}
