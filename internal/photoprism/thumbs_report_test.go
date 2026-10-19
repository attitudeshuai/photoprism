package photoprism

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/config"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/thumb"
)

// thumbStatusRows reads all recorded size results for one file.
func thumbStatusRows(t *testing.T, root, name string) (rows entity.ThumbStatuses) {
	t.Helper()

	require.NoError(t, entity.Db().Where("file_root = ? AND file_name = ?", root, name).
		Order("size_name").Find(&rows).Error)

	return rows
}

// statusBySize indexes result rows by size name.
func statusBySize(rows entity.ThumbStatuses) map[string]entity.ThumbStatus {
	result := make(map[string]entity.ThumbStatus, len(rows))

	for _, row := range rows {
		result[row.SizeName] = row
	}

	return result
}

func TestDbThumbResultSink(t *testing.T) {
	cfg := config.NewMinimalTestConfigWithDb("thumbsink", t.TempDir())

	oldCfg := Config()
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(oldCfg) })

	const name = "/thumbs-sink-test/a.jpg"

	t.Cleanup(func() {
		_ = entity.UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name = ?", name).Error
	})

	sink := &dbThumbResultSink{root: entity.RootOriginals, fileName: name, hash: "hash-sink-a"}
	names := eligibleThumbNames()
	require.NotEmpty(t, names)

	// First registration creates pending rows.
	sink.Start(names)
	rows := thumbStatusRows(t, entity.RootOriginals, name)
	assert.Len(t, rows, len(names))

	for _, row := range rows {
		assert.Equal(t, entity.ThumbStatusPending, row.Status)
	}

	// A second start does not overwrite terminal or failed rows.
	require.NoError(t, entity.UnscopedDb().Model(&entity.ThumbStatus{}).
		Where("file_root = ? AND file_name = ? AND size_name = ?", entity.RootOriginals, name, thumb.Tile50.String()).
		Updates(entity.Values{"status": entity.ThumbStatusFailed, "error": "old", "attempts": 4}).Error)

	sink.Start(names)
	failed := statusBySize(thumbStatusRows(t, entity.RootOriginals, name))[thumb.Tile50.String()]
	assert.Equal(t, entity.ThumbStatusFailed, failed.Status)
	assert.Equal(t, 4, failed.Attempts)

	// Terminal outcomes land with their semantics.
	sink.Succeeded(thumb.Tile50)
	sink.Skipped(thumb.Fit4096, "larger than original")
	sink.Failed(thumb.Tile100, errors.New("boom"))

	bySize := statusBySize(thumbStatusRows(t, entity.RootOriginals, name))
	assert.Equal(t, entity.ThumbStatusSucceeded, bySize[thumb.Tile50.String()].Status)
	assert.Equal(t, 0, bySize[thumb.Tile50.String()].Attempts)
	assert.Equal(t, entity.ThumbStatusSkipped, bySize[thumb.Fit4096.String()].Status)
	assert.Equal(t, "larger than original", bySize[thumb.Fit4096.String()].Error)
	assert.Equal(t, entity.ThumbStatusFailed, bySize[thumb.Tile100.String()].Status)
	assert.Equal(t, "boom", bySize[thumb.Tile100.String()].Error)

	// The nop sink never panics and writes nothing on its own.
	nop := nopThumbResultSink{}
	nop.Start(names)
	nop.Succeeded(thumb.Tile50)
	nop.Skipped(thumb.Fit4096, "x")
	nop.Failed(thumb.Tile100, errors.New("x"))
}

func TestRegisterPendingThumbs(t *testing.T) {
	cfg := config.NewMinimalTestConfigWithDb("thumbsregister", t.TempDir())

	oldCfg := Config()
	SetConfig(cfg)
	t.Cleanup(func() { SetConfig(oldCfg) })

	srcName := copyFixtureToOriginals(t, cfg, "testdata/2018-04-12 19_24_49.jpg", "register.jpg")

	mf, err := NewMediaFile(srcName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = entity.UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name = ?", mf.RootRelName()).Error
	})

	registerPendingThumbs(mf)

	rows := thumbStatusRows(t, mf.Root(), mf.RootRelName())
	assert.Len(t, rows, len(eligibleThumbNames()))

	for _, row := range rows {
		assert.Equal(t, entity.ThumbStatusPending, row.Status)
		assert.Equal(t, mf.Hash(), row.FileHash)
	}
}
