/*
Package workers provides index, sync, and metadata optimization background workers.

Copyright (c) 2018 - 2026 PhotoPrism UG. All rights reserved.

	This program is free software: you can redistribute it and/or modify
	it under Version 3 of the GNU Affero General Public License (the "AGPL"):
	<https://docs.photoprism.app/license/agpl>

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	The AGPL is supplemented by our Trademark and Brand Guidelines,
	which describe how our Brand Assets may be used:
	<https://www.photoprism.app/trademark/>

Feel free to send an email to hello@photoprism.app if you have questions,
want to support our work, or just want to say hello.

Additional information can be found in our Developer Guide:
<https://docs.photoprism.app/developer-guide/>
*/
package workers

import (
	"path/filepath"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/photoprism/photoprism/internal/config"
	"github.com/photoprism/photoprism/internal/config/ttl"
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/internal/mutex"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/fs"
)

var log = event.Log
var stop = make(chan bool, 1)

// Retry policies of the cron jobs. Jobs without a policy keep their historical
// behavior of a single attempt without a struggling state.
var (
	backupRetryPolicy = RetryPolicy{
		MaxAttempts: 3,
		Backoffs:    []time.Duration{time.Minute, 5 * time.Minute},
		MaxFailures: 3,
	}
	indexRetryPolicy = RetryPolicy{
		MaxAttempts: 2,
		Backoffs:    []time.Duration{5 * time.Minute},
		MaxFailures: 3,
	}
)

// Start launches background workers and scheduled tasks based on the current
// configuration. It sets up the cron scheduler and the periodic metadata/share
// workers.
func Start(conf *config.Config) {
	if scheduler, err := gocron.NewScheduler(gocron.WithLocation(conf.DefaultTimezone())); err != nil {
		log.Errorf("scheduler: %s (start)", err)
		return
	} else if scheduler != nil {
		Scheduler = scheduler

		// Register backup job; it can also be triggered manually without a cron schedule.
		if err = RegisterJob("backup", NewBackup(conf).StartScheduled, WithRetryPolicy(backupRetryPolicy)); err != nil {
			log.Errorf("scheduler: %s (backup)", err)
		} else if err = NewJob("backup", conf.BackupSchedule()); err != nil {
			log.Errorf("scheduler: %s (backup)", err)
		}

		// Only portals issue JWTs, so only they need a signing key to rotate. The startup
		// check runs as well, since a portal that is never up at the scheduled hour would
		// otherwise never reach a tick.
		if conf.Portal() {
			if err = RegisterJob("jwt-keys", func() error { return RunJWTKeyRotation(conf) }, WithRetryPolicy(backupRetryPolicy)); err != nil {
				log.Errorf("scheduler: %s (jwt keys)", err)
			} else if err = NewJob("jwt-keys", JWTKeySchedule); err != nil {
				event.SystemError([]string{"scheduler", "jwt keys", "%s"}, clean.ErrorFull(err))
			}

			go event.Safe(func() { RunJWTKeyRotation(conf) })
		}

		// Only schedule index and vision jobs if this is not a portal.
		if !conf.Portal() {
			// Register indexing job.
			if err = RegisterJob("index", NewIndex(conf).StartScheduled, WithRetryPolicy(indexRetryPolicy)); err != nil {
				log.Errorf("scheduler: %s (index)", err)
			} else if err = NewJob("index", conf.IndexSchedule()); err != nil {
				log.Errorf("scheduler: %s (index)", err)
			}

			// Register vision job.
			if err = RegisterJob("vision", NewVision(conf).StartScheduled, WithRetryPolicy(indexRetryPolicy)); err != nil {
				log.Errorf("scheduler: %s (vision)", err)
			} else if err = NewJob("vision", conf.VisionSchedule()); err != nil {
				log.Errorf("scheduler: %s (vision)", err)
			}
		}

		// Start the scheduler.
		Scheduler.Start()
	}

	// Only run metadata, share & sync background workers if this is not a portal.
	if conf.Portal() {
		log.Infof("config: disabled metadata, share & sync background workers")
		return
	}

	registerMaintenanceJobs(conf)

	// Start the other background workers.
	interval := conf.WakeupInterval()

	// Other workers can be disabled in safe mode by setting the execution interval to a value < 1.
	if interval.Seconds() <= 0 {
		log.Warnf("config: disabled metadata, share & sync background workers")
		return
	}

	ticker := time.NewTicker(interval)

	go func() {
		for {
			select {
			case <-stop:
				ticker.Stop()
				mutex.MetaWorker.Cancel()
				mutex.ShareWorker.Cancel()
				mutex.SyncWorker.Cancel()
				return
			case <-ticker.C:
				StartMaintenanceJobs()
			}
		}
	}()
}

// maintenanceJobs lists the wakeup-ticker jobs in their launch order.
var maintenanceJobs = []string{"meta", "share", "sync", "purge-expired"}

// registerMaintenanceJobs registers the wakeup-ticker jobs without retry policies,
// so their scheduling and execution behavior stays unchanged while their runs are
// recorded in the job ledger and they can be triggered manually.
func registerMaintenanceJobs(conf *config.Config) {
	if err := RegisterJob("meta", func() error {
		if mutex.WorkersRunning() {
			return ErrJobSkipped
		}

		return runMetaCycle(conf)
	}); err != nil {
		log.Errorf("scheduler: %s (meta)", err)
	}

	if err := RegisterJob("share", func() error {
		if mutex.ShareWorker.Running() {
			return ErrJobSkipped
		}

		if err := NewShare(conf).Start(); err != nil {
			if mutex.ShareWorker.Running() {
				return ErrJobSkipped
			}

			return err
		}

		return nil
	}); err != nil {
		log.Errorf("scheduler: %s (share)", err)
	}

	if err := RegisterJob("sync", func() error {
		if mutex.SyncWorker.Running() {
			return ErrJobSkipped
		}

		if err := NewSync(conf).Start(); err != nil {
			if mutex.SyncWorker.Running() {
				return ErrJobSkipped
			}

			return err
		}

		return nil
	}); err != nil {
		log.Errorf("scheduler: %s (sync)", err)
	}

	if err := RegisterJob("purge-expired", func() error {
		RunPurgeExpired(conf)
		return nil
	}); err != nil {
		log.Errorf("scheduler: %s (purge expired)", err)
	}
}

// StartMaintenanceJobs starts the wakeup-ticker jobs once, each in its own goroutine.
func StartMaintenanceJobs() {
	for _, name := range maintenanceJobs {
		if _, err := StartJob(name, TriggerSchedule); err != nil {
			log.Debugf("scheduler: %s (%s)", err, name)
		}
	}
}

// Shutdown stops the background workers and shuts down the scheduler.
func Shutdown() {
	log.Info("shutting down workers")

	signalJobShutdown()

	stop <- true

	if Scheduler != nil {
		if err := Scheduler.Shutdown(); err != nil {
			log.Warnf("scheduler: %s (shutdown)", err)
		}
	}
}

// RunMeta runs the metadata worker once.
func RunMeta(conf *config.Config) {
	if !mutex.WorkersRunning() {
		go func() {
			if err := runMetaCycle(conf); err != nil {
				log.Warnf("metadata: %s", err)
			}
		}()
	}
}

// runMetaCycle performs one metadata maintenance cycle.
func runMetaCycle(conf *config.Config) error {
	worker := NewMeta(conf)

	delay := time.Minute
	interval := entity.MetadataUpdateInterval

	return worker.Start(delay, interval, false)
}

// RunShare runs the share worker once.
func RunShare(conf *config.Config) {
	if !mutex.ShareWorker.Running() {
		go func() {
			worker := NewShare(conf)
			if err := worker.Start(); err != nil {
				log.Warnf("share: %s", err)
			}
		}()
	}
}

// RunPurgeArchives removes expired download archives from the temp directory once.
// It returns without disk access while mutex.TempArchives is clear, so an idle instance never wakes up
// sleeping storage, and clears the flag before scanning so a concurrent creation that re-arms it during
// the sweep is not overwritten.
func RunPurgeArchives(conf *config.Config) {
	if !mutex.TempArchives.Load() {
		return
	}

	mutex.TempArchives.Store(false)

	maxAge := time.Duration(ttl.DownloadArchiveAge.Int()) * time.Second
	removed, remaining, failed := fs.PurgeExpired(filepath.Join(conf.TempPath(), fs.ZipDir), fs.ExtZip, maxAge)

	// Keep scanning while archives are left, e.g. because they have not expired yet.
	if remaining > 0 {
		mutex.TempArchives.Store(true)
	}

	if removed > 0 || failed > 0 {
		log.Debugf("download: removed %d expired archives, %d could not be deleted", removed, failed)
	}
}

// RunSync runs the sync worker once.
func RunSync(conf *config.Config) {
	if !mutex.SyncWorker.Running() {
		go func() {
			worker := NewSync(conf)
			if err := worker.Start(); err != nil {
				log.Warnf("sync: %s", err)
			}
		}()
	}
}
