package batchresult

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBuilder(t *testing.T) {
	t.Run("DeduplicatesAndKeepsOrder", func(t *testing.T) {
		b := NewBuilder("a", "", "b", "a", "c")
		assert.Equal(t, []string{"a", "b", "c"}, b.IDs())
	})
	t.Run("Empty", func(t *testing.T) {
		r := NewBuilder().Result()
		assert.Equal(t, StatusSuccess, r.Status)
		assert.Nil(t, r.Items)
	})
}

func TestBuilderResult(t *testing.T) {
	t.Run("AllSucceeded", func(t *testing.T) {
		b := NewBuilder("a", "b")
		b.Success("a")
		b.Success("b")

		r := b.Result()
		require.Equal(t, StatusSuccess, r.Status)
		assert.Equal(t, 2, r.SuccessCount)
		assert.Equal(t, 0, r.FailCount)
		require.Len(t, r.Items, 2)
		assert.Equal(t, "a", r.Items[0].ID)
		assert.True(t, r.Items[0].OK)
		assert.Empty(t, r.Items[0].Error)
		assert.Equal(t, []string{"a", "b"}, b.Succeeded())
	})
	t.Run("Partial", func(t *testing.T) {
		b := NewBuilder("a", "b", "c")
		b.Success("a")
		b.Failure("b", errors.New("boom"))

		r := b.Result()
		assert.Equal(t, StatusPartial, r.Status)
		assert.Equal(t, 1, r.SuccessCount)
		assert.Equal(t, 2, r.FailCount)
		require.Len(t, r.Items, 3)
		assert.False(t, r.Items[1].OK)
		assert.Equal(t, "boom", r.Items[1].Error)
		// Pending items are never reported as successful.
		assert.Equal(t, "c", r.Items[2].ID)
		assert.False(t, r.Items[2].OK)
		assert.Equal(t, ErrNotFound.Error(), r.Items[2].Error)
	})
	t.Run("AllFailed", func(t *testing.T) {
		b := NewBuilder("a", "b")
		b.Failure("a", errors.New("x"))
		b.Failure("b", errors.New("y"))

		r := b.Result()
		assert.Equal(t, StatusFailed, r.Status)
		assert.Equal(t, 0, r.SuccessCount)
		assert.Equal(t, 2, r.FailCount)
		assert.True(t, b.HasFailures())
	})
	t.Run("StopMarksRemaining", func(t *testing.T) {
		b := NewBuilder("a", "b", "c")
		b.Success("a")
		b.Failure("b", errors.New("write failed"))
		b.Stop()

		r := b.Result()
		assert.True(t, r.Stopped)
		assert.Equal(t, StatusPartial, r.Status)
		require.Len(t, r.Items, 3)
		assert.True(t, r.Items[0].OK)
		assert.False(t, r.Items[1].OK)
		assert.Equal(t, "write failed", r.Items[1].Error)
		assert.False(t, r.Items[2].OK)
		assert.Equal(t, ErrStopped.Error(), r.Items[2].Error)
	})
	t.Run("FailFastAllFailed", func(t *testing.T) {
		b := NewBuilder("a", "b")
		b.Failure("a", errors.New("write failed"))
		b.Stop()

		r := b.Result()
		assert.True(t, r.Stopped)
		assert.Equal(t, StatusFailed, r.Status)
	})
	t.Run("ImplicitSuccesses", func(t *testing.T) {
		b := NewBuilder()
		b.AddSuccess(3)
		b.Failure("x", errors.New("gone"))

		r := b.Result()
		assert.Equal(t, StatusPartial, r.Status)
		assert.Equal(t, 3, r.SuccessCount)
		assert.Equal(t, 1, r.FailCount)
		require.Len(t, r.Items, 1)
		assert.Equal(t, "x", r.Items[0].ID)
	})
	t.Run("LateRegistration", func(t *testing.T) {
		b := NewBuilder("a")
		b.Success("a")
		b.Failure("b", errors.New("late"))

		r := b.Result()
		assert.Equal(t, StatusPartial, r.Status)
		require.Len(t, r.Items, 2)
		assert.Equal(t, "b", r.Items[1].ID)
	})
}
