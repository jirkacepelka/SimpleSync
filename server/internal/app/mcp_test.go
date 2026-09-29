package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jirkacepelka/obsisync/server/internal/i18n"
	"github.com/jirkacepelka/obsisync/server/internal/mcp"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// rpc calls a method on the MCP endpoint and returns the HTTP status and
// the decoded JSON-RPC response.
func (e *env) rpc(token, method string, params any) (int, map[string]any) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	data, _ := io.ReadAll(res.Body)
	json.Unmarshal(data, &out)
	return res.StatusCode, out
}

// tool calls an MCP tool and returns its text and whether it failed.
func (e *env) tool(token, name string, args map[string]any) (string, bool) {
	e.t.Helper()
	st, out := e.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args})
	if st != 200 {
		e.t.Fatalf("%s: status %d", name, st)
	}
	if out["error"] != nil {
		return out["error"].(map[string]any)["message"].(string), true
	}
	res := out["result"].(map[string]any)
	return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
}

var tokenRe = regexp.MustCompile(`osa_[A-Za-z0-9_-]{20,}`)

// newToken creates an API token on the AI agents page.
func newToken(b *browser, name, vault, access string) string {
	b.e.t.Helper()
	b.get("/agents")
	st, body := b.post("/agents", url.Values{"name": {name}, "vault": {vault}, "access": {access}})
	tok := tokenRe.FindString(body)
	if st != 200 || tok == "" {
		b.e.t.Fatalf("token not created: %d", st)
	}
	return tok
}

func TestMCP(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := e.user("admin", true)
	eva := e.user("eva", false)
	notes, _ := e.app.Store.CreateVault(ctx, "Notes", store.DefaultBackupPolicy, admin.ID)
	work, _ := e.app.Store.CreateVault(ctx, "Work", store.DefaultBackupPolicy, admin.ID)
	e.app.Store.SetMember(ctx, work.ID, eva.ID, store.RoleViewer)
	dev := e.login("admin")
	e.put(dev, notes.ID, "Projects/Plan.md", []byte("# Plan\n\nfirst line\nbuy milk\n"), "")
	e.put(dev, notes.ID, ".obsidian/app.json", []byte("{}"), "")

	// Without a token the client is pointed at the OAuth metadata.
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource") {
		t.Fatalf("unauthenticated: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}

	b := e.webLogin("admin")
	if body := mustGet(b, "/agents"); !strings.Contains(body, e.srv.URL+"/mcp") {
		t.Fatal("agents page does not show the MCP address")
	}
	rw := newToken(b, "Claude Code", "0", "write")
	for _, l := range i18n.Languages {
		b.get("/lang?l=" + l.Code + "&next=/")
		if m := regexp.MustCompile(`\b(?:agents|oauth|nav|title)\.[a-zA-Z]+\b`).FindString(mustGet(b, "/agents")); m != "" {
			t.Errorf("%s: untranslated key %q", l.Code, m)
		}
	}
	b.get("/lang?l=en&next=/")

	st, out := e.rpc(rw, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	if st != 200 || out["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", st, out)
	}
	_, out = e.rpc(rw, "tools/list", nil)
	if n := len(out["result"].(map[string]any)["tools"].([]any)); n != 10 {
		t.Fatalf("read-write token sees %d tools", n)
	}

	if text, _ := e.tool(rw, "list_vaults", nil); !strings.Contains(text, "Notes") || !strings.Contains(text, "Work") || !strings.Contains(text, "read-write") {
		t.Fatalf("list_vaults: %s", text)
	}
	// Two vaults: the vault must be named.
	if _, isErr := e.tool(rw, "list_notes", nil); !isErr {
		t.Fatal("list_notes without a vault should fail with two vaults")
	}
	text, _ := e.tool(rw, "list_notes", map[string]any{"vault": "notes"})
	if !strings.Contains(text, "Projects/Plan.md") || strings.Contains(text, ".obsidian") {
		t.Fatalf("list_notes: %s", text)
	}
	text, _ = e.tool(rw, "read_note", map[string]any{"vault": "Notes", "path": "Projects/Plan"})
	if !strings.Contains(text, "buy milk") {
		t.Fatalf("read_note: %s", text)
	}
	hash := regexp.MustCompile(`hash: ([0-9a-f]{64})`).FindStringSubmatch(text)[1]
	if text, _ := e.tool(rw, "search_notes", map[string]any{"vault": "Notes", "query": "MILK plan"}); !strings.Contains(text, "Projects/Plan.md") || !strings.Contains(text, "buy milk") {
		t.Fatalf("search: %s", text)
	}
	if text, _ := e.tool(rw, "search_notes", map[string]any{"vault": "Notes", "query": "nothing-like-this"}); !strings.Contains(text, "Nothing found") {
		t.Fatalf("search miss: %s", text)
	}

	// Writing goes through sync: devices see the change with its author.
	if text, isErr := e.tool(rw, "create_note", map[string]any{"vault": "Notes", "path": "Inbox/Idea", "content": "an idea"}); isErr {
		t.Fatalf("create: %s", text)
	}
	if got, _ := e.fileText(notes.ID, "Inbox/Idea.md"); got != "an idea" {
		t.Fatalf("created note: %q", got)
	}
	if _, isErr := e.tool(rw, "create_note", map[string]any{"vault": "Notes", "path": "Inbox/Idea.md", "content": "x"}); !isErr {
		t.Fatal("create over an existing note should fail")
	}
	var ch struct{ Changes []store.FileEntry }
	e.call("GET", "/api/v1/vaults/"+itoa(notes.ID)+"/changes?since=0", dev, nil, &ch)
	if len(ch.Changes) != 3 {
		t.Fatalf("device sees %d changes", len(ch.Changes))
	}
	if vs, _ := e.app.Store.Versions(ctx, notes.ID, "Inbox/Idea.md"); len(vs) != 1 || vs[0].Author != "admin (Claude Code)" {
		t.Fatalf("version author: %+v", vs)
	}

	// An edit made meanwhile on a device is merged, not overwritten.
	e.put(dev, notes.ID, "Projects/Plan.md", []byte("# Plan\n\nfirst line\nbuy milk\ncall mom\n"), hash)
	if text, isErr := e.tool(rw, "update_note", map[string]any{"vault": "Notes", "path": "Projects/Plan.md", "content": "# Plan v2\n\nfirst line\nbuy milk\n", "base_hash": hash}); isErr || !strings.Contains(text, "merged") {
		t.Fatalf("update: %s", text)
	}
	if got, _ := e.fileText(notes.ID, "Projects/Plan.md"); got != "# Plan v2\n\nfirst line\nbuy milk\ncall mom\n" {
		t.Fatalf("merged: %q", got)
	}

	e.tool(rw, "append_to_note", map[string]any{"vault": "Notes", "path": "Log.md", "content": "- one"})
	e.tool(rw, "append_to_note", map[string]any{"vault": "Notes", "path": "Log.md", "content": "- two"})
	if got, _ := e.fileText(notes.ID, "Log.md"); got != "- one\n- two" {
		t.Fatalf("append: %q", got)
	}
	if text, isErr := e.tool(rw, "move_note", map[string]any{"vault": "Notes", "from": "Log.md", "to": "Archive/Log"}); isErr {
		t.Fatalf("move: %s", text)
	}
	if _, ok := e.fileText(notes.ID, "Log.md"); ok {
		t.Fatal("moved note still at the old path")
	}
	if got, _ := e.fileText(notes.ID, "Archive/Log.md"); got != "- one\n- two" {
		t.Fatalf("moved: %q", got)
	}
	if _, isErr := e.tool(rw, "delete_note", map[string]any{"vault": "Notes", "path": "Archive/Log.md"}); isErr {
		t.Fatal("delete failed")
	}
	if _, ok := e.fileText(notes.ID, "Archive/Log.md"); ok {
		t.Fatal("not deleted")
	}
	if text, _ := e.tool(rw, "recent_changes", map[string]any{"vault": "Notes"}); !strings.Contains(text, "deleted Archive/Log.md by admin (Claude Code)") {
		t.Fatalf("recent: %s", text)
	}
	for _, bad := range []string{"../x.md", ".obsidian/x.md", "a/./b.md", "img.png"} {
		if _, isErr := e.tool(rw, "create_note", map[string]any{"vault": "Notes", "path": bad, "content": "x"}); !isErr {
			t.Errorf("create %q should fail", bad)
		}
	}

	// A read-only token for one vault sees neither the other vault nor write tools.
	ro := newToken(b, "Reader", itoa(work.ID), "read")
	_, out = e.rpc(ro, "tools/list", nil)
	if n := len(out["result"].(map[string]any)["tools"].([]any)); n != 5 {
		t.Fatalf("read-only token sees %d tools", n)
	}
	if _, isErr := e.tool(ro, "create_note", map[string]any{"path": "x.md", "content": "x"}); !isErr {
		t.Fatal("read-only token wrote")
	}
	if text, _ := e.tool(ro, "list_vaults", nil); strings.Contains(text, "Notes") {
		t.Fatalf("vault-restricted token sees other vaults: %s", text)
	}
	if text, _ := e.tool(ro, "list_notes", nil); !strings.Contains(text, "empty") {
		t.Fatalf("single vault needs no name: %s", text)
	}

	// A token never gives more than the user's own role.
	be := e.webLogin("eva")
	evaTok := newToken(be, "Eva's agent", "0", "write")
	if text, _ := e.tool(evaTok, "list_vaults", nil); strings.Contains(text, "Notes") || !strings.Contains(text, "Work (id") || !strings.Contains(text, "read-only") {
		t.Fatalf("eva's vaults: %s", text)
	}
	if _, isErr := e.tool(evaTok, "create_note", map[string]any{"path": "x.md", "content": "x"}); !isErr {
		t.Fatal("viewer wrote through a token")
	}
	if body := mustGet(be, "/agents"); strings.Contains(body, "Reader") || !strings.Contains(body, "Eva&#39;s agent") {
		t.Fatal("eva sees admin's tokens")
	}

	// Revoking stops the token at once.
	toks, _ := e.app.Store.ListAPITokens(ctx, admin.ID)
	for _, tk := range toks {
		if tk.Name == "Reader" {
			b.get("/agents")
			b.post("/agents/"+itoa(tk.ID)+"/delete", url.Values{})
		}
	}
	if st, _ := e.rpc(ro, "ping", nil); st != 401 {
		t.Fatalf("revoked token: %d", st)
	}
}

func TestMCPOAuth(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := e.user("admin", true)
	v, _ := e.app.Store.CreateVault(ctx, "Notes", store.DefaultBackupPolicy, admin.ID)

	var meta map[string]any
	e.call("GET", "/.well-known/oauth-protected-resource/mcp", "", nil, &meta)
	if meta["resource"] != e.srv.URL+"/mcp" {
		t.Fatalf("resource metadata: %v", meta)
	}
	e.call("GET", "/.well-known/oauth-authorization-server", "", nil, &meta)
	if meta["authorization_endpoint"] != e.srv.URL+"/oauth/authorize" {
		t.Fatalf("server metadata: %v", meta)
	}

	const redirect = "https://claude.ai/api/mcp/auth_callback"
	var reg map[string]any
	if st := e.call("POST", "/oauth/register", "", map[string]any{"client_name": "Claude", "redirect_uris": []string{"http://evil.example/cb"}}, &reg); st != 400 {
		t.Fatalf("plain http redirect accepted: %d", st)
	}
	if st := e.call("POST", "/oauth/register", "", map[string]any{"client_name": "Claude", "redirect_uris": []string{redirect}}, &reg); st != 201 {
		t.Fatalf("register: %d", st)
	}
	clientID := reg["client_id"].(string)

	verifier := "a-very-long-random-verifier-string-0123456789-abcdefghij"
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"xyz"},
		"code_challenge": {mcp.PKCEChallenge(verifier)}, "code_challenge_method": {"S256"}}

	b := e.webLogin("admin")
	b.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if _, body := b.get("/oauth/authorize?client_id=" + clientID + "&redirect_uri=https://other.example/cb"); !strings.Contains(body, "flash err") {
		t.Fatal("unregistered redirect URI accepted")
	}
	st, body := b.get("/oauth/authorize?" + q.Encode())
	if st != 200 || !strings.Contains(body, "Claude") {
		t.Fatalf("consent page: %d", st)
	}
	form := url.Values{"decision": {"allow"}, "vault": {itoa(v.ID)}, "access": {"write"}}
	for k := range q {
		form.Set(k, q.Get(k))
	}
	form.Set("_csrf", b.csrf)
	res, err := b.c.PostForm(e.srv.URL+"/oauth/authorize", form)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	code := loc.Query().Get("code")
	if res.StatusCode != 303 || !strings.HasPrefix(loc.String(), redirect) || code == "" || loc.Query().Get("state") != "xyz" {
		t.Fatalf("authorize redirect: %d %s", res.StatusCode, loc)
	}

	token := func(code, verifier string) (int, map[string]any) {
		out := map[string]any{}
		res, err := http.PostForm(e.srv.URL+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
			"client_id": {clientID}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if st, _ := token(code, "wrong-verifier"); st != 400 {
		t.Fatalf("wrong PKCE verifier accepted: %d", st)
	}
	// The failed attempt used the code up.
	if st, _ := token(code, verifier); st != 400 {
		t.Fatalf("code reused: %d", st)
	}

	// Again, properly this time.
	res, _ = b.c.PostForm(e.srv.URL+"/oauth/authorize", form)
	res.Body.Close()
	loc, _ = url.Parse(res.Header.Get("Location"))
	st, out := token(loc.Query().Get("code"), verifier)
	if st != 200 || out["token_type"] != "Bearer" {
		t.Fatalf("token: %d %v", st, out)
	}
	access := out["access_token"].(string)
	if text, isErr := e.tool(access, "create_note", map[string]any{"path": "From Claude.md", "content": "hi"}); isErr {
		t.Fatalf("oauth token cannot write: %s", text)
	}
	if body := mustGet(b, "/agents"); !strings.Contains(body, "Claude") || !strings.Contains(body, "oauth") {
		t.Fatal("OAuth token not listed on the agents page")
	}

	// Deny sends the user back with an error.
	form.Set("decision", "deny")
	res, _ = b.c.PostForm(e.srv.URL+"/oauth/authorize", form)
	res.Body.Close()
	if loc, _ := url.Parse(res.Header.Get("Location")); loc.Query().Get("error") != "access_denied" {
		t.Fatalf("deny: %s", loc)
	}
}
