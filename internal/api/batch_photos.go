package api

import (
	"errors"
	"net/http"
	"path"
	"time"

	"github.com/dustin/go-humanize/english"

	"github.com/gin-gonic/gin"

	"github.com/photoprism/photoprism/internal/auth/acl"
	"github.com/photoprism/photoprism/internal/batchresult"
	"github.com/photoprism/photoprism/internal/config"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/entity/query"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/form"
	"github.com/photoprism/photoprism/internal/photoprism"
	"github.com/photoprism/photoprism/internal/photoprism/get"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/i18n"
)

// errPhotoNotInArchive is the per-item reason when a selected photo cannot be permanently deleted
// because it is not in the archive.
var errPhotoNotInArchive = errors.New("photo is not in the archive")

// restrictPhotoSelection narrows frm.Photos to the pictures the session may access, so a batch
// action stays within the session's shared scope, mirroring the single-photo and album update
// gates. SelectedPhotoUIDsForSession is client and user role aware and returns the input without a
// query for full-access sessions, so admins incur no overhead. It returns false and reports the
// selection as not found when the scoped selection is empty or the lookup fails.
func restrictPhotoSelection(c *gin.Context, s *entity.Session, frm *form.Selection) bool {
	scoped, err := query.SelectedPhotoUIDsForSession(frm.Photos, s)

	if err != nil || len(scoped) == 0 {
		AbortEntityNotFound(c)
		return false
	}

	frm.Photos = scoped

	return true
}

// BatchPhotosArchive moves multiple photos to the archive.
//
//	@Summary	moves multiple photos to the archive
//	@Id			BatchPhotosArchive
//	@Tags		Photos
//	@Accept		json
//	@Produce	json
//	@Success	200							{object}	api.SelectionResponse
//	@Failure	400,401,403,404,413,429,500	{object}	i18n.Response
//	@Param		photos						body		form.Selection	true	"Photo Selection"
//	@Router		/api/v1/batch/photos/archive [post]
func BatchPhotosArchive(router *gin.RouterGroup) {
	router.POST("/batch/photos/archive", func(c *gin.Context) {
		s := Auth(c, acl.ResourcePhotos, acl.ActionDelete)

		if s.Abort(c) {
			return
		}

		var frm form.Selection

		// Assign and validate request form values.
		LimitRequestBodyBytes(c, MaxSelectionRequestBytes)

		if err := c.BindJSON(&frm); err != nil {
			if IsRequestBodyTooLarge(err) {
				AbortRequestTooLarge(c, i18n.ErrBadRequest)
				return
			}

			AbortBadRequest(c, err)
			return
		}

		if len(frm.Photos) == 0 {
			Abort(c, http.StatusBadRequest, i18n.ErrNoItemsSelected)
			return
		}

		// Restrict the selection to the session's shared scope.
		if !restrictPhotoSelection(c, s, &frm) {
			return
		}

		log.Infof("photos: archiving %s", clean.Log(frm.String()))

		// Fetch selection from index.
		photos, err := query.SelectedPhotosForSession(frm, s)

		if err != nil {
			AbortEntityNotFound(c)
			return
		}

		saveSidecar := func(p *entity.Photo) {}

		if get.Config().SidecarYaml() {
			saveSidecar = SaveSidecarYaml
		}

		res := runPhotoBatch(&frm, photos, func(p *entity.Photo) error {
			return p.Archive()
		}, saveSidecar, func(state photoBatchState) bool {
			// Archived and not removed.
			return state.DeletedAt.Valid && state.PhotoQuality > -1
		})

		if res.SuccessCount > 0 {
			// Update precalculated photo and file counts.
			entity.UpdateCountsAsync()

			// Update album, subject, and label cover thumbs.
			query.UpdateCoversAsync()

			UpdateClientConfig()

			event.EntitiesArchived("photos", succeededUIDs(res))
		}

		logBatchErrors("archive", res)

		respondBatch(c, http.StatusOK, i18n.MsgSelectionArchived, res)
	})
}

// BatchPhotosRestore restores multiple photos from the archive.
//
//	@Summary	restores multiple photos from the archive
//	@Id			BatchPhotosRestore
//	@Tags		Photos
//	@Accept		json
//	@Produce	json
//	@Success	200							{object}	api.SelectionResponse
//	@Failure	400,401,403,404,413,429,500	{object}	i18n.Response
//	@Param		photos						body		form.Selection	true	"Photo Selection"
//	@Router		/api/v1/batch/photos/restore [post]
func BatchPhotosRestore(router *gin.RouterGroup) {
	router.POST("/batch/photos/restore", func(c *gin.Context) {
		s := Auth(c, acl.ResourcePhotos, acl.ActionDelete)

		if s.Abort(c) {
			return
		}

		var frm form.Selection

		LimitRequestBodyBytes(c, MaxSelectionRequestBytes)

		if err := c.BindJSON(&frm); err != nil {
			if IsRequestBodyTooLarge(err) {
				AbortRequestTooLarge(c, i18n.ErrBadRequest)
				return
			}

			AbortBadRequest(c, err)
			return
		}

		if len(frm.Photos) == 0 {
			Abort(c, http.StatusBadRequest, i18n.ErrNoItemsSelected)
			return
		}

		// Restrict the selection to the session's shared scope.
		if !restrictPhotoSelection(c, s, &frm) {
			return
		}

		log.Infof("photos: restoring %s", clean.Log(frm.String()))

		// Fetch selection from index.
		photos, err := query.SelectedPhotosForSession(frm, s)

		if err != nil {
			AbortEntityNotFound(c)
			return
		}

		saveSidecar := func(p *entity.Photo) {}

		if get.Config().SidecarYaml() {
			saveSidecar = SaveSidecarYaml
		}

		res := runPhotoBatch(&frm, photos, func(p *entity.Photo) error {
			return p.Restore()
		}, saveSidecar, func(state photoBatchState) bool {
			return !state.DeletedAt.Valid
		})

		if res.SuccessCount > 0 {
			// Update precalculated photo and file counts.
			entity.UpdateCountsAsync()

			// Update album, subject, and label cover thumbs.
			query.UpdateCoversAsync()

			UpdateClientConfig()

			event.EntitiesRestored("photos", succeededUIDs(res))
		}

		logBatchErrors("restore", res)

		respondBatch(c, http.StatusOK, i18n.MsgSelectionRestored, res)
	})
}

// BatchPhotosApprove approves multiple photos that are currently under review.
//
//	@Summary	approves multiple photos that are currently under review
//	@Id			BatchPhotosApprove
//	@Tags		Photos
//	@Accept		json
//	@Produce	json
//	@Success	200						{object}	api.SelectionResponse
//	@Failure	400,401,403,404,413,429	{object}	i18n.Response
//	@Param		photos					body		form.Selection	true	"Photo Selection"
//	@Router		/api/v1/batch/photos/approve [post]
func BatchPhotosApprove(router *gin.RouterGroup) {
	router.POST("/batch/photos/approve", func(c *gin.Context) {
		s := Auth(c, acl.ResourcePhotos, acl.ActionUpdate)

		if s.Abort(c) {
			return
		}

		var frm form.Selection

		LimitRequestBodyBytes(c, MaxSelectionRequestBytes)

		if err := c.BindJSON(&frm); err != nil {
			if IsRequestBodyTooLarge(err) {
				AbortRequestTooLarge(c, i18n.ErrBadRequest)
				return
			}

			AbortBadRequest(c, err)
			return
		}

		if len(frm.Photos) == 0 {
			Abort(c, http.StatusBadRequest, i18n.ErrNoItemsSelected)
			return
		}

		// Restrict the selection to the session's shared scope.
		if !restrictPhotoSelection(c, s, &frm) {
			return
		}

		log.Infof("photos: approving %s", clean.Log(frm.String()))

		// Fetch selection from index.
		photos, err := query.SelectedPhotosForSession(frm, s)

		if err != nil {
			AbortEntityNotFound(c)
			return
		}

		res := runPhotoBatch(&frm, photos, func(p *entity.Photo) error {
			return p.Approve()
		}, SaveSidecarYaml, func(state photoBatchState) bool {
			// Mirrors entity.Photo.Approved() for the freshly written row.
			return !state.DeletedAt.Valid && (state.PhotoQuality >= 3 || state.PhotoType != entity.MediaImage || state.EditedAt != nil)
		})

		if res.SuccessCount > 0 {
			UpdateClientConfig()

			event.EntitiesUpdated("photos", succeededUIDs(res))
		}

		logBatchErrors("approve", res)

		respondBatch(c, http.StatusOK, i18n.MsgSelectionApproved, res)
	})
}

// BatchPhotosPrivate toggles private state of multiple photos.
//
//	@Summary	toggles private state of multiple photos
//	@Id			BatchPhotosPrivate
//	@Tags		Photos
//	@Accept		json
//	@Produce	json
//	@Success	200							{object}	api.SelectionResponse
//	@Failure	400,401,403,404,413,429,500	{object}	i18n.Response
//	@Param		photos						body		form.Selection	true	"Photo Selection"
//	@Router		/api/v1/batch/photos/private [post]
func BatchPhotosPrivate(router *gin.RouterGroup) {
	router.POST("/batch/photos/private", func(c *gin.Context) {
		s := Auth(c, acl.ResourcePhotos, acl.AccessPrivate)

		if s.Abort(c) {
			return
		}

		var frm form.Selection

		LimitRequestBodyBytes(c, MaxSelectionRequestBytes)

		if err := c.BindJSON(&frm); err != nil {
			if IsRequestBodyTooLarge(err) {
				AbortRequestTooLarge(c, i18n.ErrBadRequest)
				return
			}

			AbortBadRequest(c, err)
			return
		}

		if len(frm.Photos) == 0 {
			Abort(c, http.StatusBadRequest, i18n.ErrNoItemsSelected)
			return
		}

		// Restrict the selection to the session's shared scope.
		if !restrictPhotoSelection(c, s, &frm) {
			return
		}

		log.Infof("photos: updating private flag for %s", clean.Log(frm.String()))

		// Fetch selection from index for sidecar exports.
		photos, err := query.SelectedPhotosForSession(frm, s)

		if err != nil {
			AbortEntityNotFound(c)
			return
		}

		byUID := make(map[string]*entity.Photo, len(photos))

		for i := range photos {
			byUID[photos[i].PhotoUID] = photos[i]
		}

		// Determine the target state of every item from its current row, so concurrent toggles on
		// the same photo can be detected by re-reading the state instead of overwriting each other.
		before, err := loadPhotoBatchStates(frm.Photos)

		if err != nil {
			log.Errorf("private: %s", err)
			AbortSaveFailed(c)
			return
		}

		builder := batchresult.NewBuilder(frm.Photos...)
		targets := make(map[string]bool, len(builder.IDs()))

		var applied []string

		for _, uid := range builder.IDs() {
			state, found := before[uid]

			if !found {
				builder.Failure(uid, errors.New(uid+": "+batchresult.ErrNotFound.Error()))

				if frm.StopOnError {
					builder.Stop()
					break
				}

				continue
			}

			target := !state.PhotoPrivate
			targets[uid] = target

			if err := entity.UnscopedDb().Model(&entity.Photo{}).Where("photo_uid = ?", uid).
				UpdateColumn("photo_private", target).Error; err != nil {
				builder.Failure(uid, err)

				if frm.StopOnError {
					builder.Stop()
					break
				}

				continue
			}

			applied = append(applied, uid)
		}

		after, err := loadPhotoBatchStatesFunc(applied)

		if err != nil {
			log.Errorf("private: %s (verify result)", err)

			for _, uid := range applied {
				builder.Failure(uid, err)
			}
		} else {
			for _, uid := range applied {
				if state, found := after[uid]; found && state.PhotoPrivate == targets[uid] {
					builder.Success(uid)

					if p := byUID[uid]; p != nil {
						SaveSidecarYaml(p)
					}
				} else {
					builder.Failure(uid, errors.New(uid+": "+batchresult.ErrConflict.Error()))
				}
			}
		}

		res := builder.Result()

		if res.SuccessCount > 0 {
			// Update precalculated photo and file counts.
			entity.UpdateCountsAsync()

			UpdateClientConfig()

			FlushCoverCache()

			event.EntitiesUpdated("photos", succeededUIDs(res))
		}

		logBatchErrors("private", res)

		respondBatch(c, http.StatusOK, i18n.MsgSelectionProtected, res)
	})
}

// BatchPhotosDelete permanently removes multiple photos from the archive.
//
//	@Summary	permanently removes multiple or all photos from the archive
//	@Id			BatchPhotosDelete
//	@Tags		Photos
//	@Accept		json
//	@Produce	json
//	@Success	200						{object}	api.SelectionResponse
//	@Failure	400,401,403,404,413,429	{object}	i18n.Response
//	@Param		photos					body		form.Selection	true	"All or Photo Selection"
//	@Router		/api/v1/batch/photos/delete [post]
func BatchPhotosDelete(router *gin.RouterGroup) {
	router.POST("/batch/photos/delete", func(c *gin.Context) {
		s := Auth(c, acl.ResourcePhotos, acl.ActionDelete)

		if s.Abort(c) {
			return
		}

		conf := get.Config()

		if conf.ReadOnly() || !conf.Settings().Features.Delete {
			AbortFeatureDisabled(c)
			return
		}

		var frm form.Selection

		LimitRequestBodyBytes(c, MaxSelectionRequestBytes)

		if err := c.BindJSON(&frm); err != nil {
			if IsRequestBodyTooLarge(err) {
				AbortRequestTooLarge(c, i18n.ErrBadRequest)
				return
			}

			AbortBadRequest(c, err)
			return
		}

		deleteStart := time.Now()

		var photos entity.Photos
		var err error

		// Abort if user wants to delete all but does not have sufficient privileges.
		if frm.All && !acl.Rules.AllowAll(acl.ResourcePhotos, s.GetUserRole(), acl.Permissions{acl.AccessAll, acl.ActionManage}) {
			AbortForbidden(c)
			return
		}

		// Restrict an explicit selection to the session's shared scope.
		if !frm.All && len(frm.Photos) > 0 && !restrictPhotoSelection(c, s, &frm) {
			return
		}

		// Get selection or all archived photos if f.All is true.
		switch {
		case len(frm.Photos) == 0 && !frm.All:
			Abort(c, http.StatusBadRequest, i18n.ErrNoItemsSelected)
			return
		case frm.All:
			photos, err = query.ArchivedPhotos(1000000, 0)
		default:
			photos, err = query.SelectedPhotosForSession(frm, s)
		}

		if err != nil {
			log.Errorf("archive: %s", err)
			Abort(c, http.StatusBadRequest, i18n.ErrNoItemsSelected)
			return
		}

		// Permanently delete only the selected photos that are archived and not removed.
		archived := photos.Archived()

		if skipped := len(photos) - len(archived); skipped > 0 {
			log.Infof("archive: skipped %s not in archive", english.Plural(skipped, "selected photo", "selected photos"))
		}

		// The builder lists every explicitly requested UID; "all" selections only record failures
		// to keep the response bounded.
		var builder *batchresult.Builder

		if frm.All {
			builder = batchresult.NewBuilder()
		} else {
			builder = batchresult.NewBuilder(frm.Photos...)

			// Selected but not archived items cannot be deleted; report them per UID.
			archivedUIDs := make(map[string]bool, len(archived))

			for _, p := range archived {
				archivedUIDs[p.PhotoUID] = true
			}

			for _, p := range photos {
				if !archivedUIDs[p.PhotoUID] {
					builder.Failure(p.PhotoUID, errPhotoNotInArchive)
				}
			}
		}

		// Abort if no photos were found in the archive.
		if len(archived) == 0 {
			// "Delete all" with an empty archive has no items to report.
			if len(builder.IDs()) == 0 {
				Abort(c, http.StatusBadRequest, i18n.ErrNoItemsSelected)
				return
			}

			res := builder.Result()
			respondBatch(c, http.StatusBadRequest, i18n.ErrNoItemsSelected, res)
			return
		}

		log.Infof("archive: deleting %s", english.Plural(len(archived), "photo", "photos"))

		deleted, numFiles := deleteArchivedPhotos(c, s, archived, builder, frm.StopOnError, frm.All)

		if numFiles > 0 || len(deleted) > 0 {
			log.Infof("archive: deleted %s and %s [%s]", english.Plural(numFiles, "file", "files"), english.Plural(len(deleted), "photo", "photos"), time.Since(deleteStart))
		}

		res := builder.Result()

		// Any photos deleted?
		if len(deleted) > 0 {
			config.FlushUsageCache()

			// Update precalculated photo and file counts.
			entity.UpdateCountsAsync()

			UpdateClientConfig()

			event.EntitiesDeleted("photos", deleted.UIDs())
		}

		// Per-item causes are already logged by deleteArchivedPhotos, so no batch error log here.
		respondBatch(c, http.StatusOK, i18n.MsgPermanentlyDeleted, res)
	})
}

// archivedPhoto returns the current row of a photo if it is archived; tests may replace it.
var archivedPhoto = query.ArchivedPhoto

// deleteArchivedPhotos permanently deletes the photos that are archived when they are reached, using their
// current rows. It records the per-item outcome in builder and returns the deleted photos with the number
// of files removed. With stopOnError, the first failure marks all following items as not processed;
// otherwise remaining photos are handled independently.
func deleteArchivedPhotos(c *gin.Context, s *entity.Session, photos entity.Photos, builder *batchresult.Builder, stopOnError, all bool) (deleted entity.Photos, numFiles int) {
	for _, selected := range photos {
		// Delete only photos that are still archived.
		p, err := archivedPhoto(selected.ID)

		if err != nil {
			log.Errorf("archive: %s (%s)", clean.Log(selected.PhotoUID), clean.Error(err))
			builder.Failure(selected.PhotoUID, err)

			if stopOnError {
				stopPhotoBatch(builder, photosAfter(photos, selected))
				break
			}

			continue
		} else if p == nil {
			log.Infof("archive: skipped %s, no longer in archive", clean.Log(selected.PhotoUID))
			builder.Failure(selected.PhotoUID, errPhotoNotInArchive)

			if stopOnError {
				stopPhotoBatch(builder, photosAfter(photos, selected))
				break
			}

			continue
		}

		// Report file deletion.
		event.AuditWarn([]string{ClientIP(c), clean.LogQuote(s.UserName), "delete", clean.Log(path.Join(p.PhotoPath, p.PhotoName+"*"))})

		// Remove all related files from storage.
		n, err := photoprism.DeletePhoto(p, true, true)

		numFiles += n

		if err != nil {
			log.Errorf("delete: %s", err)
			builder.Failure(p.PhotoUID, err)

			if stopOnError {
				stopPhotoBatch(builder, photosAfter(photos, selected))
				break
			}
		} else {
			if all {
				builder.AddSuccess(1)
			} else {
				builder.Success(p.PhotoUID)
			}

			deleted = append(deleted, p)
		}
	}

	return deleted, numFiles
}

// photosAfter returns the UIDs of the photos following current in the given list.
func photosAfter(photos entity.Photos, current *entity.Photo) []string {
	uids := make([]string, 0)

	found := false

	for _, p := range photos {
		if found {
			uids = append(uids, p.PhotoUID)
		} else if p == current || p.ID == current.ID {
			found = true
		}
	}

	return uids
}

// stopPhotoBatch marks the given UIDs, and any other pending item, as not processed.
func stopPhotoBatch(builder *batchresult.Builder, remaining []string) {
	for _, uid := range remaining {
		builder.Failure(uid, batchresult.ErrStopped)
	}

	builder.Stop()
}

// logBatchErrors writes one log line per failed batch item so operators still see causes that the
// response only exposes to the requesting client.
func logBatchErrors(prefix string, res *batchresult.Result) {
	if res == nil {
		return
	}

	for _, item := range res.Items {
		if !item.OK && item.Error != batchresult.ErrStopped.Error() {
			log.Errorf("%s: %s (%s)", prefix, clean.Log(item.ID), item.Error)
		}
	}
}

// succeededUIDs returns the UIDs of the items the operation was applied to.
func succeededUIDs(res *batchresult.Result) []string {
	if res == nil {
		return nil
	}

	uids := make([]string, 0, res.SuccessCount)

	for _, item := range res.Items {
		if item.OK {
			uids = append(uids, item.ID)
		}
	}

	return uids
}
