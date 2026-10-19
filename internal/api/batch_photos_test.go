package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/photoprism/photoprism/internal/batchresult"
	"github.com/photoprism/photoprism/internal/config"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/pkg/fs"
	"github.com/photoprism/photoprism/pkg/i18n"
	"github.com/photoprism/photoprism/pkg/rnd"
)

// TestBatchPhotosArchive checks selection archiving and request validation.
func TestBatchPhotosArchive(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		uids := []string{"ps6sg6be2lvl0yh7", "ps6sg6be2lvl0ycc"}
		var photos []entity.Photo
		require.NoError(t, entity.UnscopedDb().Where("photo_uid IN (?)", uids).Find(&photos).Error)
		require.NotEmpty(t, photos)
		var links []entity.PhotoAlbum
		require.NoError(t, entity.UnscopedDb().Where("photo_uid IN (?)", uids).Find(&links).Error)
		require.NotEmpty(t, links)
		t.Cleanup(func() {
			for _, photo := range photos {
				assert.NoError(t, entity.UnscopedDb().Model(&entity.Photo{}).Where("id = ?", photo.ID).
					UpdateColumns(entity.Values{"deleted_at": photo.DeletedAt, "photo_quality": photo.PhotoQuality,
						"updated_at": photo.UpdatedAt}).Error)
			}
			for _, link := range links {
				assert.NoError(t, entity.UnscopedDb().Model(&entity.PhotoAlbum{}).
					Where("photo_uid = ? AND album_uid = ?", link.PhotoUID, link.AlbumUID).
					UpdateColumn("hidden", link.Hidden).Error)
			}
		})
		require.NoError(t, entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid IN (?)", uids).
			UpdateColumns(entity.Values{"deleted_at": nil, "photo_quality": 3}).Error)
		require.NoError(t, entity.UnscopedDb().Model(&entity.PhotoAlbum{}).Where("photo_uid IN (?)", uids).
			UpdateColumn("hidden", false).Error)
		app, router, _ := NewApiTest()
		GetPhoto(router)
		r := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0yh7")
		assert.Equal(t, http.StatusOK, r.Code)
		val := gjson.Get(r.Body.String(), "DeletedAt")
		assert.Empty(t, val.String())

		BatchPhotosArchive(router)
		r2 := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive", `{"photos": ["ps6sg6be2lvl0yh7", "ps6sg6be2lvl0ycc"]}`)
		val2 := gjson.Get(r2.Body.String(), "message")
		assert.Contains(t, val2.String(), "Selection archived")
		assert.Equal(t, http.StatusOK, r2.Code)
		assert.Equal(t, "success", gjson.Get(r2.Body.String(), "status").String())
		assert.Equal(t, int64(2), gjson.Get(r2.Body.String(), "successCount").Int())
		assert.Equal(t, int64(0), gjson.Get(r2.Body.String(), "failCount").Int())
		assert.Len(t, gjson.Get(r2.Body.String(), "items").Array(), 2)
		for _, item := range gjson.Get(r2.Body.String(), "items").Array() {
			assert.True(t, item.Get("ok").Bool(), item.Get("id").String())
			assert.Empty(t, item.Get("error").String())
		}
		for _, link := range links {
			var archived entity.PhotoAlbum
			require.NoError(t, entity.Db().Where("photo_uid = ? AND album_uid = ?", link.PhotoUID, link.AlbumUID).
				First(&archived).Error)
			assert.True(t, archived.Hidden)
		}

		r3 := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0yh7")
		assert.Equal(t, http.StatusOK, r3.Code)
		val3 := gjson.Get(r3.Body.String(), "DeletedAt")
		assert.NotEmpty(t, val3.String())
	})
	t.Run("MissingSelection", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosArchive(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive", `{"photos": []}`)
		val := gjson.Get(r.Body.String(), "error")
		assert.Equal(t, i18n.Msg(i18n.ErrNoItemsSelected), val.String())
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("InvalidRequest", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosArchive(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive", `{"photos": 123}`)
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("RequestTooLarge", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosArchive(router)

		body := `{"photos":["` + strings.Repeat("p", int(MaxSelectionRequestBytes)) + `"]}`
		r := PerformRequestWithBody(app, http.MethodPost, "/api/v1/batch/photos/archive", body)

		assert.Equal(t, http.StatusRequestEntityTooLarge, r.Code)
	})
	t.Run("PartialUnknownUID", func(t *testing.T) {
		uid := "ps6sg6be2lvl0yh8"

		require.NoError(t, entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
			UpdateColumns(entity.Values{"deleted_at": nil, "photo_quality": 3}).Error)
		t.Cleanup(func() {
			_ = entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
				UpdateColumn("deleted_at", nil).Error
		})

		app, router, _ := NewApiTest()
		BatchPhotosArchive(router)
		// Missing UID first; the existing photo must still be processed in continue mode.
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive", `{"photos": ["ps6sg6be2lvl0missing", "ps6sg6be2lvl0yh8"]}`)
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "Selection archived", gjson.Get(r.Body.String(), "message").String())
		assert.Equal(t, "partial", gjson.Get(r.Body.String(), "status").String())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "successCount").Int())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "failCount").Int())
		items := gjson.Get(r.Body.String(), "items").Array()
		require.Len(t, items, 2)
		assert.Equal(t, "ps6sg6be2lvl0missing", items[0].Get("id").String())
		assert.False(t, items[0].Get("ok").Bool())
		assert.NotEmpty(t, items[0].Get("error").String())
		assert.Equal(t, uid, items[1].Get("id").String())
		assert.True(t, items[1].Get("ok").Bool())

		var state entity.Photo
		require.NoError(t, entity.UnscopedDb().Where("photo_uid = ?", uid).First(&state).Error)
		assert.True(t, state.DeletedAt.Valid)
	})
	t.Run("StopOnError", func(t *testing.T) {
		uid := "ps6sg6be2lvl0yh8"

		require.NoError(t, entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
			UpdateColumns(entity.Values{"deleted_at": nil, "photo_quality": 3}).Error)
		t.Cleanup(func() {
			_ = entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
				UpdateColumn("deleted_at", nil).Error
		})

		app, router, _ := NewApiTest()
		BatchPhotosArchive(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive", `{"photos": ["ps6sg6be2lvl0missing", "ps6sg6be2lvl0yh8"], "stopOnError": true}`)
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "failed", gjson.Get(r.Body.String(), "status").String())
		assert.True(t, gjson.Get(r.Body.String(), "stopped").Bool())
		assert.Equal(t, int64(0), gjson.Get(r.Body.String(), "successCount").Int())
		items := gjson.Get(r.Body.String(), "items").Array()
		require.Len(t, items, 2)
		assert.False(t, items[0].Get("ok").Bool())
		assert.Contains(t, items[1].Get("error").String(), "stopped")

		// The second photo must not have been archived.
		var state entity.Photo
		require.NoError(t, entity.UnscopedDb().Where("photo_uid = ?", uid).First(&state).Error)
		assert.False(t, state.DeletedAt.Valid)
	})
}

func TestBatchPhotosRestore(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		app, router, _ := NewApiTest()

		// Register routes.
		BatchPhotosArchive(router)
		GetPhoto(router)
		BatchPhotosRestore(router)

		r2 := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive", `{"photos": ["ps6sg6be2lvl0yh8", "ps6sg6be2lvl0ycc"]}`)
		val2 := gjson.Get(r2.Body.String(), "message")
		assert.Contains(t, val2.String(), "Selection archived")
		assert.Equal(t, http.StatusOK, r2.Code)

		r3 := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0yh8")
		assert.Equal(t, http.StatusOK, r3.Code)
		val3 := gjson.Get(r3.Body.String(), "DeletedAt")
		assert.NotEmpty(t, val3.String())

		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/restore", `{"photos": ["ps6sg6be2lvl0yh8", "ps6sg6be2lvl0ycc"]}`)
		val := gjson.Get(r.Body.String(), "message")
		assert.Contains(t, val.String(), "Selection restored")
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "success", gjson.Get(r.Body.String(), "status").String())
		assert.Equal(t, int64(2), gjson.Get(r.Body.String(), "successCount").Int())
		assert.Len(t, gjson.Get(r.Body.String(), "items").Array(), 2)

		r4 := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0yh8")
		assert.Equal(t, http.StatusOK, r4.Code)
		val4 := gjson.Get(r4.Body.String(), "DeletedAt")
		assert.Empty(t, val4.String())
	})
	t.Run("MissingSelection", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosRestore(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/restore", `{"photos": []}`)
		val := gjson.Get(r.Body.String(), "error")
		assert.Equal(t, i18n.Msg(i18n.ErrNoItemsSelected), val.String())
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("InvalidRequest", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosRestore(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/restore", `{"photos": 123}`)
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("PartialUnknownUID", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosArchive(router)
		BatchPhotosRestore(router)

		r0 := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive", `{"photos": ["ps6sg6be2lvl0yh8"]}`)
		require.Equal(t, http.StatusOK, r0.Code)

		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/restore", `{"photos": ["ps6sg6be2lvl0missing", "ps6sg6be2lvl0yh8"]}`)
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "partial", gjson.Get(r.Body.String(), "status").String())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "successCount").Int())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "failCount").Int())
		items := gjson.Get(r.Body.String(), "items").Array()
		require.Len(t, items, 2)
		assert.False(t, items[0].Get("ok").Bool())
		assert.True(t, items[1].Get("ok").Bool())
	})
	t.Run("StopOnError", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosArchive(router)
		BatchPhotosRestore(router)

		r0 := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive", `{"photos": ["ps6sg6be2lvl0yh8"]}`)
		require.Equal(t, http.StatusOK, r0.Code)
		t.Cleanup(func() {
			_ = entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", "ps6sg6be2lvl0yh8").
				UpdateColumn("deleted_at", nil).Error
		})

		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/restore", `{"photos": ["ps6sg6be2lvl0missing", "ps6sg6be2lvl0yh8"], "stopOnError": true}`)
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "failed", gjson.Get(r.Body.String(), "status").String())
		assert.True(t, gjson.Get(r.Body.String(), "stopped").Bool())

		// The second photo must stay archived.
		var state entity.Photo
		require.NoError(t, entity.UnscopedDb().Where("photo_uid = ?", "ps6sg6be2lvl0yh8").First(&state).Error)
		assert.True(t, state.DeletedAt.Valid)
	})
}

func TestBatchAlbumsDelete(t *testing.T) {
	app, router, _ := NewApiTest()
	CreateAlbum(router)
	r := PerformRequestWithBody(app, "POST", "/api/v1/albums", `{"Title": "BatchDelete", "Description": "To be deleted", "Notes": "", "Favorite": true}`)
	assert.Equal(t, http.StatusCreated, r.Code)
	uid := gjson.Get(r.Body.String(), "UID").String()

	t.Run("Success", func(t *testing.T) {
		app, router, _ := NewApiTest()

		// Register routes.
		GetAlbum(router)
		BatchAlbumsDelete(router)

		r := PerformRequest(app, "GET", "/api/v1/albums/"+uid)
		val := gjson.Get(r.Body.String(), "Slug")
		assert.Equal(t, "batchdelete", val.String())

		r2 := PerformRequestWithBody(app, "POST", "/api/v1/batch/albums/delete", fmt.Sprintf(`{"albums": ["%s", "ps6sg6be2lvl0ycc"]}`, uid))
		val2 := gjson.Get(r2.Body.String(), "message")
		assert.Contains(t, val2.String(), i18n.Msg(i18n.MsgAlbumsDeleted))
		assert.Equal(t, http.StatusOK, r2.Code)
		assert.Equal(t, "partial", gjson.Get(r2.Body.String(), "status").String())
		assert.Equal(t, int64(1), gjson.Get(r2.Body.String(), "successCount").Int())
		assert.Equal(t, int64(1), gjson.Get(r2.Body.String(), "failCount").Int())
		items := gjson.Get(r2.Body.String(), "items").Array()
		require.Len(t, items, 2)
		assert.Equal(t, uid, items[0].Get("id").String())
		assert.True(t, items[0].Get("ok").Bool())
		assert.False(t, items[1].Get("ok").Bool())
		assert.NotEmpty(t, items[1].Get("error").String())

		r3 := PerformRequest(app, "GET", "/api/v1/albums/"+uid)
		val3 := gjson.Get(r3.Body.String(), "error")
		assert.Equal(t, i18n.Msg(i18n.ErrAlbumNotFound), val3.String())
		assert.Equal(t, http.StatusNotFound, r3.Code)
	})
	t.Run("NoAlbumsSelected", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchAlbumsDelete(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/albums/delete", `{"albums": []}`)
		val := gjson.Get(r.Body.String(), "error")
		assert.Equal(t, i18n.Msg(i18n.ErrNoAlbumsSelected), val.String())
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("InvalidRequest", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchAlbumsDelete(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/albums/delete", `{"albums": 123}`)
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("RequestTooLarge", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchAlbumsDelete(router)

		body := `{"albums":["` + strings.Repeat("a", int(MaxSelectionRequestBytes)) + `"]}`
		r := PerformRequestWithBody(app, http.MethodPost, "/api/v1/batch/albums/delete", body)

		assert.Equal(t, http.StatusRequestEntityTooLarge, r.Code)
	})
}

func TestBatchPhotosPrivate(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		app, router, _ := NewApiTest()

		// Register routes.
		GetPhoto(router)
		BatchPhotosPrivate(router)

		r := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0yh8")
		assert.Equal(t, http.StatusOK, r.Code)
		val := gjson.Get(r.Body.String(), "Private")
		assert.Equal(t, "false", val.String())

		r2 := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/private", `{"photos": ["ps6sg6be2lvl0yh8", "ps6sg6be2lvl0ycc"]}`)
		val2 := gjson.Get(r2.Body.String(), "message")
		assert.Contains(t, val2.String(), "Selection marked as private")
		assert.Equal(t, http.StatusOK, r2.Code)
		assert.Equal(t, "success", gjson.Get(r2.Body.String(), "status").String())
		assert.Equal(t, int64(2), gjson.Get(r2.Body.String(), "successCount").Int())
		assert.Len(t, gjson.Get(r2.Body.String(), "items").Array(), 2)

		r3 := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0yh8")
		assert.Equal(t, http.StatusOK, r3.Code)
		val3 := gjson.Get(r3.Body.String(), "Private")
		assert.Equal(t, "true", val3.String())

		// Toggling again makes the pictures public, which also restores the fixtures.
		r4 := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/private", `{"photos": ["ps6sg6be2lvl0yh8", "ps6sg6be2lvl0ycc"]}`)
		assert.Equal(t, http.StatusOK, r4.Code)

		r5 := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0yh8")
		assert.Equal(t, http.StatusOK, r5.Code)
		assert.Equal(t, "false", gjson.Get(r5.Body.String(), "Private").String())
	})
	t.Run("MissingSelection", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosPrivate(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/private", `{"photos": []}`)
		val := gjson.Get(r.Body.String(), "error")
		assert.Equal(t, i18n.Msg(i18n.ErrNoItemsSelected), val.String())
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("InvalidRequest", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosPrivate(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/private", `{"photos": 123}`)
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("PartialUnknownUID", func(t *testing.T) {
		uid := "ps6sg6be2lvl0yh8"

		require.NoError(t, entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
			UpdateColumn("photo_private", false).Error)
		t.Cleanup(func() {
			_ = entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
				UpdateColumn("photo_private", false).Error
		})

		app, router, _ := NewApiTest()
		BatchPhotosPrivate(router)
		// Missing UID first; the existing photo must still be toggled in continue mode.
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/private", `{"photos": ["ps6sg6be2lvl0missing", "ps6sg6be2lvl0yh8"]}`)
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "partial", gjson.Get(r.Body.String(), "status").String())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "successCount").Int())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "failCount").Int())
		items := gjson.Get(r.Body.String(), "items").Array()
		require.Len(t, items, 2)
		assert.False(t, items[0].Get("ok").Bool())
		assert.True(t, items[1].Get("ok").Bool())

		var state entity.Photo
		require.NoError(t, entity.UnscopedDb().Where("photo_uid = ?", uid).First(&state).Error)
		assert.True(t, state.PhotoPrivate)
	})
	t.Run("StopOnError", func(t *testing.T) {
		uid := "ps6sg6be2lvl0yh8"

		require.NoError(t, entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
			UpdateColumn("photo_private", false).Error)
		t.Cleanup(func() {
			_ = entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
				UpdateColumn("photo_private", false).Error
		})

		app, router, _ := NewApiTest()
		BatchPhotosPrivate(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/private", `{"photos": ["ps6sg6be2lvl0missing", "ps6sg6be2lvl0yh8"], "stopOnError": true}`)
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "failed", gjson.Get(r.Body.String(), "status").String())
		assert.True(t, gjson.Get(r.Body.String(), "stopped").Bool())
		assert.Contains(t, gjson.Get(r.Body.String(), "items.1.error").String(), "stopped")

		// The second photo must not have been toggled.
		var state entity.Photo
		require.NoError(t, entity.UnscopedDb().Where("photo_uid = ?", uid).First(&state).Error)
		assert.False(t, state.PhotoPrivate)
	})
}

func TestBatchLabelsDelete(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		app, router, _ := NewApiTest()

		// Register routes.
		SearchLabels(router)
		BatchLabelsDelete(router)

		r := PerformRequest(app, "GET", "/api/v1/labels?count=15")
		val := gjson.Get(r.Body.String(), `#(Name=="Batch Delete").Slug`)
		assert.Equal(t, val.String(), "batch-delete")

		r2 := PerformRequestWithBody(app, "POST", "/api/v1/batch/labels/delete", `{"labels": ["ls6sg6b1wowuy3c6", "ps6sg6be2lvl0ycc"]}`)

		var resp i18n.Response

		if err := json.Unmarshal(r2.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}

		assert.True(t, resp.Success())
		assert.Equal(t, i18n.Msg(i18n.MsgLabelsDeleted), resp.Message)
		assert.Equal(t, i18n.Msg(i18n.MsgLabelsDeleted), resp.String())
		assert.Equal(t, http.StatusOK, r2.Code)
		assert.Equal(t, http.StatusOK, resp.Code)
		assert.Equal(t, "partial", gjson.Get(r2.Body.String(), "status").String())
		assert.Equal(t, int64(1), gjson.Get(r2.Body.String(), "successCount").Int())
		assert.Equal(t, int64(1), gjson.Get(r2.Body.String(), "failCount").Int())
		items := gjson.Get(r2.Body.String(), "items").Array()
		require.Len(t, items, 2)
		assert.Equal(t, "ls6sg6b1wowuy3c6", items[0].Get("id").String())
		assert.True(t, items[0].Get("ok").Bool())
		assert.False(t, items[1].Get("ok").Bool())
		assert.NotEmpty(t, items[1].Get("error").String())

		r3 := PerformRequest(app, "GET", "/api/v1/labels?count=15")
		val3 := gjson.Get(r3.Body.String(), `#(Name=="BatchDelete").Slug`)
		assert.Equal(t, val3.String(), "")
	})
	t.Run("NoLabelsSelected", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchLabelsDelete(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/labels/delete", `{"labels": []}`)
		val := gjson.Get(r.Body.String(), "error")
		assert.Equal(t, i18n.Msg(i18n.ErrNoLabelsSelected), val.String())
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("InvalidRequest", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchLabelsDelete(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/labels/delete", `{"labels": 123}`)
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("RequestTooLarge", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchLabelsDelete(router)

		body := `{"labels":["` + strings.Repeat("l", int(MaxSelectionRequestBytes)) + `"]}`
		r := PerformRequestWithBody(app, http.MethodPost, "/api/v1/batch/labels/delete", body)

		assert.Equal(t, http.StatusRequestEntityTooLarge, r.Code)
	})
}

func TestBatchPhotosApprove(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		app, router, _ := NewApiTest()

		// Register routes.
		GetPhoto(router)
		BatchPhotosApprove(router)

		r := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0y50")
		assert.Equal(t, http.StatusOK, r.Code)
		val := gjson.Get(r.Body.String(), "Quality")
		assert.Equal(t, "1", val.String())
		val4 := gjson.Get(r.Body.String(), "EditedAt")
		assert.Empty(t, val4.String())

		r2 := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/approve", `{"photos": ["ps6sg6be2lvl0y50", "ps6sg6be2lvl0y90"]}`)
		val2 := gjson.Get(r2.Body.String(), "message")
		assert.Contains(t, val2.String(), "Selection approved")
		assert.Equal(t, http.StatusOK, r2.Code)
		assert.Equal(t, "success", gjson.Get(r2.Body.String(), "status").String())
		assert.Equal(t, int64(2), gjson.Get(r2.Body.String(), "successCount").Int())
		assert.Len(t, gjson.Get(r2.Body.String(), "items").Array(), 2)

		r3 := PerformRequest(app, "GET", "/api/v1/photos/ps6sg6be2lvl0y50")
		assert.Equal(t, http.StatusOK, r3.Code)
		val5 := gjson.Get(r3.Body.String(), "Quality")
		assert.Equal(t, "7", val5.String())
		val6 := gjson.Get(r3.Body.String(), "EditedAt")
		assert.NotEmpty(t, val6.String())
	})
	t.Run("MissingSelection", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosApprove(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/approve", `{"photos": []}`)
		val := gjson.Get(r.Body.String(), "error")
		assert.Equal(t, i18n.Msg(i18n.ErrNoItemsSelected), val.String())
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("InvalidRequest", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosApprove(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/approve", `{"photos": 123}`)
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("PartialUnknownUID", func(t *testing.T) {
		// ps6sg6be2lvl0y50 was approved by the Success test, so approving again is a successful no-op.
		app, router, _ := NewApiTest()
		BatchPhotosApprove(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/approve", `{"photos": ["ps6sg6be2lvl0missing", "ps6sg6be2lvl0y50"]}`)
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, "partial", gjson.Get(r.Body.String(), "status").String())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "successCount").Int())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "failCount").Int())
		items := gjson.Get(r.Body.String(), "items").Array()
		require.Len(t, items, 2)
		assert.False(t, items[0].Get("ok").Bool())
		assert.NotEmpty(t, items[0].Get("error").String())
		assert.True(t, items[1].Get("ok").Bool())
	})
}

// batchDeleteTestFolder returns a new originals folder for batch delete tests and removes it,
// along with the rows of the photos created in it, when the test ends.
func batchDeleteTestFolder(t *testing.T, conf *config.Config) string {
	t.Helper()

	folder := "zz-batch-delete-" + rnd.Base36(8)
	dir := filepath.Join(conf.OriginalsPath(), folder)
	require.NoError(t, fs.MkdirAll(dir))

	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
		db := entity.UnscopedDb()
		ids := db.Table(entity.Photo{}.TableName()).Select("id").Where("photo_path = ?", folder).Session(&gorm.Session{})
		_ = db.Where("photo_id IN (?)", ids).Delete(&entity.PhotoLabel{}).Error
		_ = db.Where("photo_id IN (?)", ids).Delete(&entity.Details{}).Error
		_ = db.Where("file_name LIKE ?", folder+"/%").Delete(&entity.File{}).Error
		_ = db.Where("photo_path = ?", folder).Delete(&entity.Photo{}).Error
	})

	return folder
}

// batchDeleteTestPhoto creates a photo with an original file in folder and returns it with the file's
// absolute path, so batch delete tests do not permanently remove shared fixtures.
func batchDeleteTestPhoto(t *testing.T, conf *config.Config, folder, name string) (*entity.Photo, string) {
	t.Helper()

	photo := &entity.Photo{
		PhotoPath:    folder,
		PhotoName:    name,
		PhotoType:    entity.MediaImage,
		PhotoQuality: 3,
	}

	require.NoError(t, photo.Create())

	fileName := filepath.Join(conf.OriginalsPath(), folder, name+".jpg")
	require.NoError(t, fs.Copy("./testdata/london_160x160.jpg", fileName, true))

	file := &entity.File{
		PhotoID:     photo.ID,
		PhotoUID:    photo.PhotoUID,
		FileName:    folder + "/" + name + ".jpg",
		FileRoot:    entity.RootOriginals,
		FileHash:    rnd.GenerateUID(entity.FileUID),
		FileType:    fs.ImageJpeg.String(),
		FileMime:    "image/jpeg",
		FilePrimary: true,
	}

	require.NoError(t, file.Create())

	return photo, fileName
}

// batchDeleteTestPhotoExists reports whether the photo is still indexed.
func batchDeleteTestPhotoExists(t *testing.T, photo *entity.Photo) bool {
	t.Helper()

	var count int64
	require.NoError(t, entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", photo.PhotoUID).Count(&count).Error)

	return count > 0
}

// assertBatchDeleteTestPhotoKept asserts that the photo, its file, and its original were left unchanged.
func assertBatchDeleteTestPhotoKept(t *testing.T, photo *entity.Photo, fileName string) {
	t.Helper()

	var result entity.Photo

	if !assert.NoError(t, entity.UnscopedDb().First(&result, "photo_uid = ?", photo.PhotoUID).Error, photo.PhotoName) {
		return
	}

	assert.Equal(t, !photo.DeletedAt.Valid, !result.DeletedAt.Valid, photo.PhotoName)
	assert.Equal(t, photo.PhotoQuality, result.PhotoQuality, photo.PhotoName)

	var files int64
	require.NoError(t, entity.UnscopedDb().Model(&entity.File{}).Where("photo_id = ? AND deleted_at IS NULL", result.ID).Count(&files).Error)
	assert.EqualValues(t, 1, files, photo.PhotoName)
	assert.FileExists(t, fileName)
}

func TestBatchPhotosDelete(t *testing.T) {
	t.Run("ErrNoItemsSelected", func(t *testing.T) {
		app, router, _ := NewApiTest()
		BatchPhotosDelete(router)
		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/delete", `{"photos": []}`)
		val := gjson.Get(r.Body.String(), "error")
		assert.Equal(t, i18n.Msg(i18n.ErrNoItemsSelected), val.String())
		assert.Equal(t, http.StatusBadRequest, r.Code)
	})
	t.Run("NoneArchived", func(t *testing.T) {
		app, router, conf := NewApiTest()
		BatchPhotosDelete(router)
		folder := batchDeleteTestFolder(t, conf)
		photo1, file1 := batchDeleteTestPhoto(t, conf, folder, "visible1")
		photo2, file2 := batchDeleteTestPhoto(t, conf, folder, "visible2")

		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/delete", fmt.Sprintf(`{"photos": [%q, %q]}`, photo1.PhotoUID, photo2.PhotoUID))
		assert.Equal(t, http.StatusBadRequest, r.Code)
		assert.Equal(t, i18n.Msg(i18n.ErrNoItemsSelected), gjson.Get(r.Body.String(), "error").String())
		assert.Equal(t, "failed", gjson.Get(r.Body.String(), "status").String())
		assert.Equal(t, int64(0), gjson.Get(r.Body.String(), "successCount").Int())
		assert.Equal(t, int64(2), gjson.Get(r.Body.String(), "failCount").Int())

		assertBatchDeleteTestPhotoKept(t, photo1, file1)
		assertBatchDeleteTestPhotoKept(t, photo2, file2)
	})
	t.Run("Mixed", func(t *testing.T) {
		app, router, conf := NewApiTest()
		BatchPhotosDelete(router)
		folder := batchDeleteTestFolder(t, conf)
		archived, archivedFile := batchDeleteTestPhoto(t, conf, folder, "archived")
		visible, visibleFile := batchDeleteTestPhoto(t, conf, folder, "visible")
		require.NoError(t, archived.Archive())

		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/delete", fmt.Sprintf(`{"photos": [%q, %q]}`, visible.PhotoUID, archived.PhotoUID))
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, i18n.Msg(i18n.MsgPermanentlyDeleted), gjson.Get(r.Body.String(), "message").String())
		assert.Equal(t, "partial", gjson.Get(r.Body.String(), "status").String())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "successCount").Int())
		assert.Equal(t, int64(1), gjson.Get(r.Body.String(), "failCount").Int())
		items := gjson.Get(r.Body.String(), "items").Array()
		require.Len(t, items, 2)
		itemByUID := make(map[string]gjson.Result)
		for _, item := range items {
			itemByUID[item.Get("id").String()] = item
		}
		assert.True(t, itemByUID[archived.PhotoUID].Get("ok").Bool())
		assert.False(t, itemByUID[visible.PhotoUID].Get("ok").Bool())
		assert.Contains(t, itemByUID[visible.PhotoUID].Get("error").String(), "not in the archive")

		assert.False(t, batchDeleteTestPhotoExists(t, archived))
		assert.NoFileExists(t, archivedFile)
		assertBatchDeleteTestPhotoKept(t, visible, visibleFile)
	})
	t.Run("AllArchived", func(t *testing.T) {
		app, router, conf := NewApiTest()
		BatchPhotosDelete(router)
		folder := batchDeleteTestFolder(t, conf)
		photo1, file1 := batchDeleteTestPhoto(t, conf, folder, "archived1")
		photo2, file2 := batchDeleteTestPhoto(t, conf, folder, "archived2")
		require.NoError(t, photo1.Archive())
		require.NoError(t, photo2.Archive())

		// The archive also lists pictures with the lowest quality score.
		require.NoError(t, photo2.Update("photo_quality", 0))

		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/delete", fmt.Sprintf(`{"photos": [%q, %q]}`, photo1.PhotoUID, photo2.PhotoUID))
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, i18n.Msg(i18n.MsgPermanentlyDeleted), gjson.Get(r.Body.String(), "message").String())
		assert.Equal(t, "success", gjson.Get(r.Body.String(), "status").String())
		assert.Equal(t, int64(2), gjson.Get(r.Body.String(), "successCount").Int())

		assert.False(t, batchDeleteTestPhotoExists(t, photo1))
		assert.False(t, batchDeleteTestPhotoExists(t, photo2))
		assert.NoFileExists(t, file1)
		assert.NoFileExists(t, file2)
	})
	t.Run("OtherSelectionFields", func(t *testing.T) {
		app, router, conf := NewApiTest()
		BatchPhotosDelete(router)
		folder := batchDeleteTestFolder(t, conf)
		selected, selectedFile := batchDeleteTestPhoto(t, conf, folder, "selected")
		labelArchived, labelArchivedFile := batchDeleteTestPhoto(t, conf, folder, "label-archived")
		labelVisible, labelVisibleFile := batchDeleteTestPhoto(t, conf, folder, "label-visible")
		albumVisible, albumVisibleFile := batchDeleteTestPhoto(t, conf, folder, "album-visible")

		label := entity.NewLabel("Batch Delete "+rnd.Base36(8), 0)
		require.NoError(t, label.Create())
		t.Cleanup(func() { _ = entity.UnscopedDb().Delete(label).Error })

		for _, p := range []*entity.Photo{labelArchived, labelVisible} {
			require.NoError(t, entity.NewPhotoLabel(p.ID, label.ID, 0, entity.SrcManual).Create())
		}

		album := entity.NewAlbum("Batch Delete "+rnd.Base36(8), entity.AlbumManual)
		require.NoError(t, album.Create())
		t.Cleanup(func() {
			_ = entity.UnscopedDb().Where("album_uid = ?", album.AlbumUID).Delete(&entity.PhotoAlbum{}).Error
			_ = entity.UnscopedDb().Delete(album).Error
		})
		require.NoError(t, entity.NewPhotoAlbum(albumVisible.PhotoUID, album.AlbumUID).Create())

		require.NoError(t, labelArchived.Archive())

		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/delete",
			fmt.Sprintf(`{"photos": [%q], "labels": [%q], "albums": [%q]}`, selected.PhotoUID, label.LabelUID, album.AlbumUID))
		assert.Equal(t, http.StatusOK, r.Code)
		assert.Equal(t, i18n.Msg(i18n.MsgPermanentlyDeleted), gjson.Get(r.Body.String(), "message").String())

		assert.False(t, batchDeleteTestPhotoExists(t, labelArchived))
		assert.NoFileExists(t, labelArchivedFile)

		for p, fileName := range map[*entity.Photo]string{selected: selectedFile, labelVisible: labelVisibleFile, albumVisible: albumVisibleFile} {
			assertBatchDeleteTestPhotoKept(t, p, fileName)
		}

		var labels, albums int64
		require.NoError(t, entity.UnscopedDb().Model(&entity.PhotoLabel{}).Where("photo_id = ? AND label_id = ?", labelVisible.ID, label.ID).Count(&labels).Error)
		require.NoError(t, entity.UnscopedDb().Model(&entity.PhotoAlbum{}).Where("photo_uid = ? AND album_uid = ? AND hidden = FALSE", albumVisible.PhotoUID, album.AlbumUID).Count(&albums).Error)
		assert.EqualValues(t, 1, labels)
		assert.EqualValues(t, 1, albums)
	})
	t.Run("Removed", func(t *testing.T) {
		app, router, conf := NewApiTest()
		BatchPhotosDelete(router)
		folder := batchDeleteTestFolder(t, conf)
		removed, removedFile := batchDeleteTestPhoto(t, conf, folder, "removed")
		require.NoError(t, removed.Archive())
		require.NoError(t, removed.Update("photo_quality", -1))

		r := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/delete", fmt.Sprintf(`{"photos": [%q]}`, removed.PhotoUID))
		assert.Equal(t, http.StatusBadRequest, r.Code)
		assert.Equal(t, i18n.Msg(i18n.ErrNoItemsSelected), gjson.Get(r.Body.String(), "error").String())

		assertBatchDeleteTestPhotoKept(t, removed, removedFile)
	})
}

func TestDeleteArchivedPhotos(t *testing.T) {
	t.Run("StaleSelection", func(t *testing.T) {
		_, _, conf := NewApiTest()
		folder := batchDeleteTestFolder(t, conf)
		restored, restoredFile := batchDeleteTestPhoto(t, conf, folder, "restored")
		archived, archivedFile := batchDeleteTestPhoto(t, conf, folder, "archived")
		require.NoError(t, restored.Archive())
		require.NoError(t, archived.Archive())

		// Keep the photos as they were selected, then restore one of them.
		selected := entity.Photos{new(entity.Photo), new(entity.Photo)}
		*selected[0], *selected[1] = *restored, *archived
		require.NoError(t, restored.Restore())

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch/photos/delete", nil)

		builder := batchresult.NewBuilder(restored.PhotoUID, archived.PhotoUID)
		deleted, numFiles := deleteArchivedPhotos(c, entity.SessionFixtures.Pointer("alice"), selected, builder, false, false)
		res := builder.Result()

		require.Len(t, deleted, 1)
		assert.Equal(t, archived.PhotoUID, deleted[0].PhotoUID)
		assert.Equal(t, 1, numFiles)
		assert.Equal(t, batchresult.StatusPartial, res.Status)
		require.Len(t, res.Items, 2)
		assert.False(t, res.Items[0].OK)
		assert.Contains(t, res.Items[0].Error, "not in the archive")
		assert.True(t, res.Items[1].OK)
		assert.False(t, batchDeleteTestPhotoExists(t, archived))
		assert.NoFileExists(t, archivedFile)
		assertBatchDeleteTestPhotoKept(t, restored, restoredFile)
	})
	t.Run("CurrentRow", func(t *testing.T) {
		_, _, conf := NewApiTest()
		folder := batchDeleteTestFolder(t, conf)
		renamed, oldFile := batchDeleteTestPhoto(t, conf, folder, "old")
		require.NoError(t, renamed.Archive())
		selected := entity.Photos{new(entity.Photo)}
		*selected[0] = *renamed

		// Rename the selected photo and add another one with its former name and a sidecar file.
		require.NoError(t, renamed.Update("photo_name", "renamed"))
		renamedFile := filepath.Join(conf.OriginalsPath(), folder, "renamed.jpg")
		require.NoError(t, os.Rename(oldFile, renamedFile))
		require.NoError(t, entity.UnscopedDb().Model(&entity.File{}).Where("photo_id = ?", renamed.ID).
			UpdateColumn("file_name", folder+"/renamed.jpg").Error)
		other, otherFile := batchDeleteTestPhoto(t, conf, folder, "old")
		yamlFile, _, err := other.YamlFileName(conf.OriginalsPath(), conf.SidecarPath())
		require.NoError(t, err)
		require.NoError(t, fs.WriteString(yamlFile, "Title: Other\n"))
		t.Cleanup(func() { _ = os.Remove(yamlFile) })

		orig := event.AuditLog
		logger, hook := logtest.NewNullLogger()
		logger.SetLevel(logrus.TraceLevel)
		event.AuditLog = logger
		t.Cleanup(func() { event.AuditLog = orig })

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch/photos/delete", nil)

		builder := batchresult.NewBuilder(renamed.PhotoUID)
		deleted, _ := deleteArchivedPhotos(c, entity.SessionFixtures.Pointer("alice"), selected, builder, false, false)
		res := builder.Result()

		require.Len(t, deleted, 1)
		assert.Equal(t, "renamed", deleted[0].PhotoName)
		assert.Equal(t, batchresult.StatusSuccess, res.Status)
		require.Len(t, res.Items, 1)
		assert.True(t, res.Items[0].OK)

		// The audit entry names the path of the photo that is deleted.
		var audited []string

		for _, entry := range hook.AllEntries() {
			if strings.Contains(entry.Message, folder) {
				audited = append(audited, entry.Message)
			}
		}

		require.Len(t, audited, 1)
		assert.Contains(t, audited[0], folder+"/renamed*")
		assert.False(t, batchDeleteTestPhotoExists(t, renamed))
		assert.NoFileExists(t, renamedFile)
		assert.FileExists(t, yamlFile)
		assertBatchDeleteTestPhotoKept(t, other, otherFile)
	})
	t.Run("Removed", func(t *testing.T) {
		_, _, conf := NewApiTest()
		folder := batchDeleteTestFolder(t, conf)
		removed, removedFile := batchDeleteTestPhoto(t, conf, folder, "removed")
		require.NoError(t, removed.Archive())
		selected := entity.Photos{new(entity.Photo)}
		*selected[0] = *removed
		require.NoError(t, removed.Update("photo_quality", -1))

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch/photos/delete", nil)

		builder := batchresult.NewBuilder(removed.PhotoUID)
		deleted, numFiles := deleteArchivedPhotos(c, entity.SessionFixtures.Pointer("alice"), selected, builder, false, false)
		res := builder.Result()

		assert.Empty(t, deleted)
		assert.Equal(t, 0, numFiles)
		assert.Equal(t, batchresult.StatusFailed, res.Status)
		require.Len(t, res.Items, 1)
		assert.False(t, res.Items[0].OK)
		assertBatchDeleteTestPhotoKept(t, removed, removedFile)
	})
	t.Run("ReadErrorContinues", func(t *testing.T) {
		_, _, conf := NewApiTest()
		folder := batchDeleteTestFolder(t, conf)
		first, firstFile := batchDeleteTestPhoto(t, conf, folder, "first")
		second, secondFile := batchDeleteTestPhoto(t, conf, folder, "second")
		require.NoError(t, first.Archive())
		require.NoError(t, second.Archive())

		// Continue mode reaches both photos and reports both failures.
		calls := 0
		orig := archivedPhoto
		archivedPhoto = func(id uint) (*entity.Photo, error) {
			calls++
			return nil, errors.New("read failed")
		}
		t.Cleanup(func() { archivedPhoto = orig })

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch/photos/delete", nil)

		builder := batchresult.NewBuilder(first.PhotoUID, second.PhotoUID)
		deleted, numFiles := deleteArchivedPhotos(c, entity.SessionFixtures.Pointer("alice"), entity.Photos{first, second}, builder, false, false)
		res := builder.Result()

		assert.Empty(t, deleted)
		assert.Equal(t, 0, numFiles)
		assert.Equal(t, 2, calls)
		assert.Equal(t, batchresult.StatusFailed, res.Status)
		assert.False(t, res.Stopped)
		assertBatchDeleteTestPhotoKept(t, first, firstFile)
		assertBatchDeleteTestPhotoKept(t, second, secondFile)
	})
	t.Run("ReadErrorStops", func(t *testing.T) {
		_, _, conf := NewApiTest()
		folder := batchDeleteTestFolder(t, conf)
		first, firstFile := batchDeleteTestPhoto(t, conf, folder, "first")
		second, secondFile := batchDeleteTestPhoto(t, conf, folder, "second")
		require.NoError(t, first.Archive())
		require.NoError(t, second.Archive())

		// Fail reading the first photo; fail-fast mode must not reach the second one.
		calls := 0
		orig := archivedPhoto
		archivedPhoto = func(id uint) (*entity.Photo, error) {
			calls++
			return nil, errors.New("read failed")
		}
		t.Cleanup(func() { archivedPhoto = orig })

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/batch/photos/delete", nil)

		builder := batchresult.NewBuilder(first.PhotoUID, second.PhotoUID)
		deleted, numFiles := deleteArchivedPhotos(c, entity.SessionFixtures.Pointer("alice"), entity.Photos{first, second}, builder, true, false)
		res := builder.Result()

		assert.Empty(t, deleted)
		assert.Equal(t, 0, numFiles)
		assert.Equal(t, 1, calls)
		assert.True(t, res.Stopped)
		require.Len(t, res.Items, 2)
		assert.Contains(t, res.Items[0].Error, "read failed")
		assert.Contains(t, res.Items[1].Error, "stopped")
		assertBatchDeleteTestPhotoKept(t, first, firstFile)
		assertBatchDeleteTestPhotoKept(t, second, secondFile)
	})
	t.Run("Empty", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		builder := batchresult.NewBuilder()
		deleted, numFiles := deleteArchivedPhotos(c, entity.SessionFixtures.Pointer("alice"), nil, builder, false, false)
		assert.Empty(t, deleted)
		assert.Equal(t, 0, numFiles)
		assert.Equal(t, batchresult.StatusSuccess, builder.Result().Status)
	})
}

// TestBatchPhotosConcurrentConflict forces an archive and a restore batch on the same photo to
// overlap so that restore commits after the archive write but before the archive verification.
// The archive details must then report the conflict rather than claiming success.
func TestBatchPhotosConcurrentConflict(t *testing.T) {
	uid := "ps6sg6be2lvl0yh7"

	require.NoError(t, entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
		UpdateColumns(entity.Values{"deleted_at": nil, "photo_quality": 3}).Error)
	t.Cleanup(func() {
		_ = entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
			UpdateColumn("deleted_at", nil).Error
	})

	app, router, _ := NewApiTest()
	BatchPhotosArchive(router)
	BatchPhotosRestore(router)

	releaseArchive := make(chan struct{})

	var calls int32

	origStates := loadPhotoBatchStatesFunc
	loadPhotoBatchStatesFunc = func(uids []string) (map[string]photoBatchState, error) {
		n := atomic.AddInt32(&calls, 1)

		if n == 1 {
			// Archive verification waits until the concurrent restore committed.
			<-releaseArchive
		}

		states, err := loadPhotoBatchStates(uids)

		if n == 2 {
			// Release the archive verification only after the restore state was read.
			close(releaseArchive)
		}

		return states, err
	}
	t.Cleanup(func() { loadPhotoBatchStatesFunc = origStates })

	var archiveResp *httptest.ResponseRecorder

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()
		archiveResp = PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/archive",
			fmt.Sprintf(`{"photos": [%q]}`, uid))
	}()

	// Start the restore only after the archive write has committed but while its verification waits.
	require.Eventually(t, func() bool {
		states, stateErr := loadPhotoBatchStates([]string{uid})
		return stateErr == nil && states[uid].DeletedAt.Valid
	}, 5*time.Second, 5*time.Millisecond)

	restoreResp := PerformRequestWithBody(app, "POST", "/api/v1/batch/photos/restore",
		fmt.Sprintf(`{"photos": [%q]}`, uid))
	wg.Wait()

	// Restore took effect; its details report success.
	assert.Equal(t, http.StatusOK, restoreResp.Code)
	assert.Equal(t, batchresult.StatusSuccess, gjson.Get(restoreResp.Body.String(), "status").String())
	assert.True(t, gjson.Get(restoreResp.Body.String(), "items.0.ok").Bool())

	// Archive verification happened after restore committed, so it must report a conflict.
	require.NotNil(t, archiveResp)
	assert.Equal(t, http.StatusOK, archiveResp.Code)
	assert.Equal(t, batchresult.StatusFailed, gjson.Get(archiveResp.Body.String(), "status").String())
	assert.False(t, gjson.Get(archiveResp.Body.String(), "items.0.ok").Bool())
	assert.Contains(t, gjson.Get(archiveResp.Body.String(), "items.0.error").String(), "concurrent")

	var state entity.Photo
	require.NoError(t, entity.UnscopedDb().Where("photo_uid = ?", uid).First(&state).Error)
	assert.False(t, state.DeletedAt.Valid)
}
