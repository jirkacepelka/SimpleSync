package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type User struct {
	ID        int64
	Username  string
	IsAdmin   bool
	CreatedAt time.Time
	hash      string
}

func (u *User) PasswordHash() string { return u.hash }

// Error messages are i18n keys, translated by the web UI.
var ErrUsernameTaken = errors.New("err.usernameTaken")
var ErrInvalidUsername = errors.New("err.invalidUsername")

func ValidUsername(name string) bool {
	if len(name) < 2 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@", r)) {
			return false
		}
	}
	return true
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, admin bool) (*User, error) {
	username = strings.TrimSpace(username)
	if !ValidUsername(username) {
		return nil, ErrInvalidUsername
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO users(username, password_hash, is_admin, created_at) VALUES(?, ?, ?, ?)",
		username, passwordHash, boolInt(admin), s.now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(ctx, id)
}

const userCols = "id, username, password_hash, is_admin, created_at"

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created int64
	if err := row.Scan(&u.ID, &u.Username, &u.hash, &u.IsAdmin, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

func (s *Store) UserByName(ctx context.Context, name string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE username = ?", strings.TrimSpace(name)))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) SetPassword(ctx context.Context, userID int64, hash string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE users SET password_hash = ? WHERE id = ?", hash, userID); err != nil {
			return err
		}
		// A password change signs the user out of every browser session.
		_, err := tx.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
		return err
	})
}

func (s *Store) SetAdmin(ctx context.Context, userID int64, admin bool) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET is_admin = ? WHERE id = ?", boolInt(admin), userID)
	return err
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE is_admin = 1").Scan(&n)
	return n, err
}

func (s *Store) DeleteUser(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID)
	return err
}

// ---- devices (API tokens) ----

type Device struct {
	ID        int64
	UserID    int64
	Username  string
	Name      string
	VaultName string // server-side name of the vault the device last synced; "" if none yet
	CreatedAt time.Time
	LastSeen  time.Time
}

func (s *Store) CreateDevice(ctx context.Context, userID int64, name, tokenHash string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Zařízení"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, "INSERT INTO devices(user_id, name, token_hash, created_at, last_seen) VALUES(?, ?, ?, ?, ?)",
		userID, name, tokenHash, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeviceByToken resolves an API token to its device and user and refreshes
// last_seen (at most once a minute to avoid needless writes).
func (s *Store) DeviceByToken(ctx context.Context, tokenHash string) (*Device, *User, error) {
	var d Device
	var created, seen int64
	err := s.db.QueryRowContext(ctx, "SELECT id, user_id, name, created_at, last_seen FROM devices WHERE token_hash = ?", tokenHash).
		Scan(&d.ID, &d.UserID, &d.Name, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	d.CreatedAt, d.LastSeen = time.Unix(created, 0), time.Unix(seen, 0)
	u, err := s.UserByID(ctx, d.UserID)
	if err != nil {
		return nil, nil, err
	}
	d.Username = u.Username
	if now := s.now(); now-seen > 60 {
		s.db.ExecContext(ctx, "UPDATE devices SET last_seen = ? WHERE id = ?", now, d.ID)
	}
	return &d, u, nil
}

// ListDevices lists devices of one user, or of everybody when userID is 0.
func (s *Store) ListDevices(ctx context.Context, userID int64) ([]*Device, error) {
	q := "SELECT d.id, d.user_id, u.username, d.name, COALESCE(v.name, ''), d.created_at, d.last_seen FROM devices d JOIN users u ON u.id = d.user_id LEFT JOIN vaults v ON v.id = d.vault_id"
	args := []any{}
	if userID != 0 {
		q += " WHERE d.user_id = ?"
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY d.last_seen DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		var d Device
		var created, seen int64
		if err := rows.Scan(&d.ID, &d.UserID, &d.Username, &d.Name, &d.VaultName, &created, &seen); err != nil {
			return nil, err
		}
		d.CreatedAt, d.LastSeen = time.Unix(created, 0), time.Unix(seen, 0)
		out = append(out, &d)
	}
	return out, rows.Err()
}

// SetDeviceVault records the vault a device is syncing.
func (s *Store) SetDeviceVault(ctx context.Context, deviceID, vaultID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE devices SET vault_id = ? WHERE id = ? AND vault_id IS NOT ?", vaultID, deviceID, vaultID)
	return err
}

func (s *Store) DeviceByID(ctx context.Context, id int64) (*Device, error) {
	var d Device
	var created, seen int64
	err := s.db.QueryRowContext(ctx, "SELECT id, user_id, name, created_at, last_seen FROM devices WHERE id = ?", id).
		Scan(&d.ID, &d.UserID, &d.Name, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	d.CreatedAt, d.LastSeen = time.Unix(created, 0), time.Unix(seen, 0)
	return &d, err
}

func (s *Store) DeleteDevice(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM devices WHERE id = ?", id)
	return err
}

// ---- browser sessions ----

func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash, csrf string, ttl time.Duration) error {
	s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at < ?", s.now())
	_, err := s.db.ExecContext(ctx, "INSERT INTO sessions(token_hash, user_id, csrf, expires_at) VALUES(?, ?, ?, ?)",
		tokenHash, userID, csrf, s.Now().Add(ttl).Unix())
	return err
}

func (s *Store) SessionUser(ctx context.Context, tokenHash string) (*User, string, error) {
	var userID, expires int64
	var csrf string
	err := s.db.QueryRowContext(ctx, "SELECT user_id, csrf, expires_at FROM sessions WHERE token_hash = ?", tokenHash).
		Scan(&userID, &csrf, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expires < s.now()) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	u, err := s.UserByID(ctx, userID)
	return u, csrf, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	return err
}
