package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/photoprism/photoprism/internal/entity"
)

// createJobRun inserts one final job run row at the given age and returns it.
func createJobRun(t *testing.T, name, runUID string, attempt int, final bool, status string, age time.Duration) entity.JobRun {
	t.Helper()

	started := entity.Now().Add(-age)
	finished := started.Add(time.Second)
	run := entity.JobRun{
		RunUID:     runUID,
		JobName:    name,
		Trigger:    entity.JobTriggerSchedule,
		Attempt:    attempt,
		Final:      final,
		Status:     status,
		StartedAt:  started,
		FinishedAt: &finished,
		DurationMs: 1000,
	}
	if status == entity.JobRunFailed {
		run.Error = "test failure"
	}

	require.NoError(t, Db().Create(&run).Error)

	return run
}

func TestJobRunsNotFound(t *testing.T) {
	const name = "test-query-job-empty"

	rows, err := JobRuns(name, 100, 0)
	require.NoError(t, err)
	assert.Empty(t, rows)

	latest, err := LatestJobRun(name)
	require.NoError(t, err)
	assert.Nil(t, latest)

	assert.Zero(t, JobConsecutiveFailures(name))
}

func TestJobRunsHistory(t *testing.T) {
	const name = "test-query-job-history"

	t.Cleanup(func() { _ = UnscopedDb().Where("job_name = ?", name).Delete(&entity.JobRun{}).Error })

	// Failed cycle, a retried cycle whose second attempt failed, a skipped cycle,
	// and a recovered cycle, oldest to newest.
	createJobRun(t, name, "run-1", 1, true, entity.JobRunFailed, 4*time.Minute)
	createJobRun(t, name, "run-2", 1, false, entity.JobRunFailed, 3*time.Minute)
	createJobRun(t, name, "run-2", 2, true, entity.JobRunFailed, 3*time.Minute+time.Second)
	createJobRun(t, name, "run-3", 1, true, entity.JobRunSkipped, 2*time.Minute)
	createJobRun(t, name, "run-4", 1, true, entity.JobRunSucceeded, time.Minute)

	rows, err := JobRuns(name, 2, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "run-4", rows[0].RunUID)
	assert.Equal(t, "run-3", rows[1].RunUID)

	latest, err := LatestJobRun(name)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, "run-4", latest.RunUID)

	assert.Zero(t, JobConsecutiveFailures(name), "the successful cycle ends the failure count")

	// Offset skips the newest cycles.
	older, err := JobRuns(name, 1, 2)
	require.NoError(t, err)
	require.Len(t, older, 1)
	assert.Equal(t, "run-2", older[0].RunUID)
}

func TestJobConsecutiveFailures(t *testing.T) {
	const name = "test-query-job-failures"

	t.Cleanup(func() { _ = UnscopedDb().Where("job_name = ?", name).Delete(&entity.JobRun{}).Error })

	// Two failures, a neutral skipped cycle, and another failure count consecutively.
	createJobRun(t, name, "f-1", 1, true, entity.JobRunFailed, 3*time.Minute)
	createJobRun(t, name, "f-2", 1, true, entity.JobRunFailed, 2*time.Minute)
	createJobRun(t, name, "s-1", 1, true, entity.JobRunSkipped, time.Minute)
	createJobRun(t, name, "f-3", 1, true, entity.JobRunFailed, 30*time.Second)

	assert.Equal(t, 3, JobConsecutiveFailures(name))
}
