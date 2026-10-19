package migrate

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/photoprism/photoprism/pkg/txt"
)

// Migration run states stored in the Status column.
const (
	StatusRunning  = "running"
	StatusFinished = "finished"
	StatusFailed   = "failed"
)

// RunningStaleAfter is the time after which a migration that is still marked as
// running is considered interrupted rather than executed by another process.
const RunningStaleAfter = time.Hour

// interruptedNote is the marker stored in Note when an interrupted migration blocks startup.
const interruptedNote = "interrupted while running"

// Migration represents a database schema migration.
type Migration struct {
	ID         string     `gorm:"size:16;primaryKey;autoIncrement:false;" json:"ID" yaml:"ID"`
	Dialect    string     `gorm:"size:16;" json:"Dialect" yaml:"Dialect,omitempty"`
	Stage      string     `gorm:"size:16;" json:"Stage" yaml:"Stage,omitempty"`
	Status     string     `gorm:"size:16;" json:"Status" yaml:"Status,omitempty"`
	Note       string     `gorm:"size:255;" json:"Note" yaml:"Note,omitempty"`
	Error      string     `gorm:"size:255;" json:"Error" yaml:"Error,omitempty"`
	Source     string     `gorm:"size:16;" json:"Source" yaml:"Source,omitempty"`
	Progress   int        `json:"Progress" yaml:"Progress,omitempty"`
	Statements []string   `gorm:"-" json:"Statements" yaml:"Statements,omitempty"`
	StartedAt  time.Time  `json:"StartedAt" yaml:"StartedAt,omitempty"`
	FinishedAt *time.Time `json:"FinishedAt" yaml:"FinishedAt,omitempty"`
}

// TableName returns the entity database table name.
func (Migration) TableName() string {
	return "migrations"
}

// StageName returns the stage name.
func (m *Migration) StageName() string {
	if m.Stage == "" {
		return StageMain
	} else {
		return m.Stage
	}
}

// RunStage checks if the run stage matches.
func (m *Migration) RunStage(name string) bool {
	return m.StageName() == name
}

// Skip checks if the migration should be skipped based on the options.
func (m *Migration) Skip(opt Options) bool {
	return !m.RunStage(opt.StageName())
}

// Finished tests if the migration has been finished yet.
func (m *Migration) Finished() bool {
	if m.FinishedAt == nil {
		return false
	}

	return !m.FinishedAt.IsZero()
}

// State returns the recorded run state, inferring it from the timestamps of rows
// written before the Status column existed.
func (m *Migration) State() string {
	if m.Status != "" {
		return m.Status
	} else if m.Finished() {
		return StatusFinished
	} else if m.Error != "" {
		return StatusFailed
	} else if !m.StartedAt.IsZero() {
		return StatusRunning
	}

	return ""
}

// Running tests if the migration is recorded as in progress.
func (m *Migration) Running() bool {
	return m.State() == StatusRunning
}

// Interrupted tests if a running migration was previously found to be interrupted.
func (m *Migration) Interrupted() bool {
	return m.Running() && m.Note != ""
}

// Stale tests if a running record is old enough that the process executing it must have ended.
func (m *Migration) Stale() bool {
	if !m.Running() || m.StartedAt.IsZero() {
		return false
	}

	return time.Now().UTC().Sub(m.StartedAt.UTC()) >= RunningStaleAfter
}

// RunDuration returns the run duration of started migrations.
func (m *Migration) RunDuration() time.Duration {
	if m.Error != "" || m.StartedAt.IsZero() {
		return time.Duration(0)
	}

	if m.Finished() {
		return m.FinishedAt.UTC().Sub(m.StartedAt.UTC())
	}

	return time.Now().UTC().Sub(m.StartedAt.UTC())
}

// Repeat tests if the migration should be repeated.
func (m *Migration) Repeat(runFailed bool) bool {
	if runFailed && m.Error != "" {
		return true
	}

	// Don't repeat if finished.
	if m.Finished() {
		return false
	}

	// Repeat not started yet.
	if m.StartedAt.IsZero() {
		return true
	}

	// Repeat if "running" for more than 60 minutes.
	return m.RunDuration().Minutes() >= 60
}

// Begin records that an execution attempt has started, resetting a row from a
// previous failed, finished, or interrupted attempt so that every rerun shares
// the same state transitions.
func (m *Migration) Begin(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	started := time.Now().UTC().Truncate(time.Second)
	m.StartedAt = started
	m.FinishedAt = nil
	m.Status = StatusRunning
	m.Note = ""
	m.Error = ""
	m.Progress = 0

	return db.Model(m).Updates(Map{
		"started_at":  started,
		"finished_at": nil,
		"status":      StatusRunning,
		"note":        "",
		"error":       "",
		"progress":    0,
	}).Error
}

// Fail marks the migration as failed by adding an error message and removing the FinishedAt timestamp.
func (m *Migration) Fail(err error, db *gorm.DB) {
	if err == nil || db == nil {
		return
	}

	m.FinishedAt = nil
	m.Status = StatusFailed
	m.Note = ""

	if err.Error() == "" {
		m.Error = "unknown error"
	} else {
		m.Error = err.Error()
	}

	db.Model(m).Updates(Map{"finished_at": m.FinishedAt, "status": m.Status, "note": "", "error": m.Error})
}

// Finish updates the FinishedAt timestamp and removes the error message when the migration was successful.
func (m *Migration) Finish(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	finished := time.Now().UTC().Truncate(time.Second)
	m.FinishedAt = &finished
	m.Status = StatusFinished
	m.Note = ""
	m.Error = ""
	m.Progress = len(m.Statements)
	return db.Model(m).Updates(Map{
		"finished_at": m.FinishedAt,
		"status":      m.Status,
		"note":        "",
		"error":       "",
		"progress":    m.Progress,
	}).Error
}

// Block marks a running migration as interrupted with a note for manual continuation, without
// losing the running state that keeps other starts from executing it.
func (m *Migration) Block(db *gorm.DB, note string) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	m.Status = StatusRunning
	m.FinishedAt = nil
	m.Note = txt.Clip(note, 255)

	return db.Model(m).Updates(Map{"status": m.Status, "finished_at": nil, "note": m.Note}).Error
}

// Execute runs the migration.
func (m *Migration) Execute(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	for i, s := range m.Statements {
		if err := db.Exec(s).Error; err != nil {
			// Log the errors triggered by ALTER and DROP statements
			// and otherwise ignore them, since some databases do not
			// support "IF EXISTS".
			if IgnoreErr.Matches(s, err.Error()) {
				log.Tracef("migrate: ignored %s", err)
			} else {
				return err
			}
		}

		// Record how many statements completed, so an interrupted migration
		// can leave a usable trail for manual continuation.
		if err := db.Model(m).Update("progress", i+1).Error; err != nil {
			log.Tracef("migrate: %s (update progress)", err)
		}
	}

	return nil
}

var (
	reLineComment   = regexp.MustCompile(`--[^\n]*`)
	reSpace         = regexp.MustCompile(`\s+`)
	reCreateGuarded = regexp.MustCompile(`^create\s+(?:or\s+replace\s+)?(?:unique\s+|fulltext\s+|spatial\s+)?(?:table|index|key)\b`)
	reDropGuarded   = regexp.MustCompile(`^drop\s+(?:table|index|key)\b`)
	reAlterAddDrop  = regexp.MustCompile(`\s(?:add|drop)\s+(?:column|index|key|unique\s+(?:index|key)|foreign\s+key)\b`)
	reInsertGuarded = regexp.MustCompile(`^insert\s+(?:ignore\s+|or\s+ignore\s+|.*\bon\s+duplicate\s+key\b|.*\bwhere\s+not\s+exists\b)`)
)

// Reentrant reports whether every statement is safe to execute again after the
// process was interrupted while the migration was running.
func (m *Migration) Reentrant() bool {
	for _, s := range m.Statements {
		if !reentrantStatement(s) {
			return false
		}
	}

	return true
}

// reentrantStatement classifies a single statement. Unknown shapes are treated as
// not reentrant, since falsely rerunning them may change data a second time.
func reentrantStatement(s string) bool {
	s = normalizeSQL(s)

	if s == "" {
		return false
	}

	switch {
	case strings.HasPrefix(s, "create "):
		// CREATE ... IF NOT EXISTS and MariaDB CREATE OR REPLACE INDEX converge to the same schema.
		return reCreateGuarded.MatchString(s) &&
			(strings.Contains(s, " if not exists") || strings.Contains(s, "create or replace"))
	case strings.HasPrefix(s, "drop "):
		// DROP TABLE/INDEX IF EXISTS is a no-op when the object is gone.
		return reDropGuarded.MatchString(s) && strings.Contains(s, " if exists")
	case strings.HasPrefix(s, "rename "):
		// RENAME fails against the missing source on replay, and Execute ignores that error.
		return true
	case strings.HasPrefix(s, "alter "):
		switch {
		case strings.Contains(s, " if exists"), strings.Contains(s, " if not exists"):
			return true
		case strings.Contains(s, " modify"), strings.Contains(s, " change"), strings.Contains(s, " rename"):
			// Reapplying the same definition or replaying a rename converges; the expected
			// duplicate-object and missing-object errors are ignored by Execute.
			return true
		case reAlterAddDrop.MatchString(s):
			return true
		}

		return false
	case strings.HasPrefix(s, "update "):
		// Guarded updates only touch rows that still match the pre-migration state.
		return strings.Contains(s, " where ")
	case strings.HasPrefix(s, "delete "):
		// A guarded delete removes nothing on replay; an unguarded one may delete new data.
		return strings.Contains(s, " where ")
	case strings.HasPrefix(s, "insert "):
		return reInsertGuarded.MatchString(s)
	}

	return false
}

// normalizeSQL strips comments and collapses whitespace, and lowercases the
// statement for syntactic reentrancy checks.
func normalizeSQL(s string) string {
	s = reLineComment.ReplaceAllString(s, " ")
	s = reSpace.ReplaceAllString(strings.TrimSpace(s), " ")
	return strings.ToLower(s)
}
