package migrate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/photoprism/photoprism/internal/event"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/dsn"
	"github.com/photoprism/photoprism/pkg/list"
	"github.com/photoprism/photoprism/pkg/txt"

	"github.com/dustin/go-humanize/english"
	"gorm.io/gorm"
)

// Migrations represents a sorted list of migrations.
type Migrations []Migration

// MigrationMap represents a map of migrations.
type MigrationMap map[string]Migration

// Existing finds and returns previously executed database schema migrations.
func Existing(db *gorm.DB, stage string) MigrationMap {
	var err error

	if db == nil {
		return make(MigrationMap)
	}

	// Get SQL dialect name.
	name := dsn.ParseDriver(db.Name())

	if name == dsn.DriverNone {
		return make(MigrationMap)
	}

	// Make sure a "migrations" table exists.
	once[name].Do(func() {
		err = db.AutoMigrate(&Migration{})
	})

	if err != nil {
		return make(MigrationMap)
	}

	found := Migrations{}

	stmt := db

	if stage == StageMain {
		stmt = stmt.Where("stage = ? OR stage = '' OR stage IS NULL", stage)
	} else if stage != "" {
		stmt = stmt.Where("stage = ?", stage)
	}

	if err = stmt.Find(&found).Error; err != nil {
		log.Warnf("migrate: %s (find existing)", err)
		return make(MigrationMap)
	}

	result := make(MigrationMap, len(found))

	for _, m := range found {
		result[m.ID] = m
	}

	return result
}

// Start runs all migrations that haven't been executed yet. It returns ErrInterrupted when previously
// interrupted migrations block startup, so callers can refuse to work with a partially migrated schema.
func (m *Migrations) Start(db *gorm.DB, opt Options) error {
	if db == nil {
		return nil
	}

	// Find previously executed migrations.
	executed := Existing(db, opt.StageName())

	// Log information about existing migrations.
	if prev := len(executed); prev > 0 {
		stage := fmt.Sprintf("previously executed %s stage", opt.StageName())
		log.Tracef("migrate: found %s", english.Plural(len(executed), stage+" migration", stage+" migrations"))
	}

	// Registered migrations of this stage, by ID.
	registered := make(map[string]Migration, len(*m))

	for _, migration := range *m {
		if !migration.Skip(opt) {
			registered[migration.ID] = migration
		}
	}

	// IDs authorized for an immediate rerun after an interruption.
	resume := make(map[string]bool)

	// Detect migrations left in the running state by processes that ended.
	if notes := m.checkInterrupted(db, opt, executed, registered, resume); len(notes) > 0 {
		return fmt.Errorf("%w: %s", ErrInterrupted, strings.Join(notes, "; "))
	}

	// Run migrations.
	for _, migration := range *m {
		if migration.Skip(opt) {
			continue
		}

		start := time.Now()

		// Excluded?
		if list.Excludes(opt.Migrations, migration.ID) {
			log.Tracef("migrate: %s skipped", migration.ID)
			continue
		}

		forced := list.Contains(opt.Migrations, migration.ID)

		// Already executed?
		done, ok := executed[migration.ID]
		if ok {
			// Repeat?
			if !forced && !resume[migration.ID] && !done.Repeat(opt.RunFailed) {
				log.Debugf("migrate: %s skipped", migration.ID)
				continue
			}

			// Reset failed, finished, and interrupted rows to the same running state before executing.
			if err := migration.Begin(db); err != nil {
				log.Warnf("migrate: restarting %s failed with %s [%s]", migration.ID, err, time.Since(start))
				continue
			}
		} else {
			migration.StartedAt = start.UTC().Truncate(time.Second)
			migration.Status = StatusRunning

			if err := db.Create(&migration).Error; err != nil {
				// Should not happen.
				log.Warnf("migrate: creating %s failed with %s [%s]", migration.ID, err, time.Since(start))
				continue
			}
		}

		// Run migration.
		if err := migration.Execute(db); err != nil {
			migration.Fail(err, db)
			event.SystemError([]string{"migrate", "executing %s failed with %s [%s]"}, migration.ID, clean.ErrorFull(err), time.Since(start))
		} else if err = migration.Finish(db); err != nil {
			log.Warnf("migrate: updating %s failed with %s [%s]", migration.ID, err, time.Since(start))
		} else {
			log.Infof("migrate: %s successful [%s]", migration.ID, time.Since(start))
		}
	}

	return nil
}

// checkInterrupted applies the resume policy to rows still in the running state. It records the IDs
// that are safe to rerun in resume and returns the notes of migrations that block startup instead.
func (m *Migrations) checkInterrupted(db *gorm.DB, opt Options, executed MigrationMap, registered map[string]Migration, resume map[string]bool) []string {
	var blocked []string

	ids := make([]string, 0, len(executed))

	for id := range executed {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	for _, id := range ids {
		done := executed[id]

		if !done.Running() {
			continue
		}

		// A targeted run decides only about explicitly named migrations; other interrupted
		// rows are left for the normal startup sweep, so they cannot block the escape hatch.
		if len(opt.Migrations) > 0 && !list.Contains(opt.Migrations, id) {
			continue
		}

		// Named migrations are continued on demand at any time, regardless of their age or note.
		if list.Contains(opt.Migrations, id) {
			if _, ok := registered[id]; ok {
				resume[id] = true
			}

			continue
		}

		reg, known := registered[id]
		safe := known && reg.Reentrant()

		// Rows already marked as interrupted keep blocking startup until the policy allows
		// their automatic resume or an operator names the migration explicitly.
		if done.Interrupted() {
			if opt.Resume && safe {
				m.resumeRunning(id, resume)
			} else {
				blocked = append(blocked, m.blockInterrupted(db, done, reg, known))
			}

			continue
		}

		// A recent running row may belong to another process that is still alive.
		if !done.Stale() {
			log.Warnf("migrate: %s has been recorded as running since %s and is skipped; if no other instance is running it, continue it with \"photoprism migrations run %s\"",
				id, done.StartedAt.Format("2006-01-02 15:04:05"), id)
			continue
		}

		// The process that started the migration must have ended. Rerun only migrations that are
		// known to converge when replayed; record the others for manual continuation.
		if opt.Resume && safe {
			m.resumeRunning(id, resume)
		} else {
			blocked = append(blocked, m.blockInterrupted(db, done, reg, known))
		}
	}

	return blocked
}

// resumeRunning marks a running migration for an immediate rerun in the main execution loop.
func (m *Migrations) resumeRunning(id string, resume map[string]bool) {
	resume[id] = true
	log.Warnf("migrate: %s was interrupted while running and is safe to repeat, resuming it automatically", id)
}

// blockInterrupted records the details an operator needs to continue a migration by hand and
// returns the note that blocks startup.
func (m *Migrations) blockInterrupted(db *gorm.DB, done Migration, reg Migration, known bool) string {
	var note string

	if known {
		note = fmt.Sprintf("%s (started at %s, %d/%d statements); inspect the schema and run \"photoprism migrations run %s\" to continue",
			interruptedNote, done.StartedAt.Format("2006-01-02 15:04:05"), done.Progress, len(reg.Statements), done.ID)
	} else {
		note = fmt.Sprintf("%s (started at %s); it is no longer part of this release, inspect the schema and run \"photoprism migrations run %s\" to continue",
			interruptedNote, done.StartedAt.Format("2006-01-02 15:04:05"), done.ID)
	}

	if err := done.Block(db, note); err != nil {
		log.Warnf("migrate: %s (record interrupted state)", err)
	}

	if !known {
		log.Errorf("migrate: %s was interrupted while running and is no longer part of this release; %s", done.ID, note)
		return note
	}

	if reg.Reentrant() {
		log.Errorf("migrate: %s was interrupted after %d of %d statement(s); enable automatic resume or run \"photoprism migrations run %s\" to continue: %s",
			done.ID, done.Progress, len(reg.Statements), done.ID, note)
	} else {
		log.Errorf("migrate: %s was interrupted after %d of %d statement(s) and is not safe to repeat automatically; verify the schema and run \"photoprism migrations run %s\" to continue: %s",
			done.ID, done.Progress, len(reg.Statements), done.ID, note)
	}

	if done.Progress > 0 && done.Progress <= len(reg.Statements) {
		log.Debugf("migrate: %s last completed statement: %s", done.ID, txt.Clip(reg.Statements[done.Progress-1], 160))
	}

	return note
}
