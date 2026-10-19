package migrate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/photoprism/photoprism/pkg/dsn"
)

func TestMigration_State(t *testing.T) {
	finished := time.Now().UTC().Truncate(time.Second)
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)

	t.Run("Empty", func(t *testing.T) {
		m := &Migration{}
		assert.Empty(t, m.State())
	})
	t.Run("Running", func(t *testing.T) {
		m := &Migration{Status: StatusRunning, StartedAt: started}
		assert.Equal(t, StatusRunning, m.State())
		assert.True(t, m.Running())
		assert.False(t, m.Interrupted())
	})
	t.Run("Interrupted", func(t *testing.T) {
		m := Migration{Status: StatusRunning, Note: interruptedNote, StartedAt: started}
		assert.True(t, m.Interrupted())
	})
	t.Run("Finished", func(t *testing.T) {
		m := Migration{Status: StatusFinished, StartedAt: started, FinishedAt: &finished}
		assert.Equal(t, StatusFinished, m.State())
		assert.True(t, m.Finished())
	})
	t.Run("Failed", func(t *testing.T) {
		m := Migration{Status: StatusFailed, Error: "boom", StartedAt: started}
		assert.Equal(t, StatusFailed, m.State())
	})
	t.Run("LegacyFinished", func(t *testing.T) {
		// Rows written before the Status column existed infer their state from the timestamps.
		m := Migration{StartedAt: started, FinishedAt: &finished}
		assert.Equal(t, StatusFinished, m.State())
	})
	t.Run("LegacyFailed", func(t *testing.T) {
		m := Migration{StartedAt: started, Error: "boom"}
		assert.Equal(t, StatusFailed, m.State())
	})
	t.Run("LegacyRunning", func(t *testing.T) {
		m := Migration{StartedAt: started}
		assert.Equal(t, StatusRunning, m.State())
	})
}

func TestMigration_Stale(t *testing.T) {
	t.Run("Young", func(t *testing.T) {
		m := Migration{Status: StatusRunning, StartedAt: time.Now().Add(-time.Minute).UTC()}
		assert.True(t, m.Running())
		assert.False(t, m.Stale())
	})
	t.Run("Stale", func(t *testing.T) {
		m := Migration{Status: StatusRunning, StartedAt: time.Now().Add(-2 * RunningStaleAfter)}
		assert.True(t, m.Stale())
	})
	t.Run("Finished", func(t *testing.T) {
		finished := time.Now().Add(-2 * RunningStaleAfter)
		m := Migration{Status: StatusFinished, StartedAt: finished, FinishedAt: &finished}
		assert.False(t, m.Stale())
	})
}

func TestReentrantStatement(t *testing.T) {
	reentrant := []string{
		"DROP INDEX IF EXISTS idx_places_place_label ON places;",
		"DROP TABLE IF EXISTS obsolete;",
		"CREATE INDEX IF NOT EXISTS idx_albums_album_filter ON albums (album_filter);",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_files_search_media ON files (media_id);",
		"CREATE TABLE IF NOT EXISTS x (id INT);",
		"CREATE OR REPLACE INDEX idx_files_photo_id ON files (photo_id, file_primary);",
		"CREATE OR REPLACE UNIQUE INDEX idx_files_search_media ON files (media_id);",
		"ALTER TABLE files MODIFY file_projection VARBINARY(64) NULL;",
		"ALTER TABLE files MODIFY COLUMN IF EXISTS photo_taken_at DATETIME AFTER photo_uid;",
		"ALTER TABLE files ADD COLUMN IF NOT EXISTS photo_taken_at DATETIME AFTER photo_uid;",
		"ALTER TABLE files ADD IF NOT EXISTS media_id VARBINARY(32) AFTER photo_taken_at;",
		"ALTER TABLE files ADD COLUMN photo_taken_at DATETIME AFTER photo_uid;",
		"ALTER TABLE links DROP COLUMN IF EXISTS can_edit;",
		"ALTER TABLE photos CHANGE COLUMN IF EXISTS photo_description photo_caption VARCHAR(4096);",
		"ALTER IGNORE TABLE files_sync CHANGE account_id service_id INT UNSIGNED NOT NULL;",
		"ALTER IGNORE TABLE auth_sessions RENAME COLUMN auth_domain TO auth_issuer;",
		"ALTER TABLE photos RENAME COLUMN photo_description TO photo_caption;",
		"RENAME TABLE IF EXISTS `accounts` TO `services`;",
		"UPDATE auth_users SET auth_provider = 'local' WHERE id = 1;",
		"DELETE FROM auth_users_settings WHERE user_uid IS NULL;",
		"INSERT IGNORE INTO x (id) VALUES (1);",
		"INSERT OR IGNORE INTO x (id) VALUES (1);",
		"-- backfill missing values\nUPDATE photos SET time_zone = 'Local' WHERE time_zone = '' OR time_zone IS NULL;",
	}

	for _, s := range reentrant {
		assert.True(t, reentrantStatement(s), s)
	}

	notReentrant := []string{
		"TRUNCATE auth_sessions;",
		"DELETE FROM auth_sessions;",
		"UPDATE files f JOIN photos p ON p.id = f.photo_id SET f.photo_taken_at = p.taken_at_local;",
		"UPDATE faces SET face_kind = 1;",
		"INSERT INTO x (id) VALUES (1);",
		"CREATE INDEX idx_x ON x (id);",
		"DROP INDEX idx_x ON x;",
		"ALTER TABLE x ENGINE=InnoDB;",
		"GRANT SELECT ON x TO 'u';",
		"",
		"-- only a comment",
	}

	for _, s := range notReentrant {
		assert.False(t, reentrantStatement(s), s)
	}
}

func TestMigration_Reentrant(t *testing.T) {
	safe := &Migration{Statements: []string{
		"DROP INDEX IF EXISTS idx_x ON t;",
		"CREATE INDEX IF NOT EXISTS idx_x ON t (c);",
	}}
	assert.True(t, safe.Reentrant())

	unsafe := &Migration{Statements: []string{
		"CREATE INDEX IF NOT EXISTS idx_x ON t (c);",
		"DELETE FROM t;",
	}}
	assert.False(t, unsafe.Reentrant())
}

// TestDialects_Reentrant pins which shipped migrations cannot be rerun safely after an interruption.
// Every migration added without idempotent guards must be reviewed and listed here on purpose.
func TestDialects_Reentrant(t *testing.T) {
	notReentrant := map[string]map[string]bool{
		dsn.DriverSQLite3: {
			"20240112-000001": true, // unguarded session table wipe
		},
		dsn.DriverMySQL: {
			"20220329-071000": true, // unguarded cross-table update
			"20220329-083000": true, // derived value computed for every row
			"20220329-093000": true, // derived value computed for every row
			"20240112-000001": true, // unguarded session table truncation
		},
	}

	for driver, list := range map[string]Migrations{
		dsn.DriverSQLite3: DialectSQLite3,
		dsn.DriverMySQL:   DialectMySQL,
	} {
		for _, m := range list {
			if notReentrant[driver][m.ID] {
				assert.False(t, m.Reentrant(), "%s %s expected to be non-reentrant", driver, m.ID)
			} else {
				assert.True(t, m.Reentrant(), "%s %s expected to be reentrant", driver, m.ID)
			}
		}
	}
}
