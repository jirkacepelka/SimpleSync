package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/jirkacepelka/obsisync/server/internal/pathutil"
)

// Folder access: when a vault has it switched on, members who are not
// owners see nothing until they are given folders. Folders come from the
// member's roles (a member can have several) and from folders given to the
// member alone. Each folder is given for reading or for editing; a folder
// includes everything in it, and "" stands for the whole vault. A member
// sees the sum of everything given, with the higher level winning. A
// member whose vault role is Read only never edits, whatever is given.
// Owners and administrators always see everything.

// Access levels of a folder.
const (
	LevelNone  = "none"
	LevelRead  = "read"
	LevelWrite = "write"
)

// ValidLevel reports whether l can be given for a folder.
func ValidLevel(l string) bool { return l == LevelRead || l == LevelWrite }

// ErrCodeForbidden is returned per op when the path may not be written.
const ErrCodeForbidden = "forbidden"

var (
	ErrRoleNameTaken   = errors.New("err.roleNameTaken")
	ErrInvalidRoleName = errors.New("err.invalidRoleName")
	ErrInvalidRule     = errors.New("err.invalidRule")
)

// AccessRule gives a folder or file ("" = the whole vault) for reading or
// editing. In the list sent to devices, Level can also be LevelNone.
type AccessRule struct {
	ID    int64  `json:"-"`
	Path  string `json:"path"`
	Level string `json:"level"`
}

// VaultRole is a custom role of one vault: a name and the folders it gives.
type VaultRole struct {
	ID      int64
	VaultID int64
	Name    string
	Rules   []AccessRule
	Members int
}

// Access says what one user may do with the files of one vault.
type Access struct {
	Role    string       // the vault role: owner, editor or viewer
	Limited bool         // folder access applies to this user
	Grants  []AccessRule // folders given, from roles and to the member alone
	Rev     int64        // the vault's access revision
}

// Restricted reports whether the user is limited to the folders given.
func (a *Access) Restricted() bool { return a.Limited }

func covers(folder, p string) bool {
	return folder == "" || p == folder || strings.HasPrefix(p, folder+"/")
}

// level returns the access level for p.
func (a *Access) level(p string) string {
	if !a.Limited {
		if RoleAtLeast(a.Role, RoleEditor) {
			return LevelWrite
		}
		return LevelRead
	}
	f, lvl := pathutil.Fold(p), LevelNone
	for _, g := range a.Grants {
		if covers(pathutil.Fold(g.Path), f) && (lvl == LevelNone || g.Level == LevelWrite) {
			lvl = g.Level
		}
	}
	return lvl
}

// CanRead reports whether the file at p is visible.
func (a *Access) CanRead(p string) bool { return a.level(p) != LevelNone }

// CanWrite reports whether the file at p may be created, changed or deleted.
func (a *Access) CanWrite(p string) bool {
	return RoleAtLeast(a.Role, RoleEditor) && a.level(p) == LevelWrite
}

// Effective returns the access as a list of rules where the longest matching
// path decides (the form devices understand): the whole vault, then every
// folder given with the highest level that applies to it. Empty when the
// user is not limited.
func (a *Access) Effective() []AccessRule {
	if !a.Limited {
		return []AccessRule{}
	}
	paths := map[string]string{"": ""}
	for _, g := range a.Grants {
		paths[pathutil.Fold(g.Path)] = g.Path
	}
	out := make([]AccessRule, 0, len(paths))
	for f, p := range paths {
		lvl := LevelNone
		for _, g := range a.Grants {
			if covers(pathutil.Fold(g.Path), f) && (lvl == LevelNone || g.Level == LevelWrite) {
				lvl = g.Level
			}
		}
		out = append(out, AccessRule{Path: p, Level: lvl})
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
	a.Limited = true
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, level FROM vault_access_rules WHERE vault_id = ?1 AND (user_id = ?2 OR role_id IN
		(SELECT role_id FROM vault_member_roles WHERE vault_id = ?1 AND user_id = ?2))`, vaultID, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r AccessRule
		if err := rows.Scan(&r.ID, &r.Path, &r.Level); err != nil {
			return nil, err
		}
		a.Grants = append(a.Grants, r)
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

// SetFolderAccess switches folder access on or off for a vault. Roles and
// folders are kept while it is off; they just do not apply.
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

// ---- roles ----

func cleanRoleName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 60 {
		return "", ErrInvalidRoleName
	}
	return name, nil
}

func roleErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrRoleNameTaken
	}
	return err
}

// roleNameTaken reports whether another role of the vault has the name,
// ignoring letter case (SQLite's NOCASE only folds ASCII letters).
func (s *Store) roleNameTaken(ctx context.Context, vaultID, exceptID int64, name string) bool {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name FROM vault_roles WHERE vault_id = ?", vaultID)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n string
		if rows.Scan(&id, &n) == nil && id != exceptID && pathutil.Fold(n) == pathutil.Fold(name) {
			return true
		}
	}
	return false
}

func (s *Store) CreateVaultRole(ctx context.Context, vaultID int64, name string) (*VaultRole, error) {
	name, err := cleanRoleName(name)
	if err != nil {
		return nil, err
	}
	if s.roleNameTaken(ctx, vaultID, 0, name) {
		return nil, ErrRoleNameTaken
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO vault_roles(vault_id, name, base) VALUES(?, ?, ?)", vaultID, name, RoleEditor)
	if err != nil {
		return nil, roleErr(err)
	}
	id, _ := res.LastInsertId()
	return &VaultRole{ID: id, VaultID: vaultID, Name: name}, nil
}

func (s *Store) RenameVaultRole(ctx context.Context, vaultID, id int64, name string) error {
	name, err := cleanRoleName(name)
	if err != nil {
		return err
	}
	if s.roleNameTaken(ctx, vaultID, id, name) {
		return ErrRoleNameTaken
	}
	res, err := s.db.ExecContext(ctx, "UPDATE vault_roles SET name = ? WHERE id = ? AND vault_id = ?", name, id, vaultID)
	if err != nil {
		return roleErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteVaultRole deletes a role; its members lose the folders it gave.
func (s *Store) DeleteVaultRole(ctx context.Context, vaultID, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM vault_roles WHERE id = ? AND vault_id = ?", id, vaultID); err != nil {
			return err
		}
		return bumpAccess(tx, vaultID)
	})
}

// VaultRole returns one custom role of a vault (without folders).
func (s *Store) VaultRole(ctx context.Context, vaultID, id int64) (*VaultRole, error) {
	r := &VaultRole{ID: id, VaultID: vaultID}
	err := s.db.QueryRowContext(ctx, "SELECT name FROM vault_roles WHERE id = ? AND vault_id = ?", id, vaultID).Scan(&r.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// VaultRoles lists the roles of a vault with their folders and number of
// members.
func (s *Store) VaultRoles(ctx context.Context, vaultID int64) ([]*VaultRole, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.name, (SELECT COUNT(*) FROM vault_member_roles m WHERE m.role_id = r.id)
		FROM vault_roles r WHERE r.vault_id = ? ORDER BY r.name`, vaultID)
	if err != nil {
		return nil, err
	}
	var out []*VaultRole
	byID := map[int64]*VaultRole{}
	for rows.Next() {
		r := &VaultRole{VaultID: vaultID}
		if err := rows.Scan(&r.ID, &r.Name, &r.Members); err != nil {
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

// SetMemberRoles replaces the roles of a member.
func (s *Store) SetMemberRoles(ctx context.Context, vaultID, userID int64, roleIDs []int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		tx.QueryRow("SELECT COUNT(*) FROM vault_members WHERE vault_id = ? AND user_id = ?", vaultID, userID).Scan(&n)
		if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec("DELETE FROM vault_member_roles WHERE vault_id = ? AND user_id = ?", vaultID, userID); err != nil {
			return err
		}
		for _, id := range roleIDs {
			// INSERT ... SELECT skips ids that are not roles of this vault.
			if _, err := tx.Exec("INSERT OR IGNORE INTO vault_member_roles(vault_id, user_id, role_id) SELECT ?, ?, id FROM vault_roles WHERE id = ? AND vault_id = ?",
				vaultID, userID, id, vaultID); err != nil {
				return err
			}
		}
		return bumpAccess(tx, vaultID)
	})
}

// ---- folders ----

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

// SetAccessRule gives path for reading or editing to a role (roleID) or to
// one member (userID); exactly one of them is set. Giving the same path
// again changes its level.
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

// SetAccessRuleLevel changes the level of one folder.
func (s *Store) SetAccessRuleLevel(ctx context.Context, vaultID, ruleID int64, level string) error {
	if !ValidLevel(level) {
		return ErrInvalidRule
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE vault_access_rules SET level = ? WHERE id = ? AND vault_id = ?", level, ruleID, vaultID); err != nil {
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

// memberAccess returns the roles and own folders of every member.
func (s *Store) memberAccess(ctx context.Context, vaultID int64) (map[int64][]int64, map[int64][]AccessRule, error) {
	roles := map[int64][]int64{}
	rows, err := s.db.QueryContext(ctx, "SELECT user_id, role_id FROM vault_member_roles WHERE vault_id = ?", vaultID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var uid, rid int64
		if err := rows.Scan(&uid, &rid); err != nil {
			rows.Close()
			return nil, nil, err
		}
		roles[uid] = append(roles[uid], rid)
	}
	rows.Close()
	folders := map[int64][]AccessRule{}
	rows, err = s.db.QueryContext(ctx, "SELECT id, user_id, path, level FROM vault_access_rules WHERE vault_id = ? AND user_id IS NOT NULL ORDER BY path_fold", vaultID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r AccessRule
		var uid int64
		if err := rows.Scan(&r.ID, &uid, &r.Path, &r.Level); err != nil {
			return nil, nil, err
		}
		folders[uid] = append(folders[uid], r)
	}
	return roles, folders, rows.Err()
}

// MoveAccessRules makes folders given at from, and anything under it,
// follow a rename to to.
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

// migrateFolderAccess converts the rules of 0.8.0 (one role per member,
// rules that hide or allow, the most specific one winning, members without
// rules seeing everything) to folders given (0.9.0). Allowing rules become
// folders; hiding rules are dropped, since nothing hides anymore. Whoever saw
// the whole vault apart from hidden folders is given the whole vault, so
// nobody loses access by the upgrade.
func migrateFolderAccess(db *sql.DB) error {
	var done int
	db.QueryRow("SELECT COUNT(*) FROM settings WHERE key = 'folder_access_model'").Scan(&done)
	if done > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	steps := []string{
		// Roles of members.
		`INSERT OR IGNORE INTO vault_member_roles(vault_id, user_id, role_id)
			SELECT vault_id, user_id, role_id FROM vault_members WHERE role_id IS NOT NULL`,
		// A role that did not hide the whole vault gave all of it.
		`INSERT INTO vault_access_rules(vault_id, role_id, path, path_fold, level)
			SELECT r.vault_id, r.id, '', '', CASE r.base WHEN 'viewer' THEN 'read' ELSE 'write' END FROM vault_roles r
			WHERE NOT EXISTS (SELECT 1 FROM vault_access_rules x WHERE x.role_id = r.id AND x.path = '')`,
		// So did a vault with folder access on to members without a role who
		// did not hide the whole vault themselves.
		`INSERT INTO vault_access_rules(vault_id, user_id, path, path_fold, level)
			SELECT m.vault_id, m.user_id, '', '', CASE m.role WHEN 'viewer' THEN 'read' ELSE 'write' END
			FROM vault_members m JOIN vaults v ON v.id = m.vault_id
			WHERE v.folder_access = 1 AND m.role != 'owner' AND m.role_id IS NULL
			AND NOT EXISTS (SELECT 1 FROM vault_access_rules x WHERE x.user_id = m.user_id AND x.vault_id = m.vault_id AND x.path = '')`,
		`DELETE FROM vault_access_rules WHERE level NOT IN ('read', 'write')`,
		`UPDATE vault_members SET role_id = NULL`,
		`UPDATE vaults SET access_rev = access_rev + 1 WHERE id IN (SELECT DISTINCT vault_id FROM vault_access_rules)`,
		`INSERT INTO settings(key, value) VALUES('folder_access_model', '2')`,
	}
	for _, q := range steps {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}
