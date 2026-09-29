package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// webLogin returns a browser logged in to the web GUI as name.
func (e *env) webLogin(name string) *browser {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	b := &browser{e: e, c: &http.Client{Jar: jar}}
	if st, body := b.post("/login", url.Values{"username": {name}, "password": {"heslo1234"}, "next": {"/"}}); st != 200 || strings.Contains(body, "flash err") {
		e.t.Fatalf("web login %s: %d", name, st)
	}
	b.get("/")
	return b
}

// api calls one of the editor's JSON endpoints.
func (b *browser) api(method, path string, body any) (int, map[string]any) {
	b.e.t.Helper()
	var r io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		r = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, b.e.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", b.csrf)
	res, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	data, _ := io.ReadAll(res.Body)
	json.Unmarshal(data, &out)
	return res.StatusCode, out
}

func (e *env) fileText(vault int64, path string) (string, bool) {
	f, err := e.app.Store.File(context.Background(), vault, path)
	if err != nil || f.Deleted {
		return "", false
	}
	r, err := e.app.Blobs.Open(f.Hash)
	if err != nil {
		return "", false
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b), true
}

func TestWebEditor(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := e.user("jirka", true)
	viewer := e.user("guest", false)
	v, _ := e.app.Store.CreateVault(ctx, "Home", store.DefaultBackupPolicy, admin.ID)
	e.app.Store.SetMember(ctx, v.ID, viewer.ID, store.RoleViewer)
	vp := "/vaults/" + itoa(v.ID)
	b := e.webLogin("jirka")

	if st, body := b.get(vp + "/notes"); st != 200 || !strings.Contains(body, `id="ed-config"`) {
		t.Fatalf("editor page: %d", st)
	}
	b.get(vp + "/notes")

	// Create a note.
	st, res := b.api("POST", vp+"/api/save", map[string]any{"path": "Garden plan.md", "text": "# Garden plan\n- Tomatoes\n- Carrots\n- Beans\n", "create": true})
	if st != 200 || res["ok"] != true {
		t.Fatalf("create: %d %v", st, res)
	}
	h1 := res["hash"].(string)
	if st, _ := b.api("POST", vp+"/api/save", map[string]any{"path": "Garden plan.md", "text": "x", "create": true}); st != 409 {
		t.Fatalf("creating an existing note must fail, got %d", st)
	}
	// A device sees it and edits the last line.
	tok := e.login("jirka")
	r := e.put(tok, v.ID, "Garden plan.md", []byte("# Garden plan\n- Tomatoes\n- Carrots\n- Beans along the fence\n"), h1)
	if !r.OK {
		t.Fatalf("device edit: %+v", r)
	}
	// The web editor, still on the old version, edits the first line: merged.
	st, res = b.api("POST", vp+"/api/save", map[string]any{"path": "Garden plan.md", "base": h1, "text": "# Garden plan 2027\n- Tomatoes\n- Carrots\n- Beans\n"})
	if st != 200 || res["merged"] != true {
		t.Fatalf("merge: %d %v", st, res)
	}
	want := "# Garden plan 2027\n- Tomatoes\n- Carrots\n- Beans along the fence\n"
	if got, _ := e.fileText(v.ID, "Garden plan.md"); got != want || res["text"] != want {
		t.Fatalf("merged text: %q", got)
	}
	h2 := res["hash"].(string)
	// Overlapping edit from a stale base: server version stays, ours becomes a copy.
	r = e.put(tok, v.ID, "Garden plan.md", []byte("# Garden plan 2028\n- Tomatoes\n- Carrots\n- Beans along the fence\n"), h2)
	st, res = b.api("POST", vp+"/api/save", map[string]any{"path": "Garden plan.md", "base": h2, "text": "# Garden plan 2030\n- Tomatoes\n- Carrots\n- Beans along the fence\n"})
	if st != 200 || res["conflict"] != true || !strings.Contains(res["copy"].(string), "(conflict ") || !strings.HasSuffix(res["copy"].(string), " jirka).md") {
		t.Fatalf("conflict: %d %v", st, res)
	}
	if got, _ := e.fileText(v.ID, res["copy"].(string)); !strings.Contains(got, "2030") {
		t.Fatalf("conflict copy content: %q", got)
	}
	if got, _ := e.fileText(v.ID, "Garden plan.md"); !strings.Contains(got, "2028") {
		t.Fatalf("server version must stay: %q", got)
	}

	// Reading, tree, history author.
	if st, res := b.api("GET", vp+"/api/note?path="+url.QueryEscape("Garden plan.md"), nil); st != 200 || !strings.Contains(res["text"].(string), "2028") {
		t.Fatalf("note: %d %v", st, res)
	}
	vers, _ := e.app.Store.Versions(ctx, v.ID, "Garden plan.md")
	if len(vers) < 3 || !strings.Contains(vers[len(vers)-1].Author, "jirka (web)") {
		t.Fatalf("web author not recorded: %+v", vers)
	}
	if _, res := b.api("GET", vp+"/api/tree", nil); len(res["files"].([]any)) != 2 {
		t.Fatalf("tree: %v", res)
	}

	// Invalid names are refused.
	for _, bad := range []string{".obsidian/app.json", "../x.md", "a/./b.md", "Bad:name.md", "photo.png"} {
		if st, _ := b.api("POST", vp+"/api/save", map[string]any{"path": bad, "text": "x", "create": true}); st != 400 {
			t.Errorf("path %q accepted: %d", bad, st)
		}
	}

	// Rename a folder with its notes, then delete one.
	b.api("POST", vp+"/api/save", map[string]any{"path": "Projects/Shelves.md", "text": "Oak", "create": true})
	b.api("POST", vp+"/api/save", map[string]any{"path": "Projects/Server.md", "text": "Backups", "create": true})
	if st, res := b.api("POST", vp+"/api/rename", map[string]any{"from": "Projects", "to": "Work/Projects", "folder": true}); st != 200 {
		t.Fatalf("rename folder: %d %v", st, res)
	}
	if _, ok := e.fileText(v.ID, "Projects/Shelves.md"); ok {
		t.Fatal("old path still live after rename")
	}
	if got, ok := e.fileText(v.ID, "Work/Projects/Shelves.md"); !ok || got != "Oak" {
		t.Fatalf("renamed note: %q", got)
	}
	if st, _ := b.api("POST", vp+"/api/rename", map[string]any{"from": "Work/Projects/Server.md", "to": "Work/Projects/Shelves.md"}); st != 409 {
		t.Fatalf("rename onto an existing note must fail, got %d", st)
	}
	if st, _ := b.api("POST", vp+"/api/delete", map[string]any{"path": "Work/Projects/Server.md"}); st != 200 {
		t.Fatal("delete failed")
	}
	if trash, _ := e.app.Store.ListFiles(ctx, v.ID, true); len(trash) == 0 {
		t.Fatal("deleted note is not in the trash")
	}

	// Upload an image and preview a note that embeds it.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "image.png")
	fw.Write([]byte("\x89PNG fake"))
	mw.WriteField("dir", "Work")
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+vp+"/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", b.csrf)
	resp, err := b.c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("upload: %v %v", err, resp.StatusCode)
	}
	var up map[string]any
	json.NewDecoder(resp.Body).Decode(&up)
	resp.Body.Close()
	img := up["path"].(string)
	if !strings.HasPrefix(img, "Work/Pasted image ") {
		t.Fatalf("upload name: %q", img)
	}
	name := img[len("Work/"):]
	st, res = b.api("POST", vp+"/api/render", map[string]any{"path": "Work/Note.md", "text": "![[" + name + "]] and [[Shelves]] and [[Nope]]"})
	html, _ := res["html"].(string)
	if st != 200 || !strings.Contains(html, `/raw?inline=1&amp;path=`+url.QueryEscape(img)) || !strings.Contains(html, url.QueryEscape("Work/Projects/Shelves.md")) || !strings.Contains(html, "unresolved") {
		t.Fatalf("render: %s", html)
	}
	if st, _ := b.get(vp + "/raw?inline=1&path=" + url.QueryEscape(img)); st != 200 {
		t.Fatalf("raw: %d", st)
	}

	// Without the CSRF header, writes are refused.
	b2 := *b
	b2.csrf = ""
	if st, _ := b2.api("POST", vp+"/api/save", map[string]any{"path": "x.md", "text": "x", "create": true}); st != 403 {
		t.Fatalf("csrf not enforced for JSON: %d", st)
	}

	// A read-only member can read but not write.
	g := e.webLogin("guest")
	if st, _ := g.api("GET", vp+"/api/note?path="+url.QueryEscape("Garden plan.md"), nil); st != 200 {
		t.Fatalf("viewer read: %d", st)
	}
	if st, _ := g.api("POST", vp+"/api/save", map[string]any{"path": "Garden plan.md", "base": "", "text": "hacked"}); st != 403 {
		t.Fatalf("viewer write: %d", st)
	}
	if st, _ := g.api("POST", vp+"/api/delete", map[string]any{"path": "Garden plan.md"}); st != 403 {
		t.Fatalf("viewer delete: %d", st)
	}
}

func TestPublishing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := e.user("jirka", true)
	v, _ := e.app.Store.CreateVault(ctx, "Garden", store.DefaultBackupPolicy, admin.ID)
	vp := "/vaults/" + itoa(v.ID)
	b := e.webLogin("jirka")
	save := func(p, text string) {
		t.Helper()
		if st, res := b.api("POST", vp+"/api/save", map[string]any{"path": p, "text": text, "create": true}); st != 200 {
			t.Fatalf("save %s: %d %v", p, st, res)
		}
	}
	b.get(vp + "/notes")
	save("Home.md", "# Welcome\nSee [[Beds]] and [[Private diary]].\n")
	save("Beds.md", "---\npublish: true\ntitle: Garden beds\n---\n![[bed.png]]\n\n> [!tip] Water\n> In the morning.\n")
	save("Private diary.md", "secret thoughts")
	for _, p := range []string{"bed.png", "hidden.png"} {
		h, _ := e.app.Blobs.PutBytes([]byte("png " + p))
		e.app.Store.Commit(ctx, v.ID, []store.Op{{Path: p, Hash: h, Size: 8, Mtime: 1}}, store.Author{Name: "t"}, e.app.Blobs.Has)
	}

	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(p string) (int, string) {
		res, err := anon.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	if st, _ := get("/p/garden/"); st != 404 {
		t.Fatalf("unconfigured site: %d", st)
	}

	b.get(vp + "/settings")
	if _, body := b.post(vp+"/publish", url.Values{"enabled": {"1"}, "slug": {"Bad slug!"}, "mode": {"marked"}}); !strings.Contains(body, "flash err") {
		t.Fatal("invalid slug accepted")
	}
	b.get(vp + "/settings")
	if _, body := b.post(vp+"/publish", url.Values{"enabled": {"1"}, "slug": {"garden"}, "title": {"Our garden"}, "mode": {"marked"}, "home": {"Home.md"}}); strings.Contains(body, "flash err") {
		t.Fatalf("publish settings: %s", body)
	}

	st, body := get("/p/garden/")
	if st != 200 || !strings.Contains(body, "Welcome") || !strings.Contains(body, `href="/p/garden/Beds"`) {
		t.Fatalf("home page: %d %s", st, body)
	}
	if strings.Contains(body, "Private%20diary") || strings.Contains(body, "/p/garden/Private") {
		t.Fatal("link to an unpublished note leaked")
	}
	st, body = get("/p/garden/Beds")
	if st != 200 || !strings.Contains(body, "Garden beds") || !strings.Contains(body, `src="/p/garden/bed.png"`) || !strings.Contains(body, `data-callout="tip"`) {
		t.Fatalf("published note: %d %s", st, body)
	}
	if strings.Contains(body, "publish: true") {
		t.Fatal("front matter shown on the published page")
	}
	if st, g := get("/p/garden/_graph.json"); st != 200 || !strings.Contains(g, `"Garden beds"`) || !strings.Contains(g, `"edges":[[`) || strings.Contains(g, "Private") {
		t.Fatalf("graph: %d %s", st, g)
	}
	if !strings.Contains(body, "Linked from") || !strings.Contains(body, `class="pub-card" href="/p/garden/Home"`) {
		t.Fatalf("backlink from the published home note missing: %s", body)
	}
	for p, want := range map[string]int{"/p/garden/bed.png": 200, "/p/garden/hidden.png": 404, "/p/garden/Private%20diary": 404, "/p/garden/Private%20diary.md": 404, "/p/nope/": 404} {
		if st, _ := get(p); st != want {
			t.Errorf("GET %s: %d, want %d", p, st, want)
		}
	}
	if res, _ := anon.Get(e.srv.URL + "/p/garden/Beds"); res.Header.Get("Content-Security-Policy") == "" {
		t.Error("public pages need a CSP")
	}

	// Folder mode, then switching the site off.
	save("Public/Plan.md", "Plan")
	b.get(vp + "/settings")
	b.post(vp+"/publish", url.Values{"enabled": {"1"}, "slug": {"garden"}, "mode": {"folder"}, "folder": {"Public"}})
	if st, _ := get("/p/garden/Public/Plan"); st != 200 {
		t.Fatalf("folder mode: %d", st)
	}
	if st, _ := get("/p/garden/Beds"); st != 404 {
		t.Fatalf("note outside the folder is public: %d", st)
	}
	b.get(vp + "/settings")
	b.post(vp+"/publish", url.Values{"slug": {"garden"}, "mode": {"folder"}, "folder": {"Public"}})
	if st, _ := get("/p/garden/Public/Plan"); st != 404 {
		t.Fatalf("disabled site still served: %d", st)
	}

	// Another vault can't take the same address.
	v2, _ := e.app.Store.CreateVault(ctx, "Other", store.DefaultBackupPolicy, admin.ID)
	if err := e.app.Store.SavePublish(ctx, store.Publish{VaultID: v2.ID, Slug: "garden"}); err != store.ErrSlugTaken {
		t.Fatalf("duplicate slug: %v", err)
	}
}
