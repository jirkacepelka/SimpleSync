// Package store holds all metadata in a single SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db  *sql.DB
	Now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
	password_hash TEXT NOT NULL,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS vaults (
	id                    INTEGER PRIMARY KEY AUTOINCREMENT, -- ids are never reused: devices remember them
	name                  TEXT NOT NULL UNIQUE COLLATE NOCASE,
	head_rev              INTEGER NOT NULL DEFAULT 0,
	backup_interval       INTEGER NOT NULL DEFAULT 86400,
	backup_retention_days INTEGER NOT NULL DEFAULT 30,
	backup_zip            INTEGER NOT NULL DEFAULT 0,
	last_backup_at        INTEGER,
	last_backup_error     TEXT NOT NULL DEFAULT '',
	created_at            INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS vault_members (
	vault_id INTEGER NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
	user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role     TEXT NOT NULL,
	PRIMARY KEY (vault_id, user_id)
);
CREATE TABLE IF NOT EXISTS devices (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL,
	vault_id   INTEGER REFERENCES vaults(id) ON DELETE SET NULL -- vault the device last synced
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf       TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS files (
	vault_id   INTEGER NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
	path       TEXT NOT NULL,
	path_fold  TEXT NOT NULL,
	hash       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	mtime      INTEGER NOT NULL,
	deleted    INTEGER NOT NULL,
	rev        INTEGER NOT NULL,
	device_id  INTEGER,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (vault_id, path)
);
CREATE INDEX IF NOT EXISTS files_rev ON files(vault_id, rev);
CREATE INDEX IF NOT EXISTS files_fold ON files(vault_id, path_fold);
CREATE TABLE IF NOT EXISTS file_versions (
	id         INTEGER PRIMARY KEY,
	vault_id   INTEGER NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
	path       TEXT NOT NULL,
	hash       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	mtime      INTEGER NOT NULL,
	deleted    INTEGER NOT NULL,
	rev        INTEGER NOT NULL,
	device_id  INTEGER,
	author     TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS file_versions_path ON file_versions(vault_id, path, rev);
CREATE INDEX IF NOT EXISTS file_versions_hash ON file_versions(vault_id, hash);
CREATE TABLE IF NOT EXISTS backups (
	id         INTEGER PRIMARY KEY,
	vault_id   INTEGER NOT NULL REFERENCES vaults(id) ON DELETE CASCADE,
	rev        INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	file_count INTEGER NOT NULL,
	size_bytes INTEGER NOT NULL,
	zip_path   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS backups_vault ON backups(vault_id, created_at);
CREATE TABLE IF NOT EXISTS backup_files (
	backup_id INTEGER NOT NULL REFERENCES backups(id) ON DELETE CASCADE,
	path      TEXT NOT NULL,
	hash      TEXT NOT NULL,
	size      INTEGER NOT NULL,
	mtime     INTEGER NOT NULL,
	PRIMARY KEY (backup_id, path)
);
CREATE INDEX IF NOT EXISTS backup_files_hash ON backup_files(hash);
CREATE TABLE IF NOT EXISTS vault_publish (
	vault_id INTEGER PRIMARY KEY REFERENCES vaults(id) ON DELETE CASCADE,
	enabled  INTEGER NOT NULL DEFAULT 0,
	slug     TEXT NOT NULL UNIQUE COLLATE NOCASE,
	title    TEXT NOT NULL DEFAULT '',
	mode     TEXT NOT NULL DEFAULT 'marked',
	folder   TEXT NOT NULL DEFAULT '',
	home     TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes writers and avoids SQLITE_BUSY. Traffic
	// of a personal sync server is tiny, so this is not a bottleneck.
	// Consequence: never run a query while iterating rows of another one.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Databases created before devices.vault_id existed.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('devices') WHERE name = 'vault_id'").Scan(&n); err == nil && n == 0 {
		if _, err := db.Exec("ALTER TABLE devices ADD COLUMN vault_id INTEGER REFERENCES vaults(id) ON DELETE SET NULL"); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db, Now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) now() int64 { return s.Now().Unix() }

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// BackupDB writes a consistent copy of the database to dst.
func (s *Store) BackupDB(ctx context.Context, dst string) error {
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}

// ---- settings ----

type Settings struct {
	VersionRetentionDays int   // how long old file versions are kept
	MaxFileMB            int64 // upload limit per file
	UsersCanCreateVaults bool  // whether non-admins may create vaults from the plugin
}

func (s *Store) Settings(ctx context.Context) Settings {
	out := Settings{VersionRetentionDays: 30, MaxFileMB: 200, UsersCanCreateVaults: true}
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) != nil {
			continue
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "version_retention_days":
			out.VersionRetentionDays = int(n)
		case "max_file_mb":
			if n > 0 {
				out.MaxFileMB = n
			}
		case "users_can_create_vaults":
			out.UsersCanCreateVaults = n == 1
		}
	}
	return out
}

func (s *Store) SaveSettings(ctx context.Context, st Settings) error {
	b := 0
	if st.UsersCanCreateVaults {
		b = 1
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for k, v := range map[string]int64{
			"version_retention_days":  int64(st.VersionRetentionDays),
			"max_file_mb":             st.MaxFileMB,
			"users_can_create_vaults": int64(b),
		} {
			if _, err := tx.Exec("INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", k, strconv.FormatInt(v, 10)); err != nil {
				return err
			}
		}
		return nil
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
