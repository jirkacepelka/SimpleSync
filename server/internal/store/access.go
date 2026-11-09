package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/jirkacepelka/obsisync/server/internal/pathutil"
)

// Folder access: when a vault has it switched on, members can be limited to
// parts of the vault. A rule says how much of a folder (or a single file) one
// may see: nothing, read only, or read and write. Rules belong to a custom
// role, or to one member as an exception. The most specific rule (the longest
// matching path) wins; on the same path a member's exception beats the rule of
// their role. A rule never grants more than the base role: whoever may only
// read the vault cannot write anywhere. Owners and administrators always see
// everything.

// Access levels of a rule.
const (
	LevelNone  = "none"
	LevelRead  = "read"
	LevelWrite = "write"
)

func ValidLevel(l string) bool { return l == LevelNone || l == LevelRead || l == LevelWrite }

// ErrCodeForbidden is returned per op when the path may not be written.
const ErrCodeForbidden = "forbidden"

var (
	ErrRoleNameTaken   = errors.New("err.roleNameTaken")
	ErrInvalidRoleName = errors.New("err.invalidRoleName")
	ErrInvalidRule     = errors.New("err.invalidRule")
)

// AccessRule limits access to a folder or file ("" = the whole vault).
type AccessRule struct {
	ID     int64  `json:"-"`
	Path   string `json:"path"`
	Level  string `json:"level"`
	Member bool   `json:"-"` // a member's own exception, not a rule of their role
}

// Display is the path as shown in the admin ("/" for the whole vault).
func (r AccessRule) Display() string {
	if r.Path == "" {
		return "/"
	}
	return r.Path
}

// VaultRole is a custom role of one vault.
type VaultRole struct {
	ID      int64
	VaultID int64
	Name    string
	Base    string // RoleViewer or RoleEditor
	Rules   []AccessRule
	Members int
}

// Access says what one user may do with the files of one vault.
type Access struct {
	Role  string       // base role (custom roles map to their base)
	Rules []AccessRule // empty when nothing is limited
	Rev   int64        // the vault's access revision
}

// Restricted reports whether any rule applies (then the user does not see
// everything, or may not write everywhere their role allows).
func (a *Access) Restricted() bool { return len(a.Rules) > 0 }

// level returns the access level for p.
func (a *Access) level(p string) string {
	f := pathutil.Fold(p)
	best, lvl, member := -1, "", false
	for _, r := range a.Rules {
		rf := pathutil.Fold(r.Path)
		if rf != "" && f != rf && !strings.HasPrefix(f, rf+"/") {
			continue
		}
		if n := len(rf); n > best || (n == best && r.Member && !member) {
			best, lvl, member = n, r.Level, r.Member
		}
	}
	if best < 0 {
		if RoleAtLeast(a.Role, RoleEditor) {
			return LevelWrite
		}
		return LevelRead
	}
	return lvl
}

// CanRead reports whether the file at p is visible.
func (a *Access) CanRead(p string) bool { return a.level(p) != LevelNone }

// CanWrite reports whether the file at p may be created, changed or deleted.
func (a *Access) CanWrite(p string) bool {
	return RoleAtLeast(a.Role, RoleEditor) && a.level(p) == LevelWrite
}

// Effective returns the rules that apply, one per path (member exceptions
// replace the role's rule on the same path), sorted by path. Devices get
// this list to know what they may keep and upload.
func (a *Access) Effective() []AccessRule {
	byPath := map[string]AccessRule{}
	for _, r := range a.Rules {
		f := pathutil.Fold(r.Path)
		if cur, ok := byPath[f]; !ok || (r.Member && !cur.Member) {
			byPath[f] = r
		}
	}
	out := make([]AccessRule, 0, len(byPath))
	for _, r := range byPath {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Access returns what u may do in a vault, or nil when u is not a member.
func (s *Store) Access(ctx context.Context, u *User, vaultID int64) (*Access, error) {
	role, err := s.Role(ctx, u, vaultID)
	if err != nil || role == "" {
		return nil, err
	}
	a := &Access{Role: role}
	var enabled bool
	if err := s.db.QueryRowContext(ctx, "SELECT folder_access, access_rev FROM vaults WHERE id = ?", vaultID).Scan(&enabled, &a.Rev); err != nil {
		return nil, err
	}
	if !enabled || role == RoleOwner {
		return a, nil
	}
	var roleID int64
	err = s.db.QueryRowContext(ctx, "SELECT COALESCE(role_id, 0) FROM vault_members WHERE vault_id = ? AND user_id = ?", vaultID, u.ID).Scan(&roleID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, level, user_id IS NOT NULL FROM vault_access_rules
		WHERE vault_id = ? AND (role_id = ? OR user_id = ?)`, vaultID, roleID, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r AccessRule
		if err := rows.Scan(&r.ID, &r.Path, &r.Level, &r.Member); err != nil {
			return nil, err
		}
		a.Rules = append(a.Rules, r)
	}
	return a, rows.Err()
}

func bumpAccess(tx *sql.Tx, vaultID int64) error {
	_, err := tx.Exec("UPDATE vaults SET access_rev = access_rev + 1 WHERE id = ?", vaultID)
	return err
}

// AccessRev returns the vault's access revision (0 when it does not exist).
func (s *Store) AccessRev(ctx context.Context, vaultID int64) int64 {
	var rev int64
	s.db.QueryRowContext(ctx, "SELECT access_rev FROM vaults WHERE id = ?", vaultID).Scan(&rev)
	return rev
}

// SetFolderAccess switches per-folder access rules on or off for a vault.
// Roles and rules are kept while it is off; they just do not apply.
func (s *Store) SetFolderAccess(ctx context.Context, vaultID int64, on bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("UPDATE vaults SET folder_access = ? WHERE id = ? AND folder_access != ?", boolInt(on), vaultID, boolInt(on))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return bumpAccess(tx, vaultID)
	})
}

// ---- custom roles ----

func cleanRole(name, base string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 60 {
		return "", ErrInvalidRoleName
	}
	if base != RoleViewer && base != RoleEditor {
		return "", ErrInvalidRole
	}
	return name, nil
}

func roleErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrRoleNameTaken
	}
	return err
}

func (s *Store) CreateVaultRole(ctx context.Context, vaultID int64, name, base string) (*VaultRole, error) {
	name, err := cleanRole(name, base)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO vault_roles(vault_id, name, base) VALUES(?, ?, ?)", vaultID, name, base)
	if err != nil {
		return nil, roleErr(err)
	}
	id, _ := res.LastInsertId()
	return &VaultRole{ID: id, VaultID: vaultID, Name: name, Base: base}, nil
}

// UpdateVaultRole renames a role or changes its base; its members follow.
func (s *Store) UpdateVaultRole(ctx context.Context, vaultID, id int64, name, base string) error {
	name, err := cleanRole(name, base)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("UPDATE vault_roles SET name = ?, base = ? WHERE id = ? AND vault_id = ?", name, base, id, vaultID)
		if err != nil {
			return roleErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec("UPDATE vault_members SET role = ? WHERE role_id = ?", base, id); err != nil {
			return err
		}
		return bumpAccess(tx, vaultID)
	})
}

// DeleteVaultRole deletes a role with its rules. Its members keep the base
// role without folder limits (other than their own exceptions).
func (s *Store) DeleteVaultRole(ctx context.Context, vaultID, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE vault_members SET role_id = NULL WHERE role_id = ? AND vault_id = ?", id, vaultID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM vault_roles WHERE id = ? AND vault_id = ?", id, vaultID); err != nil {
			return err
		}
		return bumpAccess(tx, vaultID)
	})
}

// VaultRole returns one custom role of a vault (without rules).
func (s *Store) VaultRole(ctx context.Context, vaultID, id int64) (*VaultRole, error) {
	r := &VaultRole{ID: id, VaultID: vaultID}
	err := s.db.QueryRowContext(ctx, "SELECT name, base FROM vault_roles WHERE id = ? AND vault_id = ?", id, vaultID).Scan(&r.Name, &r.Base)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// VaultRoles lists the custom roles of a vault with their rules and number
// of members.
func (s *Store) VaultRoles(ctx context.Context, vaultID int64) ([]*VaultRole, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.name, r.base, (SELECT COUNT(*) FROM vault_members m WHERE m.role_id = r.id)
		FROM vault_roles r WHERE r.vault_id = ? ORDER BY r.name`, vaultID)
	if err != nil {
		return nil, err
	}
	var out []*VaultRole
	byID := map[int64]*VaultRole{}
	for rows.Next() {
		r := &VaultRole{VaultID: vaultID}
		if err := rows.Scan(&r.ID, &r.Name, &r.Base, &r.Members); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
		byID[r.ID] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT id, role_id, path, level FROM vault_access_rules WHERE vault_id = ? AND role_id IS NOT NULL ORDER BY path_fold", vaultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rule AccessRule
		var roleID int64
		if err := rows.Scan(&rule.ID, &roleID, &rule.Path, &rule.Level); err != nil {
			return nil, err
		}
		if r := byID[roleID]; r != nil {
			r.Rules = append(r.Rules, rule)
		}
	}
	return out, rows.Err()
}

// SetMemberCustomRole gives a member a custom role of the vault.
func (s *Store) SetMemberCustomRole(ctx context.Context, vaultID, userID, roleID int64) error {
	r, err := s.VaultRole(ctx, vaultID, roleID)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO vault_members(vault_id, user_id, role, role_id) VALUES(?, ?, ?, ?) ON CONFLICT(vault_id, user_id) DO UPDATE SET role = excluded.role, role_id = excluded.role_id",
			vaultID, userID, r.Base, r.ID); err != nil {
			return err
		}
		return bumpAccess(tx, vaultID)
	})
}

// ---- rules ----

// RulePath normalizes a folder or file path typed in the admin; "" and "/"
// mean the whole vault.
func RulePath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	n, err := pathutil.Normalize(p)
	if err != nil {
		return "", ErrInvalidRule
	}
	return n, nil
}

// SetAccessRule adds or replaces the rule for path of a custom role (roleID)
// or of one member (userID); exactly one of them is set.
func (s *Store) SetAccessRule(ctx context.Context, vaultID, roleID, userID int64, path, level string) error {
	path, err := RulePath(path)
	if err != nil || !ValidLevel(level) || (roleID == 0) == (userID == 0) {
		return ErrInvalidRule
	}
	if roleID != 0 {
		if _, err := s.VaultRole(ctx, vaultID, roleID); err != nil {
			return err
		}
	} else {
		var n int
		s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM vault_members WHERE vault_id = ? AND user_id = ?", vaultID, userID).Scan(&n)
		if n == 0 {
			return ErrNotFound
		}
	}
	fold := pathutil.Fold(path)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM vault_access_rules WHERE vault_id = ? AND role_id IS ? AND user_id IS ? AND path_fold = ?",
			vaultID, nullID(roleID), nullID(userID), fold); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO vault_access_rules(vault_id, role_id, user_id, path, path_fold, level) VALUES(?, ?, ?, ?, ?, ?)",
			vaultID, nullID(roleID), nullID(userID), path, fold, level); err != nil {
			return err
		}
		return bumpAccess(tx, vaultID)
	})
}

func (s *Store) RemoveAccessRule(ctx context.Context, vaultID, ruleID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM vault_access_rules WHERE id = ? AND vault_id = ?", ruleID, vaultID); err != nil {
			return err
		}
		return bumpAccess(tx, vaultID)
	})
}

// memberExceptions returns the members' own rules by user id.
func (s *Store) memberExceptions(ctx context.Context, vaultID int64) (map[int64][]AccessRule, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, user_id, path, level FROM vault_access_rules WHERE vault_id = ? AND user_id IS NOT NULL ORDER BY path_fold", vaultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]AccessRule{}
	for rows.Next() {
		var r AccessRule
		var uid int64
		if err := rows.Scan(&r.ID, &uid, &r.Path, &r.Level); err != nil {
			return nil, err
		}
		r.Member = true
		out[uid] = append(out[uid], r)
	}
	return out, rows.Err()
}

// MoveAccessRules makes rules for the folder (or file) from, and anything
// under it, follow a rename to to.
func (s *Store) MoveAccessRules(ctx context.Context, vaultID int64, from, to string) error {
	ff := pathutil.Fold(from)
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT id, path FROM vault_access_rules WHERE vault_id = ? AND (path_fold = ? OR substr(path_fold, 1, ?) = ?)",
			vaultID, ff, len(ff)+1, ff+"/")
		if err != nil {
			return err
		}
		type mv struct {
			id   int64
			path string
		}
		var list []mv
		for rows.Next() {
			var m mv
			if err := rows.Scan(&m.id, &m.path); err != nil {
				rows.Close()
				return err
			}
			list = append(list, m)
		}
		rows.Close()
		if len(list) == 0 {
			return nil
		}
		for _, m := range list {
			if len(m.path) < len(from) || pathutil.Fold(m.path[:len(from)]) != ff {
				continue // letter case changed the length; leave it alone
			}
			np := to + m.path[len(from):]
			if _, err := tx.Exec("UPDATE vault_access_rules SET path = ?, path_fold = ? WHERE id = ?", np, pathutil.Fold(np), m.id); err != nil {
				return err
			}
		}
		return bumpAccess(tx, vaultID)
	})
}

// ---- scoped file access ----

// Scoped reads and writes the files of one vault as one user: whatever the
// user may not see does not exist, and writes outside what they may change
// fail with ErrCodeForbidden. Every request that touches files on behalf of
// a user goes through it.
type Scoped struct {
	s       *Store
	VaultID int64
	A       *Access
}

func (s *Store) Scope(vaultID int64, a *Access) *Scoped {
	return &Scoped{s: s, VaultID: vaultID, A: a}
}

func (sc *Scoped) filter(in []FileEntry) []FileEntry {
	if !sc.A.Restricted() {
		return in
	}
	out := in[:0:0]
	for _, f := range in {
		if sc.A.CanRead(f.Path) {
			out = append(out, f)
		}
	}
	return out
}

func (sc *Scoped) ListFiles(ctx context.Context, deleted bool) ([]FileEntry, error) {
	files, err := sc.s.ListFiles(ctx, sc.VaultID, deleted)
	return sc.filter(files), err
}

func (sc *Scoped) File(ctx context.Context, path string) (*FileEntry, error) {
	if !sc.A.CanRead(path) {
		return nil, ErrNotFound
	}
	return sc.s.File(ctx, sc.VaultID, path)
}

func (sc *Scoped) Versions(ctx context.Context, path string) ([]Version, error) {
	if !sc.A.CanRead(path) {
		return nil, nil
	}
	return sc.s.Versions(ctx, sc.VaultID, path)
}

func (sc *Scoped) Version(ctx context.Context, id int64) (*Version, error) {
	v, err := sc.s.Version(ctx, sc.VaultID, id)
	if err != nil {
		return nil, err
	}
	if !sc.A.CanRead(v.Path) {
		return nil, ErrNotFound
	}
	return v, nil
}

// RecentVersions returns up to limit of the latest visible changes.
func (sc *Scoped) RecentVersions(ctx context.Context, limit int) ([]Version, error) {
	if !sc.A.Restricted() {
		return sc.s.RecentVersions(ctx, sc.VaultID, limit)
	}
	all, err := sc.s.RecentVersions(ctx, sc.VaultID, limit*10)
	var out []Version
	for _, v := range all {
		if len(out) == limit {
			break
		}
		if sc.A.CanRead(v.Path) {
			out = append(out, v)
		}
	}
	return out, err
}

// Changes is Store.Changes without invisible entries. A page that would
// come out empty is skipped, because a client takes an empty page as the end.
func (sc *Scoped) Changes(ctx context.Context, since int64, limit int) ([]FileEntry, int64, bool, error) {
	for {
		raw, head, more, err := sc.s.Changes(ctx, sc.VaultID, since, limit)
		if err != nil || !sc.A.Restricted() {
			return raw, head, more, err
		}
		out := sc.filter(raw)
		if len(out) > 0 || !more {
			return out, head, more, nil
		}
		since = raw[len(raw)-1].Rev
	}
}

// Commit is Store.Commit, refusing ops on paths the user may not write.
func (sc *Scoped) Commit(ctx context.Context, ops []Op, author Author, hasBlob func(string) bool) ([]OpResult, int64, error) {
	results := make([]OpResult, len(ops))
	var allowed []Op
	var idx []int
	for i, op := range ops {
		if p, err := pathutil.Normalize(op.Path); err == nil && !sc.A.CanWrite(p) {
			results[i] = OpResult{Path: p, Error: ErrCodeForbidden}
			continue
		}
		allowed = append(allowed, op)
		idx = append(idx, i)
	}
	res, head, err := sc.s.Commit(ctx, sc.VaultID, allowed, author, hasBlob)
	if err != nil {
		return nil, 0, err
	}
	for j, r := range res {
		results[idx[j]] = r
	}
	return results, head, nil
}

// HashReadable reports whether the user may download content hash: some
// file (or old version of one) they can see has it. Unlimited members may
// also download content of backups, as before.
func (sc *Scoped) HashReadable(ctx context.Context, hash string) (bool, error) {
	if !sc.A.Restricted() {
		return sc.s.HashInVault(ctx, sc.VaultID, hash)
	}
	rows, err := sc.s.db.QueryContext(ctx, "SELECT path FROM files WHERE vault_id = ?1 AND hash = ?2 UNION SELECT path FROM file_versions WHERE vault_id = ?1 AND hash = ?2", sc.VaultID, hash)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return false, err
		}
		if sc.A.CanRead(p) {
			return true, nil
		}
	}
	return false, rows.Err()
}
