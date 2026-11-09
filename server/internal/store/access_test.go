package store

import (
	"context"
	"testing"
)

func TestAccessLevels(t *testing.T) {
	a := &Access{Role: RoleEditor, Rules: []AccessRule{
		{Path: "", Level: LevelNone},
		{Path: "Program 2029", Level: LevelRead},
		{Path: "Program 2029/Kampaň", Level: LevelWrite},
		{Path: "Program 2029/Finance", Level: LevelNone},
		{Path: "Program 2029/Finance", Level: LevelRead, Member: true},
	}}
	for _, c := range []struct {
		path        string
		read, write bool
	}{
		{"Notes.md", false, false},
		{"Program 2029/Plan.md", true, false},
		{"program 2029/plan.md", true, false}, // letter case does not matter
		{"Program 2029/Kampaň/Text.md", true, true},
		{"Program 2029/Kampan/Text.md", true, false}, // a different folder
		{"Program 2029/Finance/Rozpočet.md", true, false},
		{"Program 2029 old/X.md", false, false}, // prefix of a name is not a folder
	} {
		if a.CanRead(c.path) != c.read || a.CanWrite(c.path) != c.write {
			t.Errorf("%s: read=%v write=%v, want %v %v", c.path, a.CanRead(c.path), a.CanWrite(c.path), c.read, c.write)
		}
	}
	// A rule never grants more than the base role.
	v := &Access{Role: RoleViewer, Rules: []AccessRule{{Path: "A", Level: LevelWrite}}}
	if !v.CanRead("A/x.md") || v.CanWrite("A/x.md") || v.CanWrite("b.md") {
		t.Fatal("viewer can write")
	}
	if eff := a.Effective(); len(eff) != 4 || eff[2].Path != "Program 2029/Finance" || eff[2].Level != LevelRead {
		t.Fatalf("effective: %+v", eff)
	}
}

func TestFolderAccessStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	owner, _ := s.CreateUser(ctx, "owner", "x", false)
	eva, _ := s.CreateUser(ctx, "eva", "x", false)
	v, _ := s.CreateVault(ctx, "Work", DefaultBackupPolicy, owner.ID)
	a := Author{Name: "t"}
	files := []Op{
		{Path: "Public/a.md", Hash: hashOf("a"), Size: 1},
		{Path: "Secret/b.md", Hash: hashOf("b"), Size: 1},
		{Path: "Shared/c.md", Hash: hashOf("c"), Size: 1},
	}
	if _, _, err := s.Commit(ctx, v.ID, files, a, yes); err != nil {
		t.Fatal(err)
	}
	s.SetMember(ctx, v.ID, eva.ID, RoleEditor)

	role, err := s.CreateVaultRole(ctx, v.ID, "Finance", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateVaultRole(ctx, v.ID, "finance", RoleViewer); err != ErrRoleNameTaken {
		t.Fatalf("duplicate role name: %v", err)
	}
	if err := s.SetAccessRule(ctx, v.ID, role.ID, 0, "/Secret/", LevelNone); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberCustomRole(ctx, v.ID, eva.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccessRule(ctx, v.ID, 0, eva.ID, "Shared", LevelRead); err != nil {
		t.Fatal(err)
	}

	// Switched off: rules exist but do not apply.
	acc, _ := s.Access(ctx, eva, v.ID)
	if acc.Restricted() || acc.Role != RoleEditor {
		t.Fatalf("rules apply while off: %+v", acc)
	}
	rev := acc.Rev
	s.SetFolderAccess(ctx, v.ID, true)
	acc, _ = s.Access(ctx, eva, v.ID)
	if !acc.Restricted() || acc.Rev <= rev {
		t.Fatalf("rules do not apply when on: %+v", acc)
	}
	// Owners are never limited.
	if oa, _ := s.Access(ctx, owner, v.ID); oa.Restricted() {
		t.Fatal("owner is limited")
	}

	sc := s.Scope(v.ID, acc)
	list, _ := sc.ListFiles(ctx, false)
	if len(list) != 2 || list[0].Path != "Public/a.md" || list[1].Path != "Shared/c.md" {
		t.Fatalf("visible files: %+v", list)
	}
	if _, err := sc.File(ctx, "Secret/b.md"); err != ErrNotFound {
		t.Fatalf("hidden file readable: %v", err)
	}
	if ok, _ := sc.HashReadable(ctx, hashOf("b")); ok {
		t.Fatal("hidden content readable by hash")
	}
	if ok, _ := sc.HashReadable(ctx, hashOf("a")); !ok {
		t.Fatal("visible content not readable")
	}
	ch, head, more, _ := sc.Changes(ctx, 0, 10)
	if len(ch) != 2 || more || head != 3 {
		t.Fatalf("changes: %+v head=%d more=%v", ch, head, more)
	}
	// A page with only hidden entries is skipped, not returned empty.
	ch, _, more, _ = sc.Changes(ctx, 1, 1)
	if len(ch) != 1 || ch[0].Path != "Shared/c.md" || more {
		t.Fatalf("skipping hidden page: %+v more=%v", ch, more)
	}
	if vs, _ := sc.RecentVersions(ctx, 10); len(vs) != 2 {
		t.Fatalf("recent: %+v", vs)
	}
	res, _, _ := sc.Commit(ctx, []Op{
		{Path: "Public/new.md", Hash: hashOf("d"), Size: 1},
		{Path: "Secret/x.md", Hash: hashOf("d"), Size: 1},
		{Path: "shared/c.md", Hash: hashOf("d"), Size: 1, BaseHash: hashOf("c")},
	}, a, yes)
	if !res[0].OK || res[1].Error != ErrCodeForbidden || res[2].Error != ErrCodeForbidden {
		t.Fatalf("commit: %+v", res)
	}

	// Renaming a folder in the web editor moves its rules.
	s.MoveAccessRules(ctx, v.ID, "Secret", "Tajné")
	acc, _ = s.Access(ctx, eva, v.ID)
	if acc.CanRead("Tajné/b.md") || !acc.CanRead("Secret/b.md") {
		t.Fatal("rule did not move")
	}

	// Deleting the role leaves the base role and the member's exceptions.
	s.DeleteVaultRole(ctx, v.ID, role.ID)
	acc, _ = s.Access(ctx, eva, v.ID)
	if acc.Role != RoleEditor || len(acc.Rules) != 1 || !acc.CanRead("Tajné/b.md") {
		t.Fatalf("after role delete: %+v", acc)
	}
	ms, _ := s.Members(ctx, v.ID)
	for _, m := range ms {
		if m.UserID == eva.ID && (m.RoleID != 0 || len(m.Exceptions) != 1) {
			t.Fatalf("member: %+v", m)
		}
	}
	// Removing the member removes the exceptions too.
	s.RemoveMember(ctx, v.ID, eva.ID)
	s.SetMember(ctx, v.ID, eva.ID, RoleViewer)
	if acc, _ = s.Access(ctx, eva, v.ID); acc.Restricted() {
		t.Fatalf("exceptions survived removal: %+v", acc)
	}
}
