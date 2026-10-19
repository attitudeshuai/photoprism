# Database Migrations

**Last Updated:** October 15, 2026

This package contains the dialect-specific SQL migrations that complement GORM's schema auto-migration during database initialization. The SQL source files live in [`mysql/`](mysql/) and [`sqlite3/`](sqlite3/), and [`generate.go`](generate.go) embeds them into the generated [`dialect_mysql.go`](dialect_mysql.go) and [`dialect_sqlite3.go`](dialect_sqlite3.go) files. PostgreSQL (experimental) has a registered, empty migration list (`DialectPostgreSQL` in [`dialects.go`](dialects.go)); its schema is created by auto-migration alone.

Files ending in `.pre.sql` run in the `pre` stage before ORM auto-migration, files ending in `.post.sql` run in the `post` stage after all others, and all other migration files run in the `main` stage after auto-migration. Other stage names, and an ID used by more than one file, are rejected when the sources are read.

## Index Prefix Limits (VARBINARY Columns)

InnoDB caps an index key **prefix** at **767 bytes** on the `COMPACT`/`REDUNDANT` row formats, and only allows up to 3072 bytes on `DYNAMIC`/`COMPRESSED`. On a `VARBINARY` column the prefix is measured in **bytes** (on a `utf8mb4` column it is measured in characters, i.e. up to 4 bytes each), so a `CREATE INDEX … (col(N))` with `N > 767` fails with *"Specified key was too long; max key length is 767 bytes"* on older or non-`DYNAMIC` installs.

When a migration converts a long text column to `VARBINARY` and re-creates its prefix index, keep the prefix at **≤ 767 bytes** — the project convention is **512** (see `mysql/20220329-040000.sql` for `album_filter(512)` and `mysql/20260601-000001.sql` for `album_path(512)`). A prefix index only narrows candidate rows; the full-column comparison is still exact, so a shorter prefix has no correctness cost. SQLite has no equivalent limit and is not migrated for `VARBINARY` type changes (its `TEXT` comparisons are already byte-exact).

## When Migrations Run

PhotoPrism initializes the database through [`Config.InitDb()`](../../config/config_db.go) and [`Config.MigrateDb()`](../../config/config_db.go). This happens during `photoprism start` and in other commands that need a ready schema.

The migration flow is:

1. Open the configured database connection and register it as the active provider.
2. Load or create a row in the `versions` table for the current PhotoPrism `Version` and `Edition`.
3. Run `pre` SQL migrations from this package.
4. If the current release still `NeedsMigration()`, run the one-time schema work in [`entity.InitDb()`](../entity_init.go) and [`Tables.Migrate()`](../entity_tables.go):
   - drop deprecated tables when enabled
   - on SQLite, rebuild tables that still have the column types of earlier releases with [`ConvertSQLiteSchema()`](sqlite_schema.go), once per table and only when such a type is found; this runs after the `pre` stage, so columns renamed there keep their values, and [`sqlite_repair.go`](sqlite_repair.go) restores the paths, names and slugs that the earlier types stored as numbers where they can be derived
   - run GORM `AutoMigrate(...)` for all registered entities, which also alters existing columns that differ from the struct tags; on MariaDB it only lengthens shorter columns of the same base type, sets differing defaults in place and lets `NOT NULL` columns accept `NULL`, while longer columns or another base type are kept and logged, so any other change needs a SQL migration; on PostgreSQL it also lengthens `varchar` primary key columns, which the driver does not migrate
5. Run `main`, then `post` SQL migrations from this package.
6. Mark the current release as migrated by setting `versions.migrated_at`. If auto-migration failed or tables are missing afterward, `MigrateDb()` stores the error in `versions.error` instead, writes it to the system log, and returns it, so the next start runs auto-migration again. A failed auto-migration also skips the `main` and `post` stages, and `entity.InitDb()` then creates the default fixtures only if their tables exist. Running selected migrations by ID records nothing.

The important distinction is that the `versions` table gates the expensive release-level schema initialization, while the `migrations` table tracks each named SQL migration in this package.

## Why Migrations Are Not Repeated

PhotoPrism uses two persistence layers to avoid rerunning the same startup work and to keep broken migrations from producing the same error on every launch.

### `versions`: Once Per Release

The `versions` table stores one row per PhotoPrism `Version` and `Edition`; Portal builds record their edition as `portal-<edition>`, since they register tables of their own. After a successful release-level initialization, `MigrateDb()` sets `migrated_at`. On later startups of the same release, `NeedsMigration()` returns `false`, so the full ORM auto-migration path is skipped. This reduces startup time and avoids repeating the same broad schema work over and over again.

### `migrations`: Once Per Migration ID

Each SQL migration in this package has a stable timestamp-based `ID`, for example `20240709-000001`. Before running a migration, PhotoPrism looks for an existing row in the `migrations` table. Each row records its lifecycle in the `status`, `started_at`, `finished_at`, `error`, `note`, and `progress` columns:

- `status = finished` means the migration succeeded (`finished_at` is set); it is skipped.
- `status = failed` means the last attempt failed (`error` is set); it is skipped during normal startup.
- `status = running` means execution started but never reported back. Rows younger than 60 minutes are assumed to belong to another process that may still be alive, are skipped with a warning, and must not be touched. Rows older than 60 minutes belong to a process that ended and are treated as interrupted.
- Rows written before the `status` column existed infer their state from the timestamps: a set `finished_at` means finished, a set `error` means failed, and a set `started_at` alone means running.

Interrupted migrations are detected on every startup:

- The migration is **rerun automatically** only when `PHOTOPRISM_MIGRATION_RESUME` is enabled (or `photoprism migrations run --failed` is used) **and** every statement is classified as safe to repeat (`Migration.Reentrant()`). Guarded DDL such as `CREATE ... IF NOT EXISTS`, `DROP ... IF EXISTS`, `CREATE OR REPLACE INDEX`, idempotent `ALTER TABLE` forms, and guarded `UPDATE`/`DELETE` statements qualify; unguarded data changes such as `TRUNCATE` do not.
- Otherwise startup is **refused**: `migrate.Run` returns `ErrInterrupted`, the schema is not touched further, and the row keeps `status = running` with a `note` that names the migration, its start time, its progress (`n/total` completed statements), and the exact command to continue it. The note makes the interruption visible in `photoprism migrations ls` without comparing schemas by hand.
- Naming a migration explicitly always continues it, regardless of its age, note, or reentrancy: `photoprism migrations run 20240709-000001`. Both failed and interrupted reruns first reset the row through the same running state, then execute, and end as either finished or failed.

This is the reason broken migrations do not spam the logs on every startup: the failure is recorded once in `migrations.error` and written to the system log, and future runs see that row and skip it unless you explicitly ask to retry it. Errors that only mean the change is already in place, such as an existing column, match `IgnoreErr` in [`errors.go`](errors.go) and count as success.

## Troubleshooting & Testing

Use the CLI commands below when you need to inspect migration state, retry failed work, or test specific migration IDs. These commands use the normal PhotoPrism configuration, so point them at a disposable or copied database first when you are testing reruns.

### Inspecting Status

```bash
photoprism migrations
photoprism migrations ls
photoprism migrations status
photoprism migrations show
```

`photoprism migrations` shows the available subcommands. `photoprism migrations ls` displays the current status of known migrations without executing them.

For automation or diffing, `ls` also supports machine-readable output:

```bash
photoprism migrations ls --json
photoprism migrations ls --md
photoprism migrations ls --csv
photoprism migrations ls --tsv
```

The status output maps to the runtime logic in [`migration.go`](migration.go):

| Status         | Meaning                                                                                                |
|----------------|--------------------------------------------------------------------------------------------------------|
| `OK`           | The migration finished successfully.                                                                   |
| `-`            | The migration has not been started yet.                                                                |
| `Running?`     | The migration started less than 60 minutes ago and is assumed to still be in progress.                |
| `Interrupted`  | The running record is older than 60 minutes, so its process ended before the migration finished.      |
| `<note text>`  | An interrupted migration blocked startup; the note shows its progress and how to continue it manually.|
| `<error text>` | The last attempt failed and will be skipped on normal startup.                                         |

### Running Migrations

```bash
photoprism migrations run
photoprism migrations run --trace
photoprism migrations run --failed
photoprism migrations run --failed --trace
photoprism migrate
```

`photoprism migrations run` executes pending schema migrations. `--trace` enables trace logging for debugging. `--failed` retries migrations that have an `error` recorded in the `migrations` table. `photoprism migrate` is a top-level alias for `photoprism migrations run`.

### Running Specific Migration IDs

You can limit status checks or runs to one or more specific migration IDs:

```bash
photoprism migrations ls 20240709-000001
photoprism migrations run 20240709-000001
photoprism migrations run --failed "20240709-000001 20250315-000001"
```

When you pass a specific ID, PhotoPrism selects only that migration. When you pass multiple IDs, quote them as a single whitespace-separated argument so the CLI can split them correctly.

Named migrations are allowed to run again even if they already succeeded, previously failed, or were interrupted. That makes targeted troubleshooting possible, but it also means you should prefer a test database before forcing reruns. A migration that is not safe to repeat can only be continued this way after you have verified the schema; its note identifies the last completed statement.

### Automatic Resume Policy

By default, interrupted migrations block startup until an operator continues them. Set `PHOTOPRISM_MIGRATION_RESUME=true` (or use the `--migration-resume` flag) to rerun interrupted migrations automatically, but only when all of their statements are safe to repeat. The policy never applies to migrations younger than 60 minutes, since another process may still be executing them.
