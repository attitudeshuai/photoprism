package workers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/config"
)

func TestNewJob(t *testing.T) {
	t.Run("MissingName", func(t *testing.T) {
		err := NewJob("", config.DefaultBackupSchedule)
		require.Error(t, err)
	})
	t.Run("EmptySchedule", func(t *testing.T) {
		// An empty schedule neither requires a registered task nor a scheduler.
		assert.NoError(t, NewJob("test", ""))
	})
	t.Run("UnknownJob", func(t *testing.T) {
		err := NewJob("test-unknown", config.DefaultBackupSchedule)
		require.ErrorIs(t, err, ErrJobNotFound)
	})
}

func TestRegisterJob(t *testing.T) {
	t.Run("MissingName", func(t *testing.T) {
		require.Error(t, RegisterJob("", func() error { return nil }))
	})
	t.Run("MissingTask", func(t *testing.T) {
		require.Error(t, RegisterJob("test-no-task", nil))
	})
	t.Run("Registered", func(t *testing.T) {
		require.NoError(t, RegisterJob("test-registered", func() error { return nil }))
		assert.True(t, JobExists("test-registered"))
	})
}
