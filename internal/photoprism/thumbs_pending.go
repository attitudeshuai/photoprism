package photoprism

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/dustin/go-humanize/english"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/entity/query"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/mutex"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/fs"
	"github.com/photoprism/photoprism/pkg/fs/disk"
	"github.com/photoprism/photoprism/pkg/i18n"
	"github.com/photoprism/photoprism/pkg/log/status"
)

// PendingThumbsBatchSize limits the number of files fetched in one pending-list query.
const PendingThumbsBatchSize = 100

// PendingThumbsResult summarizes a targeted thumbnail retry run.
type PendingThumbsResult struct {
	// Processed is the number of pending files generation was attempted for.
	Processed int `json:"processed"`
	// Repaired is the number of pending or failed size results that reached a terminal state.
	Repaired int `json:"repaired"`
	// Remaining is the number of pending or failed size results still open after the run.
	Remaining int `json:"remaining"`
	// Pruned is the number of files whose results were removed because the original no longer exists.
	Pruned int `json:"pruned"`
	// Failed is the number of files with at least one size still pending or failed.
	Failed int `json:"failed"`
}

// pendingThumbBasePath resolves the library root a status row was recorded against.
func (w *Thumbs) pendingThumbBasePath(root string) (basePath string) {
	switch root {
	case entity.RootSidecar:
		basePath = w.conf.SidecarPath()
	case entity.RootImport:
		basePath = w.conf.ImportPath()
	default:
		basePath = w.conf.OriginalsPath()
	}

	return basePath
}

// StartPending generates only the thumbnail sizes recorded as pending or failed, without
// scanning any directory. It may run concurrently with a regular indexing run.
func (w *Thumbs) StartPending() (result PendingThumbsResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("thumbs: pending run panicked: %v", r)
			log.Error(err)
		}
	}()

	// Reset the cached disk usage so a freshly freed disk is detected immediately.
	disk.FlushFree()

	if w.insufficientStorage() {
		return result, status.ErrInsufficientStorage
	}

	if err = mutex.ThumbsPendingWorker.Start(); err != nil {
		return result, err
	}

	defer mutex.ThumbsPendingWorker.Stop()

	thumbnailsPath := w.conf.ThumbCachePath()

	var afterRoot, afterName string

	for {
		if mutex.ThumbsPendingWorker.Canceled() {
			log.Infof("thumbs: pending retry canceled")
			return result, status.ErrCanceled
		}

		// Stop before generating more files if storage dropped below the threshold.
		if w.insufficientStorage() {
			mutex.ThumbsPendingWorker.Cancel()
			return result, status.ErrInsufficientStorage
		}

		files, queryErr := query.PendingThumbFilesFrom(PendingThumbsBatchSize, afterRoot, afterName)
		if queryErr != nil {
			return result, queryErr
		} else if len(files) == 0 {
			break
		}

		for i := range files {
			group := &files[i]

			if mutex.ThumbsPendingWorker.Canceled() {
				log.Infof("thumbs: pending retry canceled")
				return result, status.ErrCanceled
			}

			openCount := len(group.Sizes)
			absName := filepath.Join(w.pendingThumbBasePath(group.FileRoot), group.FileName)

			// Remove results for originals that no longer exist instead of retrying them forever.
			if !fs.PathExists(absName) {
				if delErr := entity.DeleteThumbStatuses(group.FileRoot, group.FileName); delErr != nil {
					log.Warnf("thumbs: %s while pruning %s", clean.Error(delErr), clean.Log(group.FileName))
				} else {
					result.Pruned++
				}

				continue
			}

			mf, mediaErr := NewMediaFile(absName)
			if mediaErr != nil || mf.Empty() || !mf.IsPreviewImage() {
				log.Warnf("thumbs: skipped pending %s (%s)", clean.Log(group.FileName), clean.Error(mediaErr))
				continue
			}

			result.Processed++

			event.Publish("index.thumbnails", event.Data{
				"fileName": group.FileName,
				"baseName": filepath.Base(group.FileName),
				"force":    false,
				"pending":  true,
			})

			// GenerateThumbnails writes per-size outcomes through guarded updates, so a
			// concurrent index run cannot make one producer overwrite the other's results.
			if genErr := mf.GenerateThumbnails(thumbnailsPath, false); genErr != nil {
				if errors.Is(genErr, status.ErrInsufficientStorage) {
					log.Errorf("thumbs: aborting pending retry due to insufficient storage")
					event.ErrorMsg(i18n.ErrInsufficientStorage)
					mutex.ThumbsPendingWorker.Cancel()
					return result, status.ErrInsufficientStorage
				}

				// Source-level failures are recorded per size; continue with the next file.
				log.Warnf("thumbs: %s for %s", clean.Error(genErr), clean.Log(group.FileName))
			}

			remaining, countErr := query.PendingThumbSizes(group.FileRoot, group.FileName)
			if countErr != nil {
				log.Warnf("thumbs: %s while counting results for %s", clean.Error(countErr), clean.Log(group.FileName))
			} else {
				result.Repaired += openCount - remaining
				result.Remaining += remaining

				if remaining > 0 {
					result.Failed++
				}
			}
		}

		afterRoot, afterName = files[len(files)-1].FileRoot, files[len(files)-1].FileName
	}

	log.Infof("thumbs: pending retry processed %s (%s repaired, %s remaining, %s pruned)",
		english.Plural(result.Processed, "file", "files"),
		english.Plural(result.Repaired, "size", "sizes"),
		english.Plural(result.Remaining, "size", "sizes"),
		english.Plural(result.Pruned, "file", "files"))

	return result, nil
}
