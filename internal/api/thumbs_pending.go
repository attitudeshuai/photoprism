package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/photoprism/photoprism/internal/auth/acl"
	"github.com/photoprism/photoprism/internal/entity/query"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/form"
	"github.com/photoprism/photoprism/internal/mutex"
	"github.com/photoprism/photoprism/internal/photoprism/get"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/i18n"
	"github.com/photoprism/photoprism/pkg/log/status"
)

// FindPendingThumbs lists the thumbnail size results that are pending or failed, with optional
// status, path, and pagination filters.
//
//	@Summary	find pending thumbnail results
//	@Id			FindPendingThumbs
//	@Tags		Library
//	@Produce	json
//	@Success	200				{object}	map[string]any
//	@Failure	400,401,403,429	{object}	i18n.Response
//	@Param		status			query		string	false	"status filter (pending, failed, skipped, succeeded)"
//	@Param		path			query		string	false	"substring of the file path"
//	@Param		count			query		int		false	"maximum number of results (default 100, max 1000)"
//	@Param		offset			query		int		false	"result offset"
//	@Router		/api/v1/thumbs/pending [get]
func FindPendingThumbs(router *gin.RouterGroup) {
	router.GET("/thumbs/pending", func(c *gin.Context) {
		s := Auth(c, acl.ResourcePhotos, acl.ActionUpdate)

		if s.Abort(c) {
			return
		}

		var frm form.ThumbStatusSearch

		if err := c.BindQuery(&frm); err != nil {
			AbortBadRequest(c, err)
			return
		}

		if err := frm.Normalize(); err != nil {
			AbortBadRequest(c, err)
			return
		}

		result, err := query.ThumbStatuses(frm)
		if err != nil {
			AbortBadRequest(c, err)
			return
		}

		counts, err := query.ThumbStatusCounts()
		if err != nil {
			AbortBadRequest(c, err)
			return
		}

		AddCountHeader(c, len(result))
		AddLimitHeader(c, frm.Count)
		AddOffsetHeader(c, frm.Offset)

		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"count":   len(result),
			"limit":   frm.Count,
			"offset":  frm.Offset,
			"results": result,
			"totals":  counts,
		})
	})
}

// StartPendingThumbs generates only the thumbnail sizes recorded as pending or failed.
//
//	@Summary	start targeted thumbnail retry
//	@Id			StartPendingThumbs
//	@Tags		Library
//	@Produce	json
//	@Success	200					{object}	map[string]any
//	@Failure	401,403,429,507		{object}	i18n.Response
//	@Router		/api/v1/thumbs/pending [post]
func StartPendingThumbs(router *gin.RouterGroup) {
	router.POST("/thumbs/pending", func(c *gin.Context) {
		s := Auth(c, acl.ResourcePhotos, acl.ActionUpdate)

		if s.Abort(c) {
			return
		}

		conf := get.Config()

		if !conf.Settings().Features.Library {
			AbortFeatureDisabled(c)
			return
		}

		if mutex.ThumbsPendingWorker.Running() {
			AbortBusy(c)
			return
		}

		// Abort when the storage filesystem is critically low, mirroring the index endpoint.
		if _, low, _ := conf.StorageLow(); low {
			event.AuditErr([]string{ClientIP(c), "session %s", "generate pending thumbnails", status.InsufficientStorage}, s.RefID)
			Abort(c, http.StatusInsufficientStorage, i18n.ErrInsufficientStorage)
			return
		}

		result, err := get.Thumbs().StartPending()
		if err != nil {
			if errors.Is(err, status.ErrInsufficientStorage) {
				event.AuditErr([]string{ClientIP(c), "session %s", "generate pending thumbnails", status.InsufficientStorage}, s.RefID)
				Abort(c, http.StatusInsufficientStorage, i18n.ErrInsufficientStorage)
				return
			}

			if mutex.ThumbsPendingWorker.Running() || errors.Is(err, status.ErrCanceled) {
				AbortBusy(c)
				return
			}

			log.Errorf("thumbs: %s while generating pending thumbnails", clean.Error(err))
			AbortUnexpectedError(c)
			return
		}

		event.AuditInfo([]string{ClientIP(c), "session %s", "generate pending thumbnails", status.Succeeded}, s.RefID)

		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"processed": result.Processed,
			"repaired":  result.Repaired,
			"remaining": result.Remaining,
			"pruned":    result.Pruned,
			"failed":    result.Failed,
		})
	})
}
