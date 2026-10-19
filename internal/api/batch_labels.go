package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/photoprism/photoprism/internal/auth/acl"
	"github.com/photoprism/photoprism/internal/batchresult"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/form"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/i18n"
)

// BatchLabelsDelete deletes multiple labels.
//
//	@Summary	deletes multiple labels
//	@Id			BatchLabelsDelete
//	@Tags		Labels
//	@Accept		json
//	@Produce	json
//	@Success	200						{object}	api.SelectionResponse
//	@Failure	400,401,403,413,429,500	{object}	i18n.Response
//	@Param		labels					body		form.Selection	true	"Label Selection"
//	@Router		/api/v1/batch/labels/delete [post]
func BatchLabelsDelete(router *gin.RouterGroup) {
	router.POST("/batch/labels/delete", func(c *gin.Context) {
		s := Auth(c, acl.ResourceLabels, acl.ActionDelete)

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

		if len(frm.Labels) == 0 {
			log.Error("no labels selected")
			Abort(c, http.StatusBadRequest, i18n.ErrNoLabelsSelected)
			return
		}

		log.Infof("labels: deleting %s", clean.Log(frm.String()))

		var labels entity.Labels

		if err := entity.Db().Where("label_uid IN (?)", frm.Labels).Find(&labels).Error; err != nil {
			Error(c, http.StatusInternalServerError, err, i18n.ErrDeleteFailed)
			return
		}

		builder := batchresult.NewBuilder(frm.Labels...)

		for _, label := range labels {
			if err := label.Delete(); err != nil {
				logErr("labels", err)
				builder.Failure(label.LabelUID, err)

				if frm.StopOnError {
					builder.Stop()
					break
				}
			} else {
				builder.Success(label.LabelUID)
			}
		}

		res := builder.Result()

		UpdateClientConfig()

		// Only notify clients about labels that were actually deleted.
		event.EntitiesDeleted("labels", succeededUIDs(res))

		respondBatch(c, http.StatusOK, i18n.MsgLabelsDeleted, res)
	})
}
