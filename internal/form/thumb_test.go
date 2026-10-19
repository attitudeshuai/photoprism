package form

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThumbStatusSearch_Normalize(t *testing.T) {
	t.Run("Defaults", func(t *testing.T) {
		f := ThumbStatusSearch{}
		require.NoError(t, f.Normalize())
		assert.Empty(t, f.Status)
		assert.Equal(t, ThumbStatusDefaultCount, f.Count)
		assert.Equal(t, 0, f.Offset)
	})

	t.Run("CapsCount", func(t *testing.T) {
		f := ThumbStatusSearch{Count: ThumbStatusMaxCount * 10}
		require.NoError(t, f.Normalize())
		assert.Equal(t, ThumbStatusMaxCount, f.Count)
	})

	t.Run("KeepsExplicitValues", func(t *testing.T) {
		f := ThumbStatusSearch{Status: thumbStatusSkipped, Count: 50, Offset: 200}
		require.NoError(t, f.Normalize())
		assert.Equal(t, thumbStatusSkipped, f.Status)
		assert.Equal(t, 50, f.Count)
		assert.Equal(t, 200, f.Offset)
	})

	t.Run("RejectsInvalidStatus", func(t *testing.T) {
		assert.Error(t, (&ThumbStatusSearch{Status: "bogus"}).Normalize())
	})

	t.Run("RejectsNegativePagination", func(t *testing.T) {
		assert.Error(t, (&ThumbStatusSearch{Count: -1}).Normalize())
		assert.Error(t, (&ThumbStatusSearch{Offset: -5}).Normalize())
	})
}
