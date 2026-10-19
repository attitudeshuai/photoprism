package workers

import (
	"cmp"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/entity/query"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/rnd"
)

// Trigger identifies what started a job cycle.
type Trigger string

const (
	// TriggerSchedule marks a cycle that the cron scheduler or wakeup ticker started.
	TriggerSchedule Trigger = entity.JobTriggerSchedule
	// TriggerManual marks a cycle that an administrator or an API action started.
	TriggerManual Trigger = entity.JobTriggerManual
)

var (
	// ErrJobNotFound is returned when no background job is registered under the name.
	ErrJobNotFound = errors.New("scheduled job not found")
	// ErrJobAlreadyRunning is returned when the named job is still executing a cycle.
	ErrJobAlreadyRunning = errors.New("scheduled job is already running")
	// ErrJobSkipped marks a cycle that did not run, e.g. because its worker was busy.
	ErrJobSkipped = errors.New("scheduled job skipped")
)

// RetryPolicy declares how a failing job cycle retries and when it is marked as struggling.
// The zero value keeps the historical behavior: one attempt, no retries, no struggling state.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts per triggered cycle, including the first.
	MaxAttempts int `json:"maxAttempts"`
	// Backoffs lists the wait before the next attempt; the last value is reused for further attempts.
	Backoffs []time.Duration `json:"-"`
	// MaxFailures is the number of consecutive failed cycles after which the job is marked as struggling.
	MaxFailures int `json:"maxFailures"`
}

// Attempts returns the normalized total attempts per cycle.
func (p RetryPolicy) Attempts() int {
	if p.MaxAttempts < 1 {
		return 1
	}

	return p.MaxAttempts
}

// Backoff returns the wait before the next attempt after the given zero-based attempt failed.
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	if len(p.Backoffs) == 0 {
		return 0
	}

	if attempt < 0 {
		attempt = 0
	}

	if attempt >= len(p.Backoffs) {
		return p.Backoffs[len(p.Backoffs)-1]
	}

	return p.Backoffs[attempt]
}

// BackoffSeconds returns the retry backoffs in seconds for read-only status snapshots.
func (p RetryPolicy) BackoffSeconds() []int64 {
	if len(p.Backoffs) == 0 {
		return nil
	}

	result := make([]int64, len(p.Backoffs))
	for i, d := range p.Backoffs {
		result[i] = int64(d.Round(time.Second).Seconds())
	}

	return result
}

// JobOption configures a registered background job.
type JobOption func(*registeredJob)

// WithRetryPolicy declares the retry and struggling policy of a job.
func WithRetryPolicy(policy RetryPolicy) JobOption {
	return func(j *registeredJob) {
		j.policy = policy
	}
}

// registeredJob is a named task with its policy and current execution state.
type registeredJob struct {
	name     string
	task     func() error
	policy   RetryPolicy
	schedule string
	running  atomic.Bool
}

// jobRegistry holds the registered jobs keyed by name.
var jobRegistry = struct {
	sync.RWMutex
	jobs map[string]*registeredJob
}{jobs: make(map[string]*registeredJob)}

// jobStopCh is closed on shutdown so retry backoff waits can return early.
var jobStopCh = make(chan struct{})

// jobStopOnce guards closing jobStopCh at most once.
var jobStopOnce sync.Once

// signalJobShutdown closes the broadcast channel that interrupts retry backoff waits.
func signalJobShutdown() {
	jobStopOnce.Do(func() {
		close(jobStopCh)
	})
}

// RegisterJob registers a named background task with optional retry options. A job can be
// scheduled with NewJob, started manually with StartJob, or both.
func RegisterJob(name string, task func() error, options ...JobOption) error {
	if name == "" {
		return fmt.Errorf("new job requires a name")
	}

	if task == nil {
		return fmt.Errorf("new job requires a task to run")
	}

	job := &registeredJob{name: name, task: task}

	for _, option := range options {
		if option != nil {
			option(job)
		}
	}

	jobRegistry.Lock()
	jobRegistry.jobs[name] = job
	jobRegistry.Unlock()

	return nil
}

// lookupJob returns the registered job with the given name.
func lookupJob(name string) (*registeredJob, error) {
	jobRegistry.RLock()
	job := jobRegistry.jobs[name]
	jobRegistry.RUnlock()

	if job == nil {
		return nil, fmt.Errorf("%w: %s", ErrJobNotFound, name)
	}

	return job, nil
}

// JobExists reports whether a background job is registered under the given name.
func JobExists(name string) bool {
	jobRegistry.RLock()
	defer jobRegistry.RUnlock()

	return jobRegistry.jobs[name] != nil
}

// RunJob executes one complete cycle of the named job synchronously, including ledger
// entries and configured retries. It returns ErrJobAlreadyRunning without recording
// anything when the job is busy, and the last attempt's error when the cycle failed.
func RunJob(name string, trigger Trigger) error {
	job, err := lookupJob(name)
	if err != nil {
		return err
	}

	if !job.running.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: %s", ErrJobAlreadyRunning, name)
	}

	defer job.running.Store(false)

	return job.execute(trigger, rnd.UUID())
}

// StartJob executes one complete cycle of the named job in its own goroutine. The
// returned UID identifies the new ledger cycle; scheduled and manual triggers share
// the same ledger and retry path.
func StartJob(name string, trigger Trigger) (runUID string, err error) {
	job, err := lookupJob(name)
	if err != nil {
		return "", err
	}

	if !job.running.CompareAndSwap(false, true) {
		return "", fmt.Errorf("%w: %s", ErrJobAlreadyRunning, name)
	}

	runUID = rnd.UUID()

	go func() {
		defer job.running.Store(false)
		job.execute(trigger, runUID)
	}()

	return runUID, nil
}

// execute runs all attempts of one triggered cycle and records them in the ledger.
func (j *registeredJob) execute(trigger Trigger, runUID string) error {
	var lastErr error

	for attempt := 1; attempt <= j.policy.Attempts(); attempt++ {
		run := &entity.JobRun{
			RunUID:    runUID,
			JobName:   j.name,
			Trigger:   string(trigger),
			Attempt:   attempt,
			Status:    entity.JobRunRunning,
			StartedAt: entity.Now(),
		}

		insertJobRun(run)

		start := time.Now()
		lastErr = j.runTask()

		switch {
		case errors.Is(lastErr, ErrJobSkipped):
			// A busy worker or a gate is a neutral outcome: no retry, no failure count.
			finalizeJobRun(run, start, entity.JobRunSkipped, nil, true, j.policy, false)
			log.Debugf("scheduler: %s skipped (already running)", j.name)
			return lastErr
		case lastErr == nil:
			_, recovered := finalizeJobRun(run, start, entity.JobRunSucceeded, nil, true, j.policy, true)
			if recovered {
				event.SystemInfo([]string{"scheduler", "job %s recovered after a successful run"}, j.name)
			}

			return nil
		}

		if attempt < j.policy.Attempts() {
			wait := j.policy.Backoff(attempt - 1)

			// Keep the failed attempt visible in the ledger while the retry waits.
			finalizeJobRun(run, start, entity.JobRunFailed, lastErr, false, j.policy, false)

			event.SystemWarn([]string{"scheduler", "job %s failed on attempt %d/%d: %s; retrying in %s"},
				j.name, attempt, j.policy.Attempts(), clean.ErrorFull(lastErr), wait.Round(time.Second))

			if !waitForRetry(wait) {
				// Shutdown interrupted the backoff; the failed attempt becomes the final
				// outcome, but no struggling event is raised while the instance is stopping.
				finalizeJobRun(run, start, entity.JobRunFailed, lastErr, true, j.policy, false)
				return lastErr
			}

			continue
		}

		struggling, _ := finalizeJobRun(run, start, entity.JobRunFailed, lastErr, true, j.policy, true)

		event.SystemError([]string{"scheduler", "job %s failed: %s"}, j.name, clean.ErrorFull(lastErr))

		if struggling {
			event.SystemError([]string{"scheduler", "job %s is struggling after %d consecutive failed runs"},
				j.name, j.policy.MaxFailures)
		}
	}

	return lastErr
}

// runTask invokes the task function and converts a panic into an error.
func (j *registeredJob) runTask() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s (worker panic)\nstack: %s", r, debug.Stack())
		}
	}()

	return j.task()
}

// waitForRetry waits for the backoff duration or a shutdown signal, whichever comes first.
func waitForRetry(wait time.Duration) bool {
	if wait <= 0 {
		return true
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-jobStopCh:
		return false
	case <-timer.C:
		return true
	}
}

// insertJobRun stores a newly started attempt row, ignoring ledger failures so task
// execution never depends on the database being writable.
func insertJobRun(run *entity.JobRun) {
	if db := entity.Db(); db != nil {
		if err := db.Create(run).Error; err != nil {
			log.Warnf("scheduler: %s while recording job run (%s)", err, run.JobName)
		}
	}
}

// finalizeJobRun writes the completed attempt in a single transaction and, for final
// attempts with evaluate set, evaluates the struggling transition. It reports whether
// the transition marks the job struggling or recovered from a struggling state.
func finalizeJobRun(run *entity.JobRun, start time.Time, status string, cause error, final bool, policy RetryPolicy, evaluate bool) (struggling bool, recovered bool) {
	run.Status = status
	run.Final = final
	run.DurationMs = time.Since(start).Milliseconds()

	if cause != nil {
		run.Error = clean.ClipMiddle(cause.Error(), entity.JobRunErrorBytes)
	}

	finishedAt := entity.Now()
	run.FinishedAt = &finishedAt

	db := entity.Db()
	if db == nil {
		return false, false
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(run).Error; err != nil {
			return err
		}

		if !final || !evaluate || policy.MaxFailures < 1 {
			return nil
		}

		var rows entity.JobRuns
		if err := tx.Where("job_name = ? AND final = ?", run.JobName, true).
			Order("started_at DESC, id DESC").Limit(100).Find(&rows).Error; err != nil {
			return err
		}

		failures := rows.ConsecutiveFailures()

		// Count failures of earlier cycles only, so a transition is reported once, not again
		// on every failed cycle after the threshold was reached.
		previous := make(entity.JobRuns, 0, len(rows))
		for _, row := range rows {
			if row.RunUID != run.RunUID {
				previous = append(previous, row)
			}
		}

		priorFailures := previous.ConsecutiveFailures()

		switch status {
		case entity.JobRunFailed:
			struggling = failures >= policy.MaxFailures && priorFailures < policy.MaxFailures
		case entity.JobRunSucceeded:
			recovered = priorFailures >= policy.MaxFailures
		}

		return nil
	})

	if err != nil {
		log.Warnf("scheduler: %s while finalizing job run (%s)", err, run.JobName)
	}

	return struggling, recovered
}

// JobInfo is the read-only snapshot of a registered job and its latest ledger state.
type JobInfo struct {
	Name                string         `json:"name"`
	Scheduled           bool           `json:"scheduled"`
	Schedule            string         `json:"schedule,omitempty"`
	Running             bool           `json:"running"`
	MaxAttempts         int            `json:"maxAttempts"`
	MaxFailures         int            `json:"maxFailures,omitempty"`
	Status              string         `json:"status,omitempty"`
	ConsecutiveFailures int            `json:"consecutiveFailures"`
	Struggling          bool           `json:"struggling"`
	LatestRun           *entity.JobRun `json:"latestRun,omitempty"`
	Backoffs            []int64        `json:"backoffs,omitempty"`
}

// JobInfos returns a read-only snapshot of all registered jobs, ordered by name.
func JobInfos() []JobInfo {
	jobRegistry.RLock()
	jobs := make([]*registeredJob, 0, len(jobRegistry.jobs))
	for _, job := range jobRegistry.jobs {
		jobs = append(jobs, job)
	}
	jobRegistry.RUnlock()

	slices.SortFunc(jobs, func(a, b *registeredJob) int {
		return cmp.Compare(a.name, b.name)
	})

	result := make([]JobInfo, 0, len(jobs))

	for _, job := range jobs {
		info := JobInfo{
			Name:        job.name,
			Scheduled:   job.schedule != "",
			Schedule:    job.schedule,
			Running:     job.running.Load(),
			MaxAttempts: job.policy.Attempts(),
			MaxFailures: job.policy.MaxFailures,
			Backoffs:    job.policy.BackoffSeconds(),
		}

		if latest, err := query.LatestJobRun(job.name); err != nil {
			log.Warnf("scheduler: %s while reading latest job run (%s)", err, job.name)
		} else if latest != nil {
			info.LatestRun = latest
			info.Status = latest.Status
		}

		if job.policy.MaxFailures > 0 {
			info.ConsecutiveFailures = query.JobConsecutiveFailures(job.name)
			info.Struggling = info.ConsecutiveFailures >= job.policy.MaxFailures
		}

		result = append(result, info)
	}

	return result
}
