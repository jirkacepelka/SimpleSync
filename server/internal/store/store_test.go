package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func hashOf(c string) string { return strings.Repeat(c, 64) }

func yes(string) bool { return true }

func TestCommitCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	v, err := s.CreateVault(ctx, "Test", DefaultBackupPolicy, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := Author{Name: "t"}

	res, head, err := s.Commit(ctx, v.ID, []Op{{Path: "a.md", Hash: hashOf("a"), Size: 1}}, a, yes)
	if err != nil || !res[0].OK || head != 1 {
		t.Fatalf("create: %+v head=%d err=%v", res, head, err)
	}
	// Stale base is rejected with the current state.
	res, _, _ = s.Commit(ctx, v.ID, []Op{{Path: "a.md", Hash: hashOf("b"), Size: 1, BaseHash: ""}}, a, yes)
	if res[0].OK || res[0].Error != ErrCodeConflict || res[0].Current.Hash != hashOf("a") {
		t.Fatalf("expected conflict, got %+v", res[0])
	}
	// Correct base succeeds.
	res, head, _ = s.Commit(ctx, v.ID, []Op{{Path: "a.md", Hash: hashOf("b"), Size: 1, BaseHash: hashOf("a")}}, a, yes)
	if !res[0].OK || head != 2 {
		t.Fatalf("update: %+v", res[0])
	}
	// Retrying the same change is idempotent and does not bump the revision.
	res, head, _ = s.Commit(ctx, v.ID, []Op{{Path: "a.md", Hash: hashOf("b"), Size: 1, BaseHash: hashOf("b")}}, a, yes)
	if !res[0].OK || head != 2 {
		t.Fatalf("idempotent: %+v head=%d", res[0], head)
	}
	// Delete, then re-create with empty base.
	res, _, _ = s.Commit(ctx, v.ID, []Op{{Path: "a.md", Deleted: true, BaseHash: hashOf("b")}}, a, yes)
	if !res[0].OK {
		t.Fatalf("delete: %+v", res[0])
	}
	res, _, _ = s.Commit(ctx, v.ID, []Op{{Path: "a.md", Hash: hashOf("c"), Size: 1}}, a, yes)
	if !res[0].OK {
		t.Fatalf("recreate: %+v", res[0])
	}
	vs, _ := s.Versions(ctx, v.ID, "a.md")
	if len(vs) != 4 {
		t.Fatalf("expected 4 versions, got %d", len(vs))
	}
}

func TestCommitValidation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	v, _ := s.CreateVault(ctx, "Test", DefaultBackupPolicy, 0)
	a := Author{Name: "t"}
	res, _, _ := s.Commit(ctx, v.ID, []Op{
		{Path: "../etc/passwd", Hash: hashOf("a")},
		{Path: "/abs.md", Hash: hashOf("a")},
		{Path: "ok.md", Hash: "nothex"},
		{Path: "missing.md", Hash: hashOf("d")},
		{Path: "Café.md", Hash: hashOf("a")}, // NFD "Café"
		{Path: "Notes/X.md", Hash: hashOf("a")},
		{Path: "notes/x.md", Hash: hashOf("a")},
	}, a, func(h string) bool { return h != hashOf("d") })
	want := []string{ErrCodeInvalidPath, ErrCodeInvalidPath, ErrCodeInvalid, ErrCodeMissingBlob, "", "", ErrCodeCaseConflict}
	for i, w := range want {
		if res[i].Error != w {
			t.Errorf("op %d: want %q got %+v", i, w, res[i])
		}
	}
	if res[4].Path != "Café.md" {
		t.Errorf("path not NFC-normalized: %q", res[4].Path)
	}
}

func TestChangesPaging(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	v, _ := s.CreateVault(ctx, "Test", DefaultBackupPolicy, 0)
	var ops []Op
	for _, p := range []string{"a", "b", "c", "d", "e"} {
		ops = append(ops, Op{Path: p + ".md", Hash: hashOf("a")})
	}
	s.Commit(ctx, v.ID, ops, Author{}, yes)
	got, head, more, err := s.Changes(ctx, v.ID, 0, 2)
	if err != nil || len(got) != 2 || !more || head != 5 {
		t.Fatalf("page1: %v %d %v %v", got, head, more, err)
	}
	got, _, more, _ = s.Changes(ctx, v.ID, got[1].Rev, 10)
	if len(got) != 3 || more {
		t.Fatalf("page2: %v %v", got, more)
	}
	// Updating a file moves it to the end of the feed.
	s.Commit(ctx, v.ID, []Op{{Path: "a.md", Hash: hashOf("b"), BaseHash: hashOf("a")}}, Author{}, yes)
	got, _, _, _ = s.Changes(ctx, v.ID, 5, 10)
	if len(got) != 1 || got[0].Path != "a.md" || got[0].Rev != 6 {
		t.Fatalf("after update: %+v", got)
	}
}

func TestRolesAndMembership(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, _ := s.CreateUser(ctx, "jirka", "x", false)
	admin, _ := s.CreateUser(ctx, "admin", "x", true)
	v, _ := s.CreateVault(ctx, "Práce", DefaultBackupPolicy, 0)
	if r, _ := s.Role(ctx, u, v.ID); r != "" {
		t.Fatalf("non-member has role %q", r)
	}
	if r, _ := s.Role(ctx, admin, v.ID); r != RoleOwner {
		t.Fatalf("admin role %q", r)
	}
	s.SetMember(ctx, v.ID, u.ID, RoleViewer)
	if r, _ := s.Role(ctx, u, v.ID); r != RoleViewer {
		t.Fatalf("member role %q", r)
	}
	vs, _ := s.UserVaults(ctx, u.ID)
	if len(vs) != 1 || vs[0].Role != RoleViewer {
		t.Fatalf("user vaults %+v", vs)
	}
	if _, err := s.CreateVault(ctx, "Test", DefaultBackupPolicy, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateVault(ctx, "TEST", DefaultBackupPolicy, 0); err != ErrVaultNameTaken {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := s.CreateVault(ctx, "Other", BackupPolicy{IntervalSec: 123}, 0); err == nil {
		t.Fatal("invalid backup policy accepted")
	}
}

func TestDeviceVaultName(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "user1", "pw", false)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateDevice(ctx, u.ID, "Laptop", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.ListDevices(ctx, 0); len(ds) != 1 || ds[0].VaultName != "" {
		t.Fatalf("new device should have no vault: %+v", ds)
	}
	v, _ := s.CreateVault(ctx, "Server name", DefaultBackupPolicy, 0)
	if err := s.SetDeviceVault(ctx, id, v.ID); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.ListDevices(ctx, 0); ds[0].VaultName != "Server name" {
		t.Fatalf("got %q", ds[0].VaultName)
	}
}
