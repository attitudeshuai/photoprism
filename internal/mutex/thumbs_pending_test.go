package mutex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThumbsPendingWorker_IndependentOfIndexWorker(t *testing.T) {
	// A targeted thumbnail retry may run while an index operation is in progress.
	require.NoError(t, IndexWorker.Start())
	defer IndexWorker.Stop()

	require.NoError(t, ThumbsPendingWorker.Start())
	defer ThumbsPendingWorker.Stop()

	assert.True(t, IndexWorker.Running())
	assert.True(t, ThumbsPendingWorker.Running())

	// Starting the same activity twice is still rejected.
	assert.Error(t, ThumbsPendingWorker.Start())
}

func TestThumbsPendingWorker_Cancel(t *testing.T) {
	require.NoError(t, ThumbsPendingWorker.Start())
	ThumbsPendingWorker.Cancel()
	assert.True(t, ThumbsPendingWorker.Canceled())

	// CancelAll also cancels the pending retry activity.
	require.NoError(t, ThumbsPendingWorker.Start())
	CancelAll()
	assert.True(t, ThumbsPendingWorker.Canceled())
	ThumbsPendingWorker.Stop()
}
