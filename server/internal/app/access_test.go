package app

import (
	"archive/zip"
	"bytes"
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/jirkacepelka/obsisync/server/internal/i18n"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// TestFolderAccess checks that a member limited to some folders sees and
// changes nothing else, through every way into the server.
func TestFolderAccess(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := e.user("jirka", true)
	eva := e.user("eva", false)
	v, _ := e.app.Store.CreateVault(ctx, "Strana", store.DefaultBackupPolicy, admin.ID)
	e.app.Store.SetMember(ctx, v.ID, eva.ID, store.RoleEditor)
	vp := "/vaults/" + itoa(v.ID)
	api := "/api/v1/vaults/" + itoa(v.ID)

	secret := []byte("# Rozpočet\ntajná čísla\n")
	dev := e.login("jirka")
	for p, c := range map[string][]byte{
		"Program 2029/Plan.md":             []byte("# Plán\n[[Rozpočet]]\n"),
		"Program 2029/Finance/Rozpočet.md": secret,
		"Program 2029/Kampaň/Text.md":      []byte("# Kampaň\n"),
		"Jiné.md":                          []byte("# Jiné\n"),
		"Program 2029/Finance/Starý.md":    []byte("tajná stará čísla\n"),
	} {
		if r := e.put(dev, v.ID, p, c, ""); !r.OK {
			t.Fatalf("put %s: %+v", p, r)
		}
	}
	old, _ := e.app.Store.File(ctx, v.ID, "Program 2029/Finance/Starý.md")
	e.app.Store.Commit(ctx, v.ID, []store.Op{{Path: old.Path, Deleted: true, BaseHash: old.Hash}}, store.Author{Name: "t"}, e.app.Blobs.Has)

	owner := e.webLogin("jirka")
	// Off by default: no Roles tab, the settings offer the switch.
	if body := mustGet(owner, vp+"/settings"); strings.Contains(body, vp+"/roles") || !strings.Contains(body, `action="`+vp+`/folder-access"`) {
		t.Fatal("settings: switch missing or roles shown while off")
	}
	owner.get(vp + "/settings")
	if st, _ := owner.post(vp+"/folder-access", url.Values{"folder_access": {"1"}}); st != 200 {
		t.Fatalf("switch on: %d", st)
	}
	owner.get(vp + "/roles")
	owner.post(vp+"/roles", url.Values{"name": {"Tým"}, "base": {"editor"}})
	roles, _ := e.app.Store.VaultRoles(ctx, v.ID)
	if len(roles) != 1 {
		t.Fatalf("roles: %+v", roles)
	}
	rid := itoa(roles[0].ID)
	for p, l := range map[string]string{"/": "none", "Program 2029": "read", "Program 2029/Kampaň": "write"} {
		if st, body := owner.post(vp+"/rules", url.Values{"role_id": {rid}, "path": {p}, "level": {l}}); st != 200 || strings.Contains(body, "flash err") {
			t.Fatalf("rule %s: %d", p, st)
		}
	}
	owner.get(vp + "/members")
	owner.post(vp+"/members", url.Values{"user_id": {itoa(eva.ID)}, "role": {"r:" + rid}})
	owner.post(vp+"/rules", url.Values{"user_id": {itoa(eva.ID)}, "back": {"members"}, "path": {"Program 2029/Finance"}, "level": {"none"}})
	if acc, _ := e.app.Store.Access(ctx, eva, v.ID); len(acc.Rules) != 4 {
		t.Fatalf("eva's rules: %+v", acc)
	}
	for _, l := range i18n.Languages {
		owner.get("/lang?l=" + l.Code + "&next=/")
		for _, p := range []string{vp + "/roles", vp + "/members", vp + "/settings"} {
			st, body := owner.get(p)
			if st != 200 || !strings.Contains(body, "</footer>") {
				t.Errorf("%s %s: %d", l.Code, p, st)
			}
			if m := regexp.MustCompile(`\b(?:access|roles|level|tab|msg)\.[a-zA-Z]+\b`).FindString(body); m != "" {
				t.Errorf("%s %s: untranslated key %q", l.Code, p, m)
			}
		}
	}
	owner.get("/lang?l=en&next=/")
	if body := mustGet(owner, vp+"/members"); !strings.Contains(body, `value="r:`+rid+`" selected`) || !strings.Contains(body, "Program 2029/Finance") {
		t.Fatal("members page does not show the custom role and exception")
	}

	// ---- sync API ----
	tok := e.login("eva")
	var page struct {
		Changes []store.FileEntry
		Role    string
		Access  struct {
			Rev   int64
			Rules []store.AccessRule
		}
	}
	e.call("GET", api+"/changes?since=0", tok, nil, &page)
	var paths []string
	for _, c := range page.Changes {
		paths = append(paths, c.Path)
	}
	sort.Strings(paths)
	if strings.Join(paths, "|") != "Program 2029/Kampaň/Text.md|Program 2029/Plan.md" || page.Role != "editor" || len(page.Access.Rules) != 4 || page.Access.Rev == 0 {
		t.Fatalf("changes: %v role=%s access=%+v", paths, page.Role, page.Access)
	}
	if st := e.call("GET", api+"/blobs/"+sha(secret), tok, nil, nil); st != 404 {
		t.Fatalf("hidden blob: %d", st)
	}
	var missing struct{ Missing []string }
	e.call("POST", api+"/blobs/missing", tok, map[string]any{"hashes": []string{sha(secret)}}, &missing)
	if len(missing.Missing) != 1 {
		t.Fatal("missing-blob check discloses hidden content")
	}
	if r := e.put(tok, v.ID, "Program 2029/Plan.md", []byte("změna"), sha([]byte("# Plán\n[[Rozpočet]]\n"))); r.Error != store.ErrCodeForbidden {
		t.Fatalf("write to read-only folder: %+v", r)
	}
	if r := e.put(tok, v.ID, "Jiné.md", []byte("x"), ""); r.Error != store.ErrCodeForbidden {
		t.Fatalf("write to hidden file: %+v", r)
	}
	if r := e.put(tok, v.ID, "Program 2029/Kampaň/Nový.md", []byte("nový"), ""); !r.OK {
		t.Fatalf("write to writable folder: %+v", r)
	}
	// The socket tells the device its access revision.
	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + api + "/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer " + tok}}})
	if err != nil {
		t.Fatal(err)
	}
	_, msg, _ := conn.Read(ctx)
	conn.Close(websocket.StatusNormalClosure, "")
	if !strings.Contains(string(msg), `"access":`+itoa(page.Access.Rev)) {
		t.Fatalf("socket message: %s", msg)
	}

	// ---- web ----
	b := e.webLogin("eva")
	_, tree := b.api("GET", vp+"/api/tree", nil)
	var treePaths []string
	for _, f := range tree["files"].([]any) {
		treePaths = append(treePaths, f.(map[string]any)["path"].(string))
	}
	if strings.Join(treePaths, "|") != "Program 2029/Kampaň/Nový.md|Program 2029/Kampaň/Text.md|Program 2029/Plan.md" {
		t.Fatalf("tree: %v", treePaths)
	}
	if st, _ := b.api("GET", vp+"/api/note?path="+url.QueryEscape("Program 2029/Finance/Rozpočet.md"), nil); st != 404 {
		t.Fatalf("hidden note: %d", st)
	}
	if _, n := b.api("GET", vp+"/api/note?path="+url.QueryEscape("Program 2029/Plan.md"), nil); n["writable"] != false {
		t.Fatalf("read-only note: %v", n)
	}
	if _, n := b.api("GET", vp+"/api/note?path="+url.QueryEscape("Program 2029/Kampaň/Text.md"), nil); n["writable"] != true {
		t.Fatalf("writable note: %v", n)
	}
	if st, _ := b.api("POST", vp+"/api/save", map[string]any{"path": "Program 2029/Plan.md", "base": sha([]byte("# Plán\n[[Rozpočet]]\n")), "text": "x"}); st != 403 {
		t.Fatalf("web save to read-only folder: %d", st)
	}
	if st, _ := b.api("POST", vp+"/api/rename", map[string]any{"from": "Program 2029/Kampaň", "to": "Kampaň", "folder": true}); st != 403 {
		t.Fatalf("moving a folder out of where one may write: %d", st)
	}
	if st, _ := b.api("POST", vp+"/api/delete", map[string]any{"path": "Program 2029/Plan.md"}); st != 403 {
		t.Fatalf("web delete in read-only folder: %d", st)
	}
	if _, r := b.api("POST", vp+"/api/render", map[string]any{"path": "Program 2029/Plan.md", "text": "[[Rozpočet]]"}); strings.Contains(r["html"].(string), "Finance") {
		t.Fatalf("link to a hidden note resolved: %v", r["html"])
	}
	sv, _ := e.app.Store.Versions(ctx, v.ID, "Program 2029/Finance/Rozpočet.md")
	for _, p := range []string{
		"/raw?path=" + url.QueryEscape("Program 2029/Finance/Rozpočet.md"),
		"/file?path=" + url.QueryEscape("Program 2029/Finance/Rozpočet.md"),
		"/version/" + itoa(sv[0].ID),
		"/backups",
	} {
		if st, _ := b.get(vp + p); st != 404 && st != 403 {
			t.Errorf("GET %s: %d", p, st)
		}
	}
	for _, p := range []string{"", "/trash", "/notes"} {
		st, body := b.get(vp + p)
		if st != 200 || strings.Contains(body, "Finance") || strings.Contains(body, "Jiné") || strings.Contains(body, vp+"/backups") {
			t.Errorf("GET %s shows hidden content (status %d)", vp+p, st)
		}
	}
	_, zipBody := b.get(vp + "/zip")
	zr, err := zip.NewReader(bytes.NewReader([]byte(zipBody)), int64(len(zipBody)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "Finance") || strings.Contains(f.Name, "Jiné") {
			t.Errorf("zip contains %s", f.Name)
		}
	}

	// ---- MCP ----
	agent := newToken(b, "Claude", "0", "write")
	for name, args := range map[string]map[string]any{
		"list_notes":     {},
		"search_notes":   {"query": "tajná"},
		"recent_changes": {},
	} {
		if out, _ := e.tool(agent, name, args); strings.Contains(out, "Finance") || strings.Contains(out, "Jiné") || strings.Contains(out, "čísla") {
			t.Errorf("%s shows hidden content: %s", name, out)
		}
	}
	if _, failed := e.tool(agent, "read_note", map[string]any{"path": "Program 2029/Finance/Rozpočet"}); !failed {
		t.Error("read_note of a hidden note works")
	}
	if out, failed := e.tool(agent, "create_note", map[string]any{"path": "Program 2029/X", "content": "x"}); !failed || !strings.Contains(out, "read-only") {
		t.Errorf("create_note in a read-only folder: %s", out)
	}
	if _, failed := e.tool(agent, "move_note", map[string]any{"from": "Program 2029/Kampaň/Text", "to": "Text"}); !failed {
		t.Error("move_note out of the writable folder works")
	}
	if _, failed := e.tool(agent, "append_to_note", map[string]any{"path": "Program 2029/Kampaň/Text", "content": "ok"}); failed {
		t.Error("append_to_note in a writable folder fails")
	}

	// The owner still sees everything.
	if _, tr := owner.api("GET", vp+"/api/tree", nil); len(tr["files"].([]any)) != 5 {
		t.Fatalf("owner's tree: %v", tr["files"])
	}

	// Switched off, eva sees the whole vault again and gets a new access revision.
	owner.get(vp + "/settings")
	owner.post(vp+"/folder-access", url.Values{})
	e.call("GET", api+"/changes?since=0", tok, nil, &page)
	if len(page.Changes) != 6 || len(page.Access.Rules) != 0 {
		t.Fatalf("after switching off: %d changes, rules %+v", len(page.Changes), page.Access.Rules)
	}
	if st, _ := b.get(vp + "/backups"); st != 200 {
		t.Fatalf("backups after switching off: %d", st)
	}
}
