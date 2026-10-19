# Migration Package Guidelines

**Last Updated:** October 15, 2026

This file applies to `internal/entity/migrate/`. Read [`README.md`](README.md) here for the runtime flow, retry behavior, and CLI troubleshooting commands.

## Editing Migrations

- Add or change SQL in [`mysql/`](mysql/) and [`sqlite3/`](sqlite3/); never hand-edit the generated [`dialect_mysql.go`](dialect_mysql.go) or [`dialect_sqlite3.go`](dialect_sqlite3.go) files, not even to add a comment. `TestGeneratedDialects` (also `make check-migrations`, part of `make lint`) fails when they differ from what the SQL files generate. Put the reasoning for a migration in the commit message or the specs, since generated files cannot carry it. PostgreSQL (experimental) has a registered, empty migration list in [`dialects.go`](dialects.go); its schema comes from auto-migration alone.
- Reuse the same timestamp-based migration ID across dialects for the same logical change.
- Use `.pre.sql` only when the SQL must run before GORM `AutoMigrate(...)`, typically for renames or shape changes that the ORM must see afterward.
- Use `.post.sql` for SQL that must run after all `main` migrations. A file name with any other stage, or an ID used twice within a dialect, fails generation.
- Keep migrations idempotent when possible with `IF EXISTS`, `IF NOT EXISTS`, or safe update conditions. Failed migrations are recorded once and then skipped on normal startup, so noisy or brittle SQL creates persistent operator friction until someone retries it manually.
- When (re-)creating a prefix index on a `VARBINARY` column, keep the prefix at **≤ 767 bytes** (the convention is `512`). InnoDB caps key prefixes at 767 bytes on `COMPACT`/`REDUNDANT` row formats, and `VARBINARY` prefixes are counted in bytes, so a longer prefix fails on older or non-`DYNAMIC` installs. See [`README.md`](README.md) → "Index Prefix Limits".
- If a migration requires custom Go logic instead of plain SQL, keep the reason narrow and obvious. Package-local helpers should still preserve the same retry semantics described in [`README.md`](README.md).

## Generation & Verification

- From the repository root, after changing migration SQL, run `go generate ./internal/entity/migrate`. The parsing and rendering live in [`source.go`](source.go) (`DialectSource`, `DialectCode`), which [`generate.go`](generate.go) and the drift test share. `generate.go` compiles this package, so after a merge conflict in a generated file, take either side first, then regenerate.
- After Go edits in this package, run `make fmt-go` from the repository root.
- Verify the package with `go test ./internal/entity/migrate -count=1`.
- When a change affects operator workflows, also verify the CLI help and status paths with `./photoprism migrations --help`, `./photoprism migrations ls --help`, and `./photoprism migrations run --help`.

## Test Fixtures

- Keep [`testdata/migrate_sqlite3`](testdata/migrate_sqlite3) and [`testdata/migrate_mysql.sql`](testdata/migrate_mysql.sql) aligned with the pre-migration schema shape expected by the regression tests.
- `TestDialectSQLite3` runs against a copied SQLite fixture. `TestDialectMysql` expects a MariaDB service reachable as `mariadb:${MARIADB_PORT:-4001}` with the `migrate` database and user from [`testdata/migrate_mysql.sql`](testdata/migrate_mysql.sql).

## Runtime Model

- The `versions` table gates the once-per-release schema initialization path.
- The `migrations` table stores per-migration `status` (`running`, `finished`, `failed`), `started_at`, `finished_at`, `error`, `note`, and `progress` (completed statements) state; rows written before `status` existed infer it from the timestamps via `Migration.State()`.
- Normal startup does not rerun rows that already failed. Retrying failed rows requires `photoprism migrations run --failed` or explicitly naming migration IDs.
- Running rows younger than 60 minutes are assumed to belong to another live process and are skipped with a warning. Older running rows are interrupted: `Start` returns `ErrInterrupted` and blocks startup unless the migration is reentrant (`Migration.Reentrant()`, a conservative statement classifier in `migration.go`) and resume is declared via `Options.Resume` (`PHOTOPRISM_MIGRATION_RESUME`, or `--failed`). Non-reentrant or policy-disabled rows keep status `running` plus a blocking `note` for manual continuation by ID; failed and interrupted reruns share the same reset-to-running transition in `Migration.Begin()`.
