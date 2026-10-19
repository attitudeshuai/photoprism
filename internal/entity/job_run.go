package entity

import "time"

// JobRunErrorBytes is the storage limit of the error column in bytes,
// matching its VARBINARY(2048) definition. Failure messages may contain
// multi-byte text such as recovered-panic stack traces, so writes are clipped
// in the middle by bytes, without splitting a character, so the cause at the
// end stays.
const JobRunErrorBytes = 2048

const (
	// JobTriggerSchedule marks a run that the cron scheduler or wakeup ticker started.
	JobTriggerSchedule = "schedule"
	// JobTriggerManual marks a run that an administrator or an API action started.
	JobTriggerManual = "manual"
)

const (
	// JobRunRunning marks an attempt that is currently in progress.
	JobRunRunning = "running"
	// JobRunSucceeded marks a completed attempt without an error.
	JobRunSucceeded = "succeeded"
	// JobRunFailed marks an attempt that returned an error or panicked.
	JobRunFailed = "failed"
	// JobRunSkipped marks a cycle that did not run, e.g. because the worker was busy.
	JobRunSkipped = "skipped"
)

// JobRun records a single attempt of a registered background job. Rows that
// share the same RunUID belong to the same triggered cycle, with one row per
// retry attempt and the last one flagged as Final.
type JobRun struct {
	ID         uint       `gorm:"primaryKey" json:"ID" yaml:"ID"`
	RunUID     string     `gorm:"size:64;index" json:"UID" yaml:"UID"`
	JobName    string     `gorm:"size:64;index:idx_job_runs_name_time,priority:1;index:idx_job_runs_final,priority:1" json:"Job" yaml:"Job"`
	Trigger    string     `gorm:"type:bytes;size:16" json:"Trigger" yaml:"Trigger"`
	Attempt    int        `json:"Attempt" yaml:"Attempt"`
	Final      bool       `gorm:"index:idx_job_runs_final,priority:2" json:"Final" yaml:"Final"`
	Status     string     `gorm:"type:bytes;size:16" json:"Status" yaml:"Status"`
	Error      string     `gorm:"column:error_message;type:bytes;size:2048" json:"Error,omitempty" yaml:"Error,omitempty"`
	StartedAt  time.Time  `gorm:"index:idx_job_runs_name_time,priority:2;index:idx_job_runs_final,priority:3" json:"StartedAt" yaml:"StartedAt"`
	FinishedAt *time.Time `gorm:"type:datetime" json:"FinishedAt,omitempty" yaml:"FinishedAt,omitempty"`
	DurationMs int64      `json:"DurationMs" yaml:"DurationMs"`
}

// JobRuns represents a list of background job runs.
type JobRuns []JobRun

// TableName returns the entity table name.
func (JobRun) TableName() string {
	return "job_runs"
}

// ConsecutiveFailures counts the consecutive failed cycles at the beginning of a
// newest-first list of final attempts. Skipped cycles are neutral, and a succeeded
// cycle ends the count.
func (runs JobRuns) ConsecutiveFailures() (count int) {
	for _, run := range runs {
		switch run.Status {
		case JobRunFailed:
			count++
		case JobRunSucceeded:
			return count
		}
	}

	return count
}
