package workers

import (
	"errors"
	"fmt"

	"github.com/go-co-op/gocron/v2"
)

var (
	// Jobs keeps the scheduled background jobs keyed by name.
	Jobs map[string]gocron.Job
	// Scheduler is the shared job scheduler instance.
	Scheduler gocron.Scheduler
)

func init() {
	Jobs = make(map[string]gocron.Job)
}

// NewJob schedules the job registered under name with the given cron expression,
// see https://pkg.go.dev/github.com/go-co-op/gocron/v2. An empty schedule creates no
// cron job, while the registered task stays available for manual runs.
func NewJob(name, schedule string) error {
	if name == "" {
		return fmt.Errorf("new job requires a name")
	}

	if schedule == "" {
		return nil
	}

	if _, err := lookupJob(name); err != nil {
		return err
	}

	job, err := Scheduler.NewJob(
		gocron.CronJob(
			schedule,
			false,
		),
		gocron.NewTask(
			func() {
				if err := RunJob(name, TriggerSchedule); err != nil &&
					!errors.Is(err, ErrJobAlreadyRunning) && !errors.Is(err, ErrJobSkipped) {
					log.Debugf("scheduler: %s (%s)", err, name)
				}
			},
		),
		gocron.WithSingletonMode(gocron.LimitModeWait),
	)

	if err != nil {
		return err
	}

	Jobs[name] = job

	markJobScheduled(name, schedule)

	return nil
}

// markJobScheduled records the cron expression of the scheduled job for status snapshots.
func markJobScheduled(name, schedule string) {
	jobRegistry.Lock()
	defer jobRegistry.Unlock()

	if job := jobRegistry.jobs[name]; job != nil {
		job.schedule = schedule
	}
}
