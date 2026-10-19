package workers

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/entity/query"
	"github.com/photoprism/photoprism/internal/event"
)

// jobTestPolicyFast retries quickly so scheduler behavior can be tested without long waits.
var jobTestPolicyFast = RetryPolicy{
	MaxAttempts: 3,
	Backoffs:    []time.Duration{10 * time.Millisecond, 10 * time.Millisecond},
	MaxFailures: 2,
}

// jobRunsFor loads all recorded rows of the named job, oldest first.
func jobRunsFor(t *testing.T, name string) entity.JobRuns {
	t.Helper()

	rows, err := query.JobRuns(name, 100, 0)
	require.NoError(t, err)

	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}

	return rows
}

// waitForJobStatus polls the ledger until the newest final row of the job has the given status.
func waitForJobStatus(t *testing.T, name, status string) entity.JobRun {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if rows := jobRunsFor(t, name); len(rows) > 0 {
			last := rows[len(rows)-1]
			if last.Final && last.Status == status {
				return last
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("job %s did not reach status %s", name, status)

	return entity.JobRun{}
}

func TestJobRunOutcomes(t *testing.T) {
	t.Run("Success", func(t *testing.T) {
		const name = "test-job-success"
		calls := 0
		require.NoError(t, RegisterJob(name, func() error { calls++; return nil }))

		require.NoError(t, RunJob(name, TriggerSchedule))

		assert.Equal(t, 1, calls)
		rows := jobRunsFor(t, name)
		require.Len(t, rows, 1)
		assert.Equal(t, entity.JobTriggerSchedule, rows[0].Trigger)
		assert.Equal(t, 1, rows[0].Attempt)
		assert.True(t, rows[0].Final)
		assert.Equal(t, entity.JobRunSucceeded, rows[0].Status)
		assert.NotNil(t, rows[0].FinishedAt)
		assert.Empty(t, rows[0].Error)
	})
	t.Run("Failure", func(t *testing.T) {
		const name = "test-job-failure"
		require.NoError(t, RegisterJob(name, func() error { return errors.New("test boom") }))

		err := RunJob(name, TriggerManual)
		require.Error(t, err)

		rows := jobRunsFor(t, name)
		require.Len(t, rows, 1)
		assert.Equal(t, entity.JobTriggerManual, rows[0].Trigger)
		assert.True(t, rows[0].Final)
		assert.Equal(t, entity.JobRunFailed, rows[0].Status)
		assert.Contains(t, rows[0].Error, "test boom")
		assert.NotNil(t, rows[0].FinishedAt)
	})
	t.Run("Panic", func(t *testing.T) {
		const name = "test-job-panic"
		require.NoError(t, RegisterJob(name, func() error { panic("test panic") }))

		err := RunJob(name, TriggerSchedule)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "worker panic")

		rows := jobRunsFor(t, name)
		require.Len(t, rows, 1)
		assert.Equal(t, entity.JobRunFailed, rows[0].Status)
		assert.Contains(t, rows[0].Error, "test panic")
	})
	t.Run("Skipped", func(t *testing.T) {
		const name = "test-job-skipped"
		require.NoError(t, RegisterJob(name, func() error { return ErrJobSkipped }))

		err := RunJob(name, TriggerSchedule)
		require.ErrorIs(t, err, ErrJobSkipped)

		rows := jobRunsFor(t, name)
		require.Len(t, rows, 1)
		assert.True(t, rows[0].Final)
		assert.Equal(t, entity.JobRunSkipped, rows[0].Status)
		assert.Equal(t, 0, query.JobConsecutiveFailures(name))
	})
	t.Run("Unknown", func(t *testing.T) {
		require.ErrorIs(t, RunJob("test-job-unknown", TriggerSchedule), ErrJobNotFound)
		_, err := StartJob("test-job-unknown", TriggerManual)
		require.ErrorIs(t, err, ErrJobNotFound)
	})
}

func TestJobRunRetries(t *testing.T) {
	const name = "test-job-retries"
	attempts := 0
	require.NoError(t, RegisterJob(name, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary failure")
		}
		return nil
	}, WithRetryPolicy(jobTestPolicyFast)))

	require.NoError(t, RunJob(name, TriggerSchedule))
	assert.Equal(t, 3, attempts)

	rows := jobRunsFor(t, name)
	require.Len(t, rows, 3)

	var runUID string
	for i, row := range rows {
		if i == 0 {
			runUID = row.RunUID
		} else {
			assert.Equal(t, runUID, row.RunUID, "all attempts belong to the same triggered cycle")
		}

		assert.Equal(t, i+1, row.Attempt)
		assert.Equal(t, entity.JobTriggerSchedule, row.Trigger)
		assert.NotEmpty(t, row.RunUID)

		if i < 2 {
			assert.False(t, row.Final)
			assert.Equal(t, entity.JobRunFailed, row.Status)
		} else {
			assert.True(t, row.Final)
			assert.Equal(t, entity.JobRunSucceeded, row.Status)
		}
	}

	assert.Zero(t, query.JobConsecutiveFailures(name))
}

func TestJobRunStrugglingAndRecovery(t *testing.T) {
	const name = "test-job-struggling"
	policy := RetryPolicy{MaxAttempts: 1, MaxFailures: 2}

	sub := event.Subscribe("system.log.error", "system.log.info", "system.log.warning")
	defer event.Unsubscribe(sub)

	failing := true
	require.NoError(t, RegisterJob(name, func() error {
		if failing {
			return errors.New("persistent failure")
		}
		return nil
	}, WithRetryPolicy(policy)))

	// First failed cycle stays below the threshold.
	require.Error(t, RunJob(name, TriggerSchedule))
	assert.Equal(t, 1, query.JobConsecutiveFailures(name))

	// Second consecutive failed cycle crosses it and marks the job as struggling.
	require.Error(t, RunJob(name, TriggerSchedule))
	assert.Equal(t, 2, query.JobConsecutiveFailures(name))

	// A third failure does not raise another struggling event.
	require.Error(t, RunJob(name, TriggerSchedule))
	assert.Equal(t, 3, query.JobConsecutiveFailures(name))

	// A successful manual run clears the struggling state.
	failing = false
	require.NoError(t, RunJob(name, TriggerManual))
	assert.Zero(t, query.JobConsecutiveFailures(name))

	var strugglingEvents, recoveredEvents []string

	deadline := time.After(300 * time.Millisecond)

collect:
	for {
		select {
		case msg := <-sub.Receiver:
			message, _ := msg.Fields["message"].(string)
			if strings.Contains(message, "is struggling after 2 consecutive failed runs") {
				strugglingEvents = append(strugglingEvents, message)
			} else if strings.Contains(message, "recovered after a successful run") {
				recoveredEvents = append(recoveredEvents, message)
			}
		case <-deadline:
			break collect
		}
	}

	assert.Len(t, strugglingEvents, 1)
	assert.Len(t, recoveredEvents, 1)

	var info JobInfo
	for _, job := range JobInfos() {
		if job.Name == name {
			info = job
			break
		}
	}

	require.Equal(t, name, info.Name)
	assert.False(t, info.Struggling)
	assert.Equal(t, entity.JobRunSucceeded, info.Status)
	require.NotNil(t, info.LatestRun)
	assert.Equal(t, entity.JobTriggerManual, info.LatestRun.Trigger)
}

func TestStartJobManualAndConcurrency(t *testing.T) {
	const name = "test-job-manual"
	started := make(chan struct{})
	release := make(chan struct{})

	require.NoError(t, RegisterJob(name, func() error {
		close(started)
		<-release
		return nil
	}))

	runUID, err := StartJob(name, TriggerManual)
	require.NoError(t, err)
	require.NotEmpty(t, runUID)
	<-started

	// Neither another async trigger nor a synchronous one may overlap the same job.
	_, err = StartJob(name, TriggerManual)
	require.ErrorIs(t, err, ErrJobAlreadyRunning)
	require.ErrorIs(t, RunJob(name, TriggerManual), ErrJobAlreadyRunning)

	// While running, the ledger shows one complete running row with no half-written fields.
	runningRows := jobRunsFor(t, name)
	require.Len(t, runningRows, 1)
	assert.Equal(t, runUID, runningRows[0].RunUID)
	assert.Equal(t, entity.JobRunRunning, runningRows[0].Status)
	assert.Equal(t, entity.JobTriggerManual, runningRows[0].Trigger)
	assert.Nil(t, runningRows[0].FinishedAt)
	assert.NotZero(t, runningRows[0].StartedAt)

	close(release)

	last := waitForJobStatus(t, name, entity.JobRunSucceeded)
	assert.True(t, last.Final)
	assert.Equal(t, runUID, last.RunUID)
}

func TestJobRetryInterruptedByShutdown(t *testing.T) {
	const name = "test-job-shutdown"
	attempts := 0
	shutdownPolicy := RetryPolicy{
		MaxAttempts: 3,
		Backoffs:    []time.Duration{time.Hour},
		MaxFailures: 2,
	}
	require.NoError(t, RegisterJob(name, func() error {
		attempts++
		return errors.New("failing until shutdown")
	}, WithRetryPolicy(shutdownPolicy)))

	// Closing the shutdown channel interrupts the long retry backoff without waiting.
	signalJobShutdown()
	start := time.Now()
	require.Error(t, RunJob(name, TriggerSchedule))
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 1, attempts, "no retry attempt runs after the shutdown signal")

	rows := jobRunsFor(t, name)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Final)
	assert.Equal(t, entity.JobRunFailed, rows[0].Status)
}
