package migrate

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/photoprism/photoprism/internal/event"
)

// captureSystemLog redirects the system log to a buffer until the test ends.
func captureSystemLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	orig := event.SystemLog
	event.SystemLog = &logrus.Logger{
		Out:       &buf,
		Formatter: &logrus.TextFormatter{DisableTimestamp: true},
		Level:     logrus.TraceLevel,
	}
	t.Cleanup(func() { event.SystemLog = orig })

	return &buf
}

// openTestDb opens a test database with the dialector for the driver name.
func openTestDb(driver, dataSourceName string) (*gorm.DB, error) {
	switch driver {
	case "mysql":
		return gorm.Open(mysql.Open(dataSourceName), &gorm.Config{Logger: logger.Discard})
	default:
		return gorm.Open(sqlite.Open(dataSourceName), &gorm.Config{Logger: logger.Discard})
	}
}

// closeTestDb closes the connection pool of a test database.
func closeTestDb(db *gorm.DB) error {
	sqlDb, err := db.DB()

	if err != nil {
		return err
	}

	return sqlDb.Close()
}

func TestMigrations_Start(t *testing.T) {
	db, err := openTestDb("sqlite3", filepath.Join(t.TempDir(), "migrations.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeTestDb(db) })

	require.NoError(t, db.AutoMigrate(&Migration{}))
	require.NoError(t, db.Exec("CREATE TABLE start_test (id INTEGER PRIMARY KEY, name VARCHAR(16))").Error)

	// find returns the stored migration with the specified ID.
	find := func(id string) Migration {
		var m Migration
		require.NoError(t, db.Where("id = ?", id).First(&m).Error)
		return m
	}

	t.Run("Success", func(t *testing.T) {
		buf := captureSystemLog(t)
		list := Migrations{{ID: "20261010-000001", Statements: []string{"ALTER TABLE start_test ADD COLUMN added INT;"}}}
		require.NoError(t, list.Start(db, Opt(true, false, nil)))

		m := find("20261010-000001")
		assert.True(t, m.Finished())
		assert.Equal(t, StatusFinished, m.Status)
		assert.Empty(t, m.Error)
		assert.Equal(t, 1, m.Progress)
		assert.Empty(t, buf.String())
	})
	t.Run("Ignored", func(t *testing.T) {
		require.NoError(t, db.Exec("ALTER TABLE start_test ADD COLUMN existing INT").Error)

		buf := captureSystemLog(t)
		list := Migrations{{ID: "20261010-000002", Statements: []string{"ALTER TABLE start_test ADD COLUMN existing INT;"}}}
		require.NoError(t, list.Start(db, Opt(true, false, nil)))

		m := find("20261010-000002")
		assert.True(t, m.Finished())
		assert.Equal(t, StatusFinished, m.Status)
		assert.Empty(t, m.Error)
		assert.Empty(t, buf.String())
	})
	t.Run("Failed", func(t *testing.T) {
		buf := captureSystemLog(t)
		list := Migrations{
			{ID: "20261010-000003", Statements: []string{"UPDATE start_test_missing SET name = 'x';"}},
			{ID: "20261010-000004", Statements: []string{"UPDATE start_test SET name = 'y';"}},
		}
		require.NoError(t, list.Start(db, Opt(true, false, nil)))

		failed := find("20261010-000003")
		assert.False(t, failed.Finished())
		assert.Equal(t, StatusFailed, failed.Status)
		assert.Contains(t, failed.Error, "no such table")
		later := find("20261010-000004")
		assert.True(t, later.Finished(), "later migrations still run")

		out := buf.String()
		assert.Contains(t, out, "level=error")
		assert.Contains(t, out, "migrate: executing 20261010-000003 failed with no such table: start_test_missing")
		assert.NotContains(t, out, "20261010-000004")
	})
}

// TestMigrations_Interrupted covers startup detection and recovery of migrations whose process ended
// while they were still running.
func TestMigrations_Interrupted(t *testing.T) {
	past := time.Now().Add(-2 * RunningStaleAfter)

	// newCase opens an isolated database for a single scenario and returns it with a row lookup.
	newCase := func(t *testing.T) (*gorm.DB, func(string) Migration) {
		t.Helper()

		db, err := openTestDb("sqlite3", filepath.Join(t.TempDir(), "interrupted.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = closeTestDb(db) })

		require.NoError(t, db.AutoMigrate(&Migration{}))
		require.NoError(t, db.Exec("CREATE TABLE interrupted_test (id INTEGER PRIMARY KEY, name VARCHAR(16))").Error)

		find := func(id string) Migration {
			var m Migration
			require.NoError(t, db.Where("id = ?", id).First(&m).Error)
			return m
		}

		return db, find
	}

	// seed inserts a migration row in the given state.
	seed := func(t *testing.T, db *gorm.DB, id, status, note string, progress int, started time.Time) {
		t.Helper()
		require.NoError(t, db.Create(&Migration{
			ID: id, Dialect: "sqlite3", Stage: StageMain, Status: status, Note: note,
			Progress: progress, StartedAt: started.UTC().Truncate(time.Second),
		}).Error)
	}

	reentrant := func(id string) Migrations {
		return Migrations{{ID: id, Statements: []string{
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_interrupted_name ON interrupted_test (name);",
		}}}
	}
	nonReentrant := func(id string) Migrations {
		return Migrations{{ID: id, Statements: []string{"UPDATE interrupted_test SET name = 'z';"}}}
	}

	t.Run("BlocksNonReentrant", func(t *testing.T) {
		db, find := newCase(t)
		id := "20261015-000001"
		seed(t, db, id, StatusRunning, "", 0, past)

		list := nonReentrant(id)
		err := list.Start(db, Opt(true, false, nil))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInterrupted)

		m := find(id)
		assert.Equal(t, StatusRunning, m.Status, "the row stays running so later starts do not execute it")
		assert.Empty(t, m.Error)
		assert.Contains(t, m.Note, interruptedNote)
		assert.Contains(t, m.Note, id)
	})
	t.Run("BlockPersistsUntilManualContinue", func(t *testing.T) {
		db, find := newCase(t)
		id := "20261015-000002"
		seed(t, db, id, StatusRunning, interruptedNote+" (seeded)", 0, past)

		// Without an explicit decision the blocked migration keeps startup from continuing.
		list := nonReentrant(id)
		err := list.Start(db, Opt(true, false, nil))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInterrupted)

		// Naming the migration explicitly is the manual continuation path.
		require.NoError(t, list.Start(db, Opt(false, false, []string{id})))

		m := find(id)
		assert.True(t, m.Finished())
		assert.Equal(t, StatusFinished, m.Status)
		assert.Empty(t, m.Note)
		assert.Empty(t, m.Error)
	})
	t.Run("ResumesReentrantWithPolicy", func(t *testing.T) {
		db, find := newCase(t)
		id := "20261015-000003"
		seed(t, db, id, StatusRunning, "", 0, past)

		list := reentrant(id)
		opt := Options{RunStage: StageMain, Resume: true}
		require.NoError(t, list.Start(db, opt))

		m := find(id)
		assert.True(t, m.Finished())
		assert.Equal(t, StatusFinished, m.Status)
		assert.Empty(t, m.Note)
		assert.Equal(t, 1, m.Progress)
	})
	t.Run("DoesNotResumeWithoutPolicy", func(t *testing.T) {
		db, find := newCase(t)
		id := "20261015-000004"
		seed(t, db, id, StatusRunning, "", 0, past)

		list := reentrant(id)
		err := list.Start(db, Opt(true, false, nil))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInterrupted)

		m := find(id)
		assert.Equal(t, StatusRunning, m.Status)
		assert.Contains(t, m.Note, interruptedNote)
	})
	t.Run("SkipsRecentRunningRow", func(t *testing.T) {
		db, find := newCase(t)
		id := "20261015-000005"
		seed(t, db, id, StatusRunning, "", 0, time.Now())

		list := nonReentrant(id)
		require.NoError(t, list.Start(db, Opt(true, false, nil)))

		m := find(id)
		assert.Equal(t, StatusRunning, m.Status)
		assert.Empty(t, m.Note, "a recent row may belong to another process that is still alive")
	})
	t.Run("BlocksUnknownStaleRow", func(t *testing.T) {
		db, find := newCase(t)
		id := "20261015-000006"
		seed(t, db, id, StatusRunning, "", 2, past)

		list := Migrations{{ID: "20261015-999999", Statements: []string{"SELECT 1;"}}}
		err := list.Start(db, Opt(true, false, nil))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInterrupted)

		m := find(id)
		assert.Equal(t, StatusRunning, m.Status)
		assert.Contains(t, m.Note, "no longer part of this release")
	})
	t.Run("FailedRerunUsesSameState", func(t *testing.T) {
		db, find := newCase(t)
		id := "20261015-000007"
		list := Migrations{{ID: id, Statements: []string{"UPDATE interrupted_test_missing SET name = 'x';"}}}
		require.NoError(t, list.Start(db, Opt(true, false, nil)))

		m := find(id)
		assert.Equal(t, StatusFailed, m.Status)
		assert.Contains(t, m.Error, "no such table")

		// A normal startup keeps skipping the failed migration.
		require.NoError(t, list.Start(db, Opt(true, false, nil)))
		m = find(id)
		assert.Equal(t, StatusFailed, m.Status)

		// Retrying failed work resets it through the same running state as interrupted reruns.
		retry := reentrant(id)
		require.NoError(t, retry.Start(db, Opt(true, true, nil)))
		m = find(id)
		assert.True(t, m.Finished())
		assert.Equal(t, StatusFinished, m.Status)
		assert.Empty(t, m.Error)
		assert.Empty(t, m.Note)
	})
	t.Run("TargetedRunIgnoresOtherInterrupted", func(t *testing.T) {
		db, find := newCase(t)
		target := "20261015-000009"
		other := "20261015-000010"
		seed(t, db, target, StatusRunning, "", 0, past)
		seed(t, db, other, StatusRunning, "", 0, past)

		// The named migration continues while the unrelated interrupted row is left for normal startup.
		list := reentrant(target)
		require.NoError(t, list.Start(db, Opt(false, false, []string{target})))

		done := find(target)
		assert.True(t, done.Finished())

		left := find(other)
		assert.Equal(t, StatusRunning, left.Status)
		assert.Empty(t, left.Note)
	})
	t.Run("RecordsProgress", func(t *testing.T) {
		db, find := newCase(t)
		id := "20261015-000008"
		list := Migrations{{ID: id, Statements: []string{
			"CREATE INDEX IF NOT EXISTS idx_interrupted_one ON interrupted_test (id);",
			"CREATE INDEX IF NOT EXISTS idx_interrupted_two ON interrupted_test (name);",
		}}}
		require.NoError(t, list.Start(db, Opt(true, false, nil)))

		m := find(id)
		assert.True(t, m.Finished())
		assert.Equal(t, 2, m.Progress)
	})
}
