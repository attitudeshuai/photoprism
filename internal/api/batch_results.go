/*
Shared per-item result handling for batch endpoints.

Copyright (c) 2018 - 2026 PhotoPrism UG. All rights reserved.

	This program is free software: you can redistribute it and/or modify
	it under Version 3 of the GNU Affero General Public License (the "AGPL"):
	<https://docs.photoprism.app/license/agpl>

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	The AGPL is supplemented by our Trademark and Brand Guidelines,
	which describe how our Brand Assets may be used:
	<https://www.photoprism.app/trademark/>

Feel free to send an email to hello@photoprism.app if you have questions,
want to support our work, or just want to say hello.

Additional information can be found in our Developer Guide:
<https://docs.photoprism.app/developer-guide/>
*/
package api

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/photoprism/photoprism/internal/batchresult"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/form"
	"github.com/photoprism/photoprism/pkg/i18n"
)

// SelectionResponse embeds the standard i18n response and the per-item batch result, so clients
// that only read code and message keep the previous behavior while new clients can inspect items.
type SelectionResponse struct {
	i18n.Response
	batchresult.Result
}

// respondBatch sends the given i18n message together with the per-item batch result.
func respondBatch(c *gin.Context, code int, msg i18n.Message, res *batchresult.Result) {
	c.JSON(code, SelectionResponse{
		Response: i18n.NewResponse(code, msg),
		Result:   *res,
	})
}

// photoBatchState is the minimal current row state used to verify batch outcomes.
type photoBatchState struct {
	PhotoUID     string
	PhotoType    string
	DeletedAt    gorm.DeletedAt
	PhotoQuality int
	PhotoPrivate bool
	EditedAt     *time.Time
}

// loadPhotoBatchStatesFunc is the state lookup used after batch writes; tests may replace it to
// force interleavings with concurrent batches.
var loadPhotoBatchStatesFunc = loadPhotoBatchStates

// loadPhotoBatchStates returns the current rows of the given photo UIDs, including archived and
// removed rows, so batch results can be verified against the state that actually took effect.
func loadPhotoBatchStates(uids []string) (map[string]photoBatchState, error) {
	states := make(map[string]photoBatchState, len(uids))

	if len(uids) == 0 {
		return states, nil
	}

	rows := make([]photoBatchState, 0, len(uids))

	if err := entity.UnscopedDb().Table(entity.Photo{}.TableName()).
		Select("photo_uid, photo_type, deleted_at, photo_quality, photo_private, edited_at").
		Where("photo_uid IN (?)", uids).
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	for _, row := range rows {
		states[row.PhotoUID] = row
	}

	return states, nil
}

// photoBatchAction applies a state change to a single photo.
type photoBatchAction func(p *entity.Photo) error

// runPhotoBatch applies action to every uniquely selected photo in selection order, honors the
// stop-on-error option, and only reports an item as successful when the intended end state is
// present after all writes. This final verification ensures the details reflect which operation
// actually took effect when another batch changes the same photos concurrently.
func runPhotoBatch(frm *form.Selection, photos entity.Photos, action photoBatchAction, onSuccess func(p *entity.Photo), intended func(state photoBatchState) bool) *batchresult.Result {
	builder := batchresult.NewBuilder(frm.Photos...)

	byUID := make(map[string]*entity.Photo, len(photos))

	for i := range photos {
		byUID[photos[i].PhotoUID] = photos[i]
	}

	// applied lists items whose write returned without an error and still need verification.
	var applied []string

	process := func(uid string, p *entity.Photo) bool {
		if p == nil {
			builder.Failure(uid, fmt.Errorf("%s: %w", uid, batchresult.ErrNotFound))
			return false
		}

		if err := action(p); err != nil {
			builder.Failure(uid, err)
			return false
		}

		applied = append(applied, uid)
		return true
	}

	for _, uid := range builder.IDs() {
		if !process(uid, byUID[uid]) && frm.StopOnError {
			builder.Stop()
			break
		}
	}

	if len(applied) > 0 {
		states, err := loadPhotoBatchStatesFunc(applied)
		if err != nil {
			log.Errorf("batch: %s (verify result)", err)

			for _, uid := range applied {
				builder.Failure(uid, err)
			}
		} else {
			for _, uid := range applied {
				state, found := states[uid]

				if found && intended(state) {
					builder.Success(uid)

					if onSuccess != nil {
						onSuccess(byUID[uid])
					}
				} else {
					builder.Failure(uid, fmt.Errorf("%s: %w", uid, batchresult.ErrConflict))
				}
			}
		}
	}

	return builder.Result()
}
