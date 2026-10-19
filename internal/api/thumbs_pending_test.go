package api

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/mutex"
)

// seedThumbStatusRow inserts one thumbnail status row for the pending-thumbs API tests.
func seedThumbStatusRow(t *testing.T, name, statusName string) {
	t.Helper()

	row := entity.NewThumbStatus(entity.RootOriginals, name, "fit_720", "api-hash-"+statusName, statusName)
	if statusName == entity.ThumbStatusFailed {
		row.Error = "render failed"
		row.Attempts = 2
	}

	require.NoError(t, entity.UnscopedDb().Create(row).Error)

	t.Cleanup(func() {
		_ = entity.UnscopedDb().Delete(&entity.ThumbStatus{}, "file_name = ?", name).Error
	})
}

func TestFindPendingThumbs(t *testing.T) {
	t.Run("Unauthenticated", func(t *testing.T) {
		app, router, _ := NewApiTest()
		FindPendingThumbs(router)
		r := PerformRequest(app, "GET", "/api/v1/thumbs/pending")
		assert.Equal(t, http.StatusUnauthorized, r.Code)
	})

	t.Run("Success", func(t *testing.T) {
		app, router, _ := NewApiTest()
		FindPendingThumbs(router)
		sess := AuthenticateAdmin(app, router)

		seedThumbStatusRow(t, "/api-thumbs-pending/failed.jpg", entity.ThumbStatusFailed)
		seedThumbStatusRow(t, "/api-thumbs-pending/skipped.jpg", entity.ThumbStatusSkipped)

		r := AuthenticatedRequest(app, "GET", "/api/v1/thumbs/pending?status=failed", sess)
		require.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "100", r.Header().Get("X-Limit"))

		results := gjson.Get(r.Body.String(), "results").Array()
		assert.NotEmpty(t, results)

		for _, n := range results {
			assert.Contains(t, n.Get("Name").String(), "api-thumbs-pending/failed.jpg")
		}

		assert.GreaterOrEqual(t, gjson.Get(r.Body.String(), "totals.skipped").Int(), int64(1))
	})

	t.Run("InvalidStatus", func(t *testing.T) {
		app, router, _ := NewApiTest()
		FindPendingThumbs(router)
		sess := AuthenticateAdmin(app, router)
		r := AuthenticatedRequest(app, "GET", "/api/v1/thumbs/pending?status=bogus", sess)
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})

	t.Run("InvalidOffset", func(t *testing.T) {
		app, router, _ := NewApiTest()
		FindPendingThumbs(router)
		sess := AuthenticateAdmin(app, router)
		r := AuthenticatedRequest(app, "GET", "/api/v1/thumbs/pending?offset=-3", sess)
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})

	t.Run("CountCapped", func(t *testing.T) {
		app, router, _ := NewApiTest()
		FindPendingThumbs(router)
		sess := AuthenticateAdmin(app, router)
		r := AuthenticatedRequest(app, "GET", "/api/v1/thumbs/pending?count=10000", sess)
		require.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "1000", r.Header().Get("X-Limit"))
	})
}

func TestStartPendingThumbs(t *testing.T) {
	t.Run("Unauthenticated", func(t *testing.T) {
		app, router, _ := NewApiTest()
		StartPendingThumbs(router)
		r := PerformRequest(app, "POST", "/api/v1/thumbs/pending")
		assert.Equal(t, http.StatusUnauthorized, r.Code)
	})

	t.Run("Busy", func(t *testing.T) {
		require.NoError(t, mutex.ThumbsPendingWorker.Start())
		t.Cleanup(mutex.ThumbsPendingWorker.Stop)

		app, router, _ := NewApiTest()
		StartPendingThumbs(router)
		sess := AuthenticateAdmin(app, router)
		r := AuthenticatedRequest(app, "POST", "/api/v1/thumbs/pending", sess)
		assert.Equal(t, http.StatusTooManyRequests, r.Code)
	})

	t.Run("Success", func(t *testing.T) {
		app, router, _ := NewApiTest()
		StartPendingThumbs(router)
		sess := AuthenticateAdmin(app, router)
		r := AuthenticatedRequest(app, "POST", "/api/v1/thumbs/pending", sess)
		require.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "ok", gjson.Get(r.Body.String(), "status").String())
	})
}
