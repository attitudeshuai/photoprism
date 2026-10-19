package batch

import (
	"github.com/photoprism/photoprism/internal/batchresult"
	"github.com/photoprism/photoprism/internal/entity/search"
)

// PhotosResponse represents the selected photo model data,
// the values of the batch edit form, and the per-item result.
type PhotosResponse struct {
	Models search.PhotoResults `json:"models"`
	Values *PhotosForm         `json:"values"`
	// Result carries the per-item outcome and is present when the request applied changes.
	batchresult.Result
}
