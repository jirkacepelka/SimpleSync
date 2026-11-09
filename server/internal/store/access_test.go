package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jirkacepelka/obsisync/server/internal/pathutil"
)

// longest evaluates rules the way devices do: the longest matching path wins.
func longest(rules []AccessRule, p string) string {
	f, best, lvl := pathutil.Fold(p), -1, LevelNone
	for _, r := range rules {
		rf := pathutil.Fold(r.Path)
		if covers(rf, f) && len(rf) > best {
			best, lvl = len(rf), r.Level
		}
	}
	return lvl
}

func TestAccessLevels(t *testing.T) {
	a := &Access{Role: RoleEditor, Limited: true, Grants: []AccessRule{
		{Path: "Školství", Level: LevelWrite},
		{Path: "Společné", Level: LevelRead},
		{Path: "Program", Level: LevelRead},
		{Path: "Program/Kampaň", Level: LevelWrite},
		{Path: "Doprava", Level: LevelWrite},
		{Path: "Doprava/Archiv", Level: LevelRead}, // a narrower grant never takes anything away
	}}
	for _, c := range []struct {
		path        string
		read, write bool
	}{
		{"Jiné.md", false, false},
		{"Školství/Plán.md", true, true},
		{"školství/plán.md", true, true}, // letter case does not matter
		{"Školství staré/X.md", false, false},
		{"Společné/Kontakty.md", true, false},
		{"Program/Body.md", true, false},
		{"Program/Kampaň/Text.md", true, true},
		{"Doprava/Archiv/2020.md", true, true},
	} {
		if a.CanRead(c.path) != c.read || a.CanWrite(c.path) != c.write {
			t.Errorf("%s: read=%v write=%v, want %v %v", c.path, a.CanRead(c.path), a.CanWrite(c.path), c.read, c.write)
		}
		// What devices compute from Effective() must agree.
		if got := longest(a.Effective(), c.path); got != a.level(c.path) {
			t.Errorf("%s: devices see %s, server %s", c.path, got, a.level(c.path))
		}
	}
	// Read only in the vault never edits.
	v := &Access{Role: RoleViewer, Limited: true, Grants: []AccessRule{{Path: "A", Level: LevelWrite}}}
	if !v.CanRead("A/x.md") || v.CanWrite("A/x.md") {
		t.Fatal("viewer can write")
	}
	// Nothing given: nothing visible. Not limited: everything.
	if (&Access{Role: RoleEditor, Limited: true}).CanRead("x.md") {
		t.Fatal("limited member without folders sees something")
	}
	if !(&Access{Role: RoleViewer}).CanRead("x.md") || len((&Access{Role: RoleViewer}).Effective()) != 0 {
		t.Fatal("unlimited member")
	}
	whole := &Access{Role: RoleEditor, Limited: true, Grants: []AccessRule{{Path: "", Level: LevelRead}, {Path: "A", Level: LevelWrite}}}
	if !whole.CanRead("x.md") || whole.CanWrite("x.md") || !whole.CanWrite("A/y.md") {
		t.Fatal("whole vault for reading plus a folder for editing")
	}
}

func TestFolderAccessStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	owner, _ := s.CreateUser(ctx, "owner", "x", false)
	eva, _ := s.CreateUser(ctx, "eva", "x", false)
	v, _ := s.CreateVault(ctx, "Kraj", DefaultBackupPolicy, owner.ID)
	a := Author{Name: "t"}
	files := []Op{
		{Path: "Školství/a.md", Hash: hashOf("a"), Size: 1},
		{Path: "Doprava/b.md", Hash: hashOf("b"), Size: 1},
		{Path: "Společné/c.md", Hash: hashOf("c"), Size: 1},
		{Path: "Vedení/d.md", Hash: hashOf("d"), Size: 1},
	}
	if _, _, err := s.Commit(ctx, v.ID, files, a, yes); err != nil {
		t.Fatal(err)
	}
	s.SetMember(ctx, v.ID, eva.ID, RoleEditor)

	skol, err := s.CreateVaultRole(ctx, v.ID, "Školství")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateVaultRole(ctx, v.ID, "školství"); err != ErrRoleNameTaken {
		t.Fatalf("duplicate role name: %v", err)
	}
	dop, _ := s.CreateVaultRole(ctx, v.ID, "Doprava")
	s.SetAccessRule(ctx, v.ID, skol.ID, 0, "/Školství/", LevelWrite)
	s.SetAccessRule(ctx, v.ID, skol.ID, 0, "Společné", LevelRead)
	s.SetAccessRule(ctx, v.ID, dop.ID, 0, "Doprava", LevelWrite)
	if err := s.SetAccessRule(ctx, v.ID, skol.ID, 0, "x", LevelNone); err != ErrInvalidRule {
		t.Fatalf("level none accepted: %v", err)
	}

	// Switched off: roles exist but do not apply.
	acc, _ := s.Access(ctx, eva, v.ID)
	if acc.Restricted() {
		t.Fatalf("limited while off: %+v", acc)
	}
	rev := acc.Rev
	s.SetFolderAccess(ctx, v.ID, true)
	acc, _ = s.Access(ctx, eva, v.ID)
	if !acc.Restricted() || acc.Rev <= rev || acc.CanRead("Školství/a.md") {
		t.Fatalf("a member without roles must see nothing: %+v", acc)
	}
	if oa, _ := s.Access(ctx, owner, v.ID); oa.Restricted() {
		t.Fatal("owner is limited")
	}

	// Two roles and a folder of her own.
	if err := s.SetMemberRoles(ctx, v.ID, eva.ID, []int64{skol.ID, dop.ID, 9999}); err != nil {
		t.Fatal(err)
	}
	s.SetAccessRule(ctx, v.ID, 0, eva.ID, "Společné", LevelWrite)
	acc, _ = s.Access(ctx, eva, v.ID)
	sc := s.Scope(v.ID, acc)
	list, _ := sc.ListFiles(ctx, false)
	var paths []string
	for _, f := range list {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, "|") != "Doprava/b.md|Společné/c.md|Školství/a.md" {
		t.Fatalf("visible files: %v", paths)
	}
	if !acc.CanWrite("Společné/c.md") {
		t.Fatal("her own folder for editing must beat the role's reading")
	}
	if _, err := sc.File(ctx, "Vedení/d.md"); err != ErrNotFound {
		t.Fatalf("hidden file readable: %v", err)
	}
	if ok, _ := sc.HashReadable(ctx, hashOf("d")); ok {
		t.Fatal("hidden content readable by hash")
	}
	ch, _, more, _ := sc.Changes(ctx, 3, 1) // only "Vedení" after rev 3
	if len(ch) != 0 || more {
		t.Fatalf("hidden tail: %+v", ch)
	}
	res, _, _ := sc.Commit(ctx, []Op{
		{Path: "Školství/new.md", Hash: hashOf("e"), Size: 1},
		{Path: "Vedení/x.md", Hash: hashOf("e"), Size: 1},
	}, a, yes)
	if !res[0].OK || res[1].Error != ErrCodeForbidden {
		t.Fatalf("commit: %+v", res)
	}
	ms, _ := s.Members(ctx, v.ID)
	for _, m := range ms {
		if m.UserID == eva.ID && (len(m.RoleIDs) != 2 || !m.HasRole(dop.ID) || len(m.Folders) != 1) {
			t.Fatalf("member: %+v", m)
		}
	}
	roles, _ := s.VaultRoles(ctx, v.ID)
	if len(roles) != 2 || roles[1].Name != "Školství" || len(roles[1].Rules) != 2 || roles[1].Members != 1 {
		t.Fatalf("roles: %+v", roles[1])
	}

	// Renaming a folder in the web editor moves what was given.
	s.MoveAccessRules(ctx, v.ID, "Doprava", "Doprava a silnice")
	acc, _ = s.Access(ctx, eva, v.ID)
	if !acc.CanWrite("Doprava a silnice/b.md") || acc.CanRead("Doprava/b.md") {
		t.Fatal("folder did not move")
	}
	// Deleting a role takes its folders away.
	s.DeleteVaultRole(ctx, v.ID, dop.ID)
	if acc, _ = s.Access(ctx, eva, v.ID); acc.CanRead("Doprava a silnice/b.md") {
		t.Fatal("deleted role still gives its folders")
	}
	// Removing the member removes roles and folders of her own.
	s.RemoveMember(ctx, v.ID, eva.ID)
	s.SetMember(ctx, v.ID, eva.ID, RoleViewer)
	if acc, _ = s.Access(ctx, eva, v.ID); len(acc.Grants) != 0 {
		t.Fatalf("access survived removal: %+v", acc)
	}
}

// TestMigrateFolderAccess converts rules of 0.8.0 without anyone losing access.
func TestMigrateFolderAccess(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	owner, _ := s.CreateUser(ctx, "owner", "x", false)
	ana, _ := s.CreateUser(ctx, "ana", "x", false) // role that hid everything but one folder
	bob, _ := s.CreateUser(ctx, "bob", "x", false) // role that only hid one folder
	cyr, _ := s.CreateUser(ctx, "cyr", "x", false) // no role, no rules: saw everything
	v, _ := s.CreateVault(ctx, "Kraj", DefaultBackupPolicy, owner.ID)
	for _, u := range []*User{ana, bob, cyr} {
		s.SetMember(ctx, v.ID, u.ID, RoleEditor)
	}
	exec := func(q string, args ...any) {
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE vaults SET folder_access = 1 WHERE id = ?", v.ID)
	exec("INSERT INTO vault_roles(id, vault_id, name, base) VALUES(1, ?, 'Tým', 'editor'), (2, ?, 'Skoro vše', 'viewer')", v.ID, v.ID)
	exec("UPDATE vault_members SET role_id = 1 WHERE user_id = ?", ana.ID)
	exec("UPDATE vault_members SET role_id = 2 WHERE user_id = ?", bob.ID)
	for _, r := range []struct {
		role      int64
		path, lvl string
	}{{1, "", "none"}, {1, "Kampaň", "write"}, {2, "Finance", "none"}} {
		exec("INSERT INTO vault_access_rules(vault_id, role_id, path, path_fold, level) VALUES(?, ?, ?, ?, ?)", v.ID, r.role, r.path, pathutil.Fold(r.path), r.lvl)
	}
	exec("DELETE FROM settings WHERE key = 'folder_access_model'")
	if err := migrateFolderAccess(s.db); err != nil {
		t.Fatal(err)
	}
	if err := migrateFolderAccess(s.db); err != nil { // runs once
		t.Fatal(err)
	}
	check := func(u *User, p string, read, write bool) {
		t.Helper()
		acc, _ := s.Access(ctx, u, v.ID)
		if acc.CanRead(p) != read || acc.CanWrite(p) != write {
			t.Errorf("%s %s: read=%v write=%v, want %v %v", u.Username, p, acc.CanRead(p), acc.CanWrite(p), read, write)
		}
	}
	check(ana, "Kampaň/x.md", true, true)
	check(ana, "Jiné.md", false, false)
	check(bob, "Jiné.md", true, false) // whole vault for reading (the role was based on Read only)
	check(bob, "Finance/x.md", true, false)
	check(cyr, "Jiné.md", true, true)
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM vault_access_rules WHERE level = 'none'").Scan(&n)
	if n != 0 {
		t.Fatal("hiding rules left over")
	}
}
