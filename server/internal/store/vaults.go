package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Vault roles, from least to most privileged.
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleOwner  = "owner"
)

func ValidRole(r string) bool { return r == RoleViewer || r == RoleEditor || r == RoleOwner }

// RoleAtLeast reports whether role grants at least the privileges of min.
func RoleAtLeast(role, min string) bool {
	rank := map[string]int{RoleViewer: 1, RoleEditor: 2, RoleOwner: 3}
	return rank[role] >= rank[min]
}

// Backup intervals offered in the UI (seconds; 0 = off).
var BackupIntervals = []int64{0, 3600, 6 * 3600, 86400, 7 * 86400}

// Backup retention choices (days; 0 = keep forever).
var BackupRetentions = []int{7, 30, 90, 365, 0}

func ValidBackupInterval(sec int64) bool {
	for _, i := range BackupIntervals {
		if i == sec {
			return true
		}
	}
	return false
}

func ValidBackupRetention(days int) bool {
	for _, r := range BackupRetentions {
		if r == days {
			return true
		}
	}
	return false
}

type BackupPolicy struct {
	IntervalSec   int64
	RetentionDays int
	Zip           bool
}

var DefaultBackupPolicy = BackupPolicy{IntervalSec: 86400, RetentionDays: 30}

type Vault struct {
	ID              int64
	Name            string
	HeadRev         int64
	Backup          BackupPolicy
	LastBackupAt    *time.Time
	LastBackupError string
	CreatedAt       time.Time
	FolderAccess    bool   // per-folder access rules are switched on
	AccessRev       int64  // bumped whenever who may see what changes
	Role            string // role of the requesting user (filled by list helpers)
}

// Error messages are i18n keys, translated by the web UI.
var (
	ErrVaultNameTaken      = errors.New("err.vaultNameTaken")
	ErrInvalidVaultName    = errors.New("err.invalidVaultName")
	ErrInvalidBackupPolicy = errors.New("err.invalidBackupPolicy")
	ErrInvalidRole         = errors.New("err.invalidRole")
)

const vaultCols = "id, name, head_rev, backup_interval, backup_retention_days, backup_zip, last_backup_at, last_backup_error, created_at, folder_access, access_rev"

func scanVault(row interface{ Scan(...any) error }, extra ...any) (*Vault, error) {
	var v Vault
	var last sql.NullInt64
	var created int64
	dest := append([]any{&v.ID, &v.Name, &v.HeadRev, &v.Backup.IntervalSec, &v.Backup.RetentionDays, &v.Backup.Zip, &last, &v.LastBackupError, &created, &v.FolderAccess, &v.AccessRev}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if last.Valid {
		t := time.Unix(last.Int64, 0)
		v.LastBackupAt = &t
	}
	v.CreatedAt = time.Unix(created, 0)
	return &v, nil
}

// CreateVault creates a vault; if ownerID is non-zero that user becomes owner.
func (s *Store) CreateVault(ctx context.Context, name string, policy BackupPolicy, ownerID int64) (*Vault, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "/\\") {
		return nil, ErrInvalidVaultName
	}
	if !ValidBackupInterval(policy.IntervalSec) || !ValidBackupRetention(policy.RetentionDays) {
		return nil, ErrInvalidBackupPolicy
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("INSERT INTO vaults(name, backup_interval, backup_retention_days, backup_zip, created_at) VALUES(?, ?, ?, ?, ?)",
			name, policy.IntervalSec, policy.RetentionDays, boolInt(policy.Zip), s.now())
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrVaultNameTaken
			}
			return err
		}
		id, _ = res.LastInsertId()
		if ownerID != 0 {
			_, err = tx.Exec("INSERT INTO vault_members(vault_id, user_id, role) VALUES(?, ?, ?)", id, ownerID, RoleOwner)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Vault(ctx, id)
}

func (s *Store) Vault(ctx context.Context, id int64) (*Vault, error) {
	return scanVault(s.db.QueryRowContext(ctx, "SELECT "+vaultCols+" FROM vaults WHERE id = ?", id))
}

func (s *Store) RenameVault(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "/\\") {
		return ErrInvalidVaultName
	}
	_, err := s.db.ExecContext(ctx, "UPDATE vaults SET name = ? WHERE id = ?", name, id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrVaultNameTaken
	}
	return err
}

func (s *Store) SetBackupPolicy(ctx context.Context, id int64, p BackupPolicy) error {
	if !ValidBackupInterval(p.IntervalSec) || !ValidBackupRetention(p.RetentionDays) {
		return ErrInvalidBackupPolicy
	}
	_, err := s.db.ExecContext(ctx, "UPDATE vaults SET backup_interval = ?, backup_retention_days = ?, backup_zip = ? WHERE id = ?",
		p.IntervalSec, p.RetentionDays, boolInt(p.Zip), id)
	return err
}

func (s *Store) DeleteVault(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM vaults WHERE id = ?", id)
	return err
}

// ListVaults returns every vault (for admins) with Role set to "owner".
func (s *Store) ListVaults(ctx context.Context) ([]*Vault, error) {
	return s.queryVaults(ctx, "SELECT "+vaultCols+", 'owner' FROM vaults ORDER BY name")
}

// UserVaults returns the vaults a user is a member of, with their role.
func (s *Store) UserVaults(ctx context.Context, userID int64) ([]*Vault, error) {
	return s.queryVaults(ctx, "SELECT "+prefixed("v.", vaultCols)+", m.role FROM vaults v JOIN vault_members m ON m.vault_id = v.id WHERE m.user_id = ? ORDER BY v.name", userID)
}

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ", ")
}

func (s *Store) queryVaults(ctx context.Context, q string, args ...any) ([]*Vault, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Vault
	for rows.Next() {
		var role string
		v, err := scanVault(rows, &role)
		if err != nil {
			return nil, err
		}
		v.Role = role
		out = append(out, v)
	}
	return out, rows.Err()
}

// Role returns the role user has in vault ("" if none). Admins are treated as
// owners of every vault.
func (s *Store) Role(ctx context.Context, u *User, vaultID int64) (string, error) {
	if u.IsAdmin {
		if _, err := s.Vault(ctx, vaultID); err != nil {
			return "", err
		}
		return RoleOwner, nil
	}
	var role string
	err := s.db.QueryRowContext(ctx, "SELECT role FROM vault_members WHERE vault_id = ? AND user_id = ?", vaultID, u.ID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

type Member struct {
	UserID   int64
	Username string
	Role     string       // owner, editor or viewer
	RoleIDs  []int64      // custom roles (folder access)
	Folders  []AccessRule // folders given to this member alone
}

// HasRole reports whether the member has the custom role id.
func (m Member) HasRole(id int64) bool {
	for _, r := range m.RoleIDs {
		if r == id {
			return true
		}
	}
	return false
}

func (s *Store) Members(ctx context.Context, vaultID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.username, m.role FROM vault_members m JOIN users u ON u.id = m.user_id
		WHERE m.vault_id = ? ORDER BY u.username`, vaultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Username, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // the store has one connection: finish before the next query
	roles, folders, err := s.memberAccess(ctx, vaultID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].RoleIDs, out[i].Folders = roles[out[i].UserID], folders[out[i].UserID]
	}
	return out, nil
}

func (s *Store) SetMember(ctx context.Context, vaultID, userID int64, role string) error {
	if !ValidRole(role) {
		return ErrInvalidRole
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO vault_members(vault_id, user_id, role) VALUES(?, ?, ?) ON CONFLICT(vault_id, user_id) DO UPDATE SET role = excluded.role",
			vaultID, userID, role); err != nil {
			return err
		}
		return bumpAccess(tx, vaultID)
	})
}

func (s *Store) RemoveMember(ctx context.Context, vaultID, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM vault_members WHERE vault_id = ? AND user_id = ?", vaultID, userID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM vault_access_rules WHERE vault_id = ? AND user_id = ?", vaultID, userID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM vault_member_roles WHERE vault_id = ? AND user_id = ?", vaultID, userID); err != nil {
			return err
		}
		return bumpAccess(tx, vaultID)
	})
}

type VaultStats struct {
	Files int64
	Bytes int64
}

func (s *Store) VaultStats(ctx context.Context, vaultID int64) (VaultStats, error) {
	var st VaultStats
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files WHERE vault_id = ? AND deleted = 0", vaultID).Scan(&st.Files, &st.Bytes)
	return st, err
}
