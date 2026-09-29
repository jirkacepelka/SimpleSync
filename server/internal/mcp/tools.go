package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jirkacepelka/obsisync/server/internal/pathutil"
	"github.com/jirkacepelka/obsisync/server/internal/store"
	"github.com/jirkacepelka/obsisync/server/internal/textmerge"
)

// maxText is the largest note an agent can read or write (as in the web editor).
const maxText = 2 << 20

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
	write       bool
	run         func(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error)
}

func schema(required []string, props map[string]any) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

var vaultProp = prop("string", "Vault name or id (see list_vaults). Can be left out when you have access to one vault only.")

func readOnly() map[string]any {
	return map[string]any{"readOnlyHint": true, "openWorldHint": false}
}

func writes(destructive, idempotent bool) map[string]any {
	return map[string]any{"readOnlyHint": false, "destructiveHint": destructive, "idempotentHint": idempotent, "openWorldHint": false}
}

var tools = []*tool{
	{
		Name: "list_vaults", Title: "List vaults",
		Description: "Lists the vaults you can access, with your access level and the number of files.",
		InputSchema: schema(nil, map[string]any{}), Annotations: readOnly(), run: listVaults,
	},
	{
		Name: "list_notes", Title: "List notes",
		Description: "Lists files in a vault (or in one folder of it, including subfolders) with size and last change.",
		InputSchema: schema(nil, map[string]any{
			"vault":  vaultProp,
			"folder": prop("string", `Only files in this folder, e.g. "Projects". Empty for the whole vault.`),
			"limit":  prop("integer", "At most this many files (default 500, max 5000)."),
		}), Annotations: readOnly(), run: listNotes,
	},
	{
		Name: "read_note", Title: "Read note",
		Description: "Returns the content of a text file (Markdown note, canvas, etc.) and its hash. Pass the hash as base_hash to update_note so edits made meanwhile are merged, not overwritten.",
		InputSchema: schema([]string{"path"}, map[string]any{
			"vault": vaultProp,
			"path":  prop("string", `Path in the vault, e.g. "Projects/Plan.md". ".md" can be left out.`),
		}), Annotations: readOnly(), run: readNote,
	},
	{
		Name: "search_notes", Title: "Search notes",
		Description: "Full-text search in note names and text files. Every word of the query must occur (case-insensitive). Returns matching paths with matching lines.",
		InputSchema: schema([]string{"query"}, map[string]any{
			"vault":  vaultProp,
			"query":  prop("string", "Words to find."),
			"folder": prop("string", "Only search in this folder."),
			"limit":  prop("integer", "At most this many files (default 20, max 100)."),
		}), Annotations: readOnly(), run: searchNotes,
	},
	{
		Name: "recent_changes", Title: "Recent changes",
		Description: "Lists the latest changes in a vault (created, edited and deleted files), newest first, with who made them.",
		InputSchema: schema(nil, map[string]any{
			"vault": vaultProp,
			"limit": prop("integer", "At most this many changes (default 30, max 500)."),
		}), Annotations: readOnly(), run: recentChanges,
	},
	{
		Name: "create_note", Title: "Create note", write: true,
		Description: "Creates a new note. Fails when the file already exists. Folders are created as needed. \".md\" is added when the path has no extension.",
		InputSchema: schema([]string{"path", "content"}, map[string]any{
			"vault":   vaultProp,
			"path":    prop("string", `Path of the new note, e.g. "Inbox/Idea.md".`),
			"content": prop("string", "Text of the note (Markdown)."),
		}), Annotations: writes(false, false), run: createNote,
	},
	{
		Name: "update_note", Title: "Replace note content", write: true,
		Description: "Replaces the whole content of an existing note. With base_hash (from read_note) changes made by others since then are merged in; when they overlap, your version is kept as a conflict copy next to the note. Without base_hash the note is overwritten (older versions stay in its history).",
		InputSchema: schema([]string{"path", "content"}, map[string]any{
			"vault":     vaultProp,
			"path":      prop("string", "Path of the note."),
			"content":   prop("string", "The new full text of the note."),
			"base_hash": prop("string", "Hash returned by read_note for the text you edited."),
		}), Annotations: writes(true, true), run: updateNote,
	},
	{
		Name: "append_to_note", Title: "Append to note", write: true,
		Description: "Adds text to the end of a note (on a new line), e.g. to a log or daily note. Creates the note when it does not exist.",
		InputSchema: schema([]string{"path", "content"}, map[string]any{
			"vault":   vaultProp,
			"path":    prop("string", "Path of the note."),
			"content": prop("string", "Text to add."),
		}), Annotations: writes(false, false), run: appendNote,
	},
	{
		Name: "move_note", Title: "Move or rename note", write: true,
		Description: "Moves or renames a file. Fails when the target exists. Links in other notes are not changed.",
		InputSchema: schema([]string{"from", "to"}, map[string]any{
			"vault": vaultProp,
			"from":  prop("string", "Current path."),
			"to":    prop("string", "New path."),
		}), Annotations: writes(true, false), run: moveNote,
	},
	{
		Name: "delete_note", Title: "Delete note", write: true,
		Description: "Deletes a file. It goes to the vault's trash in the web admin and can be restored from there.",
		InputSchema: schema([]string{"path"}, map[string]any{
			"vault": vaultProp,
			"path":  prop("string", "Path of the file."),
		}), Annotations: writes(true, true), run: deleteNote,
	},
}

func toolList(canWrite bool) []*tool {
	out := []*tool{}
	for _, t := range tools {
		if canWrite || !t.write {
			out = append(out, t)
		}
	}
	return out
}

func findTool(name string, canWrite bool) *tool {
	for _, t := range toolList(canWrite) {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// ---- arguments ----

func argStr(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func argInt(args map[string]any, key string, def, max int) int {
	n := def
	switch v := args[key].(type) {
	case float64:
		n = int(v)
	case string:
		if x, err := strconv.Atoi(v); err == nil {
			n = x
		}
	}
	if n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return n
}

// ---- access ----

// vaults returns the vaults the token opens, with the user's role in each.
func (s *Server) vaults(ctx context.Context, c *caller) ([]*store.Vault, error) {
	if c.token.VaultID != 0 {
		role, err := s.Store.Role(ctx, c.user, c.token.VaultID)
		if err != nil || role == "" {
			return nil, nil
		}
		v, err := s.Store.Vault(ctx, c.token.VaultID)
		if err != nil {
			return nil, nil
		}
		v.Role = role
		return []*store.Vault{v}, nil
	}
	if c.user.IsAdmin {
		return s.Store.ListVaults(ctx)
	}
	return s.Store.UserVaults(ctx, c.user.ID)
}

func canWrite(c *caller, v *store.Vault) bool {
	return c.token.CanWrite && store.RoleAtLeast(v.Role, store.RoleEditor)
}

// vault picks the vault named in args (by name or id).
func (s *Server) vault(ctx context.Context, c *caller, args map[string]any, write bool) (*store.Vault, error) {
	vs, err := s.vaults(ctx, c)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, errors.New("you have no access to any vault")
	}
	want := strings.TrimSpace(argStr(args, "vault"))
	var v *store.Vault
	if want == "" {
		if len(vs) > 1 {
			names := []string{}
			for _, x := range vs {
				names = append(names, x.Name)
			}
			return nil, fmt.Errorf("say which vault: %s", strings.Join(names, ", "))
		}
		v = vs[0]
	}
	for _, x := range vs {
		if v == nil && (strings.EqualFold(x.Name, want) || strconv.FormatInt(x.ID, 10) == want) {
			v = x
		}
	}
	if v == nil {
		return nil, fmt.Errorf("vault %q not found or you have no access (see list_vaults)", want)
	}
	if write && !canWrite(c, v) {
		return nil, fmt.Errorf("you have read-only access to vault %q", v.Name)
	}
	return v, nil
}

// ---- paths and content ----

// hidden paths (".obsidian", ".trash", ...) are Obsidian's own files and
// are not shown to agents, as in the web editor.
func hidden(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// isText matches the web editor's list of text files.
func isText(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".txt", ".canvas", ".json", ".css", ".js", ".csv", ".yaml", ".yml", ".html", ".xml", ".svg", ".base":
		return true
	}
	return false
}

// notePath validates a path given by an agent; with addMD a path without
// an extension gets ".md".
func notePath(p string, addMD bool) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if addMD && path.Ext(p) == "" && p != "" {
		p += ".md"
	}
	n, err := pathutil.Normalize(p)
	if err != nil || hidden(n) || strings.ContainsAny(n, "\\:*?\"<>|") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	for _, seg := range strings.Split(n, "/") {
		if strings.TrimSpace(seg) != seg {
			return "", fmt.Errorf("invalid path %q: names cannot start or end with a space", p)
		}
	}
	return n, nil
}

func folderPrefix(args map[string]any) string {
	f := strings.Trim(strings.TrimSpace(argStr(args, "folder")), "/")
	if f == "" {
		return ""
	}
	return f + "/"
}

// live returns the current entry for p, or nil.
func (s *Server) live(ctx context.Context, v *store.Vault, p string) *store.FileEntry {
	f, err := s.Store.File(ctx, v.ID, p)
	if err != nil || f.Deleted {
		return nil
	}
	return f
}

// find resolves a path to an existing file, trying ".md" when it has no
// extension.
func (s *Server) find(ctx context.Context, v *store.Vault, raw string) (*store.FileEntry, error) {
	p, err := notePath(raw, false)
	if err != nil {
		return nil, err
	}
	if f := s.live(ctx, v, p); f != nil {
		return f, nil
	}
	if path.Ext(p) == "" {
		if f := s.live(ctx, v, p+".md"); f != nil {
			return f, nil
		}
	}
	return nil, fmt.Errorf("%q does not exist in vault %q", p, v.Name)
}

func (s *Server) text(hash string) (string, bool) {
	f, err := s.Blobs.Open(hash)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxText+1))
	if err != nil || len(b) > maxText || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

func when(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// ---- writing ----

func author(c *caller) store.Author {
	return store.Author{Name: c.user.Username + " (" + c.token.Name + ")"}
}

func (s *Server) commit(ctx context.Context, c *caller, v *store.Vault, ops []store.Op) ([]store.OpResult, error) {
	res, head, err := s.Store.Commit(ctx, v.ID, ops, author(c), s.Blobs.Has)
	if err != nil {
		return nil, err
	}
	s.Hub.Notify(v.ID, head)
	return res, nil
}

func (s *Server) put(text string) (string, error) {
	if len(text) > maxText {
		return "", fmt.Errorf("the text is too long (max %d MB)", maxText>>20)
	}
	return s.Blobs.PutBytes([]byte(text))
}

func opError(r store.OpResult) error {
	switch r.Error {
	case store.ErrCodeCaseConflict:
		return fmt.Errorf("%q already exists with different letter case", r.Current.Path)
	case store.ErrCodeConflict:
		return fmt.Errorf("%q was changed at the same time, try again", r.Path)
	}
	return fmt.Errorf("saving %q failed: %s", r.Path, r.Error)
}

func nowMs() int64 { return time.Now().UnixMilli() }

// conflictPath names a copy the way the plugin and the web editor do:
// "Note (conflict 2026-09-26 1530 jirka).md".
func (s *Server) conflictPath(ctx context.Context, v *store.Vault, p, who string) string {
	dir, name := path.Split(p)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	stamp := time.Now().Format("2006-01-02 1504")
	who = strings.TrimSpace(strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|#^[]`, r) {
			return -1
		}
		return r
	}, who))
	for n := 1; ; n++ {
		suffix := ""
		if n > 1 {
			suffix = fmt.Sprintf(" %d", n)
		}
		cp := fmt.Sprintf("%s%s (conflict %s %s%s)%s", dir, stem, stamp, who, suffix, ext)
		if s.live(ctx, v, cp) == nil {
			return cp
		}
	}
}

// save writes text to p if the server still has base ("" = new file);
// otherwise it merges with the newer version or keeps a conflict copy,
// exactly like a device would.
func (s *Server) save(ctx context.Context, c *caller, v *store.Vault, p, base, text string) (string, error) {
	hash, err := s.put(text)
	if err != nil {
		return "", err
	}
	res, err := s.commit(ctx, c, v, []store.Op{{Path: p, Hash: hash, Size: int64(len(text)), Mtime: nowMs(), BaseHash: base}})
	if err != nil {
		return "", err
	}
	if res[0].OK {
		return fmt.Sprintf("Saved %s (hash %s).", res[0].Path, hash), nil
	}
	if res[0].Error != store.ErrCodeConflict {
		return "", opError(res[0])
	}
	cur := res[0].Current
	if cur == nil || cur.Deleted {
		// Deleted meanwhile: an edit beats a delete.
		res, err := s.commit(ctx, c, v, []store.Op{{Path: p, Hash: hash, Size: int64(len(text)), Mtime: nowMs()}})
		if err != nil {
			return "", err
		}
		if !res[0].OK {
			return "", opError(res[0])
		}
		return fmt.Sprintf("Saved %s (hash %s).", p, hash), nil
	}
	theirs, okT := s.text(cur.Hash)
	baseText, okB := "", base == ""
	if base != "" {
		baseText, okB = s.text(base)
	}
	if okT && okB {
		if merged, ok := textmerge.Merge(baseText, text, theirs); ok {
			if mh, err := s.put(merged); err == nil {
				res, err := s.commit(ctx, c, v, []store.Op{{Path: p, Hash: mh, Size: int64(len(merged)), Mtime: nowMs(), BaseHash: cur.Hash}})
				if err == nil && res[0].OK {
					return fmt.Sprintf("Saved %s; it had changed meanwhile, so both versions were merged (hash %s).", p, mh), nil
				}
			}
		}
	}
	cp := s.conflictPath(ctx, v, p, c.user.Username)
	cres, err := s.commit(ctx, c, v, []store.Op{{Path: cp, Hash: hash, Size: int64(len(text)), Mtime: nowMs()}})
	if err != nil {
		return "", err
	}
	if !cres[0].OK {
		return "", opError(cres[0])
	}
	return fmt.Sprintf("%s had overlapping changes made meanwhile, so it was left as it is and your version was saved as %s.", p, cp), nil
}

// ---- tools ----

func listVaults(ctx context.Context, s *Server, c *caller, _ map[string]any) (string, error) {
	vs, err := s.vaults(ctx, c)
	if err != nil {
		return "", err
	}
	if len(vs) == 0 {
		return "You have no access to any vault.", nil
	}
	var b strings.Builder
	for _, v := range vs {
		st, _ := s.Store.VaultStats(ctx, v.ID)
		access := "read-only"
		if canWrite(c, v) {
			access = "read-write"
		}
		fmt.Fprintf(&b, "- %s (id %d): %s, %d files\n", v.Name, v.ID, access, st.Files)
	}
	return b.String(), nil
}

func listNotes(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, false)
	if err != nil {
		return "", err
	}
	files, err := s.Store.ListFiles(ctx, v.ID, false)
	if err != nil {
		return "", err
	}
	prefix, limit := folderPrefix(args), argInt(args, "limit", 500, 5000)
	var b strings.Builder
	n := 0
	for _, f := range files {
		if hidden(f.Path) || !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		n++
		if n <= limit {
			fmt.Fprintf(&b, "%s  (%s, %s)\n", f.Path, humanSize(f.Size), when(time.UnixMilli(f.Mtime)))
		}
	}
	switch {
	case n == 0 && prefix != "":
		return fmt.Sprintf("No files in folder %q of vault %q.", strings.TrimSuffix(prefix, "/"), v.Name), nil
	case n == 0:
		return fmt.Sprintf("Vault %q is empty.", v.Name), nil
	case n > limit:
		fmt.Fprintf(&b, "... and %d more (raise limit or pick a folder)\n", n-limit)
	}
	return b.String(), nil
}

func readNote(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, false)
	if err != nil {
		return "", err
	}
	f, err := s.find(ctx, v, argStr(args, "path"))
	if err != nil {
		return "", err
	}
	if !isText(f.Path) {
		return "", fmt.Errorf("%q is not a text file (%s)", f.Path, humanSize(f.Size))
	}
	text, ok := s.text(f.Hash)
	if !ok {
		return "", fmt.Errorf("%q is too large or not valid text", f.Path)
	}
	return fmt.Sprintf("path: %s\nmodified: %s\nhash: %s\n\n%s", f.Path, when(time.UnixMilli(f.Mtime)), f.Hash, text), nil
}

func searchNotes(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, false)
	if err != nil {
		return "", err
	}
	terms := strings.Fields(strings.ToLower(argStr(args, "query")))
	if len(terms) == 0 {
		return "", errors.New("query is empty")
	}
	files, err := s.Store.ListFiles(ctx, v.ID, false)
	if err != nil {
		return "", err
	}
	prefix, limit := folderPrefix(args), argInt(args, "limit", 20, 100)
	containsAll := func(hay string) bool {
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				return false
			}
		}
		return true
	}
	type hit struct {
		path  string
		lines []string
		name  bool
	}
	var hits []hit
	for _, f := range files {
		if hidden(f.Path) || !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		lowPath := strings.ToLower(f.Path)
		h := hit{path: f.Path, name: containsAll(lowPath)}
		if isText(f.Path) && f.Size <= maxText {
			if text, ok := s.text(f.Hash); ok {
				low := strings.ToLower(text)
				if h.name || containsAll(lowPath+"\n"+low) {
					for _, line := range strings.Split(text, "\n") {
						if len(h.lines) == 3 {
							break
						}
						ll := strings.ToLower(line)
						for _, t := range terms {
							if strings.Contains(ll, t) {
								line = strings.TrimSpace(line)
								if r := []rune(line); len(r) > 200 {
									line = string(r[:200]) + "…"
								}
								h.lines = append(h.lines, line)
								break
							}
						}
					}
					hits = append(hits, h)
					continue
				}
			}
		}
		if h.name {
			hits = append(hits, h)
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("Nothing found for %q.", argStr(args, "query")), nil
	}
	// Name matches first, then the rest in path order.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].name && !hits[j].name })
	var b bytes.Buffer
	for i, h := range hits {
		if i == limit {
			fmt.Fprintf(&b, "... and %d more files\n", len(hits)-limit)
			break
		}
		fmt.Fprintf(&b, "%s\n", h.path)
		for _, l := range h.lines {
			fmt.Fprintf(&b, "    %s\n", l)
		}
	}
	return b.String(), nil
}

func recentChanges(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, false)
	if err != nil {
		return "", err
	}
	vs, err := s.Store.RecentVersions(ctx, v.ID, argInt(args, "limit", 30, 500)*2)
	if err != nil {
		return "", err
	}
	limit := argInt(args, "limit", 30, 500)
	var b strings.Builder
	n := 0
	for _, x := range vs {
		if hidden(x.Path) {
			continue
		}
		if n++; n > limit {
			break
		}
		what := "changed"
		if x.Deleted {
			what = "deleted"
		}
		fmt.Fprintf(&b, "%s  %s %s by %s\n", when(x.CreatedAt), what, x.Path, x.Author)
	}
	if n == 0 {
		return "No changes yet.", nil
	}
	return b.String(), nil
}

func content(args map[string]any) (string, error) {
	if _, ok := args["content"].(string); !ok {
		return "", errors.New("content is required")
	}
	return args["content"].(string), nil
}

func textPath(raw string) (string, error) {
	p, err := notePath(raw, true)
	if err != nil {
		return "", err
	}
	if !isText(p) {
		return "", fmt.Errorf("%q is not a text file; only notes and other text files can be written", p)
	}
	return p, nil
}

func createNote(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, true)
	if err != nil {
		return "", err
	}
	p, err := textPath(argStr(args, "path"))
	if err != nil {
		return "", err
	}
	text, err := content(args)
	if err != nil {
		return "", err
	}
	if s.live(ctx, v, p) != nil {
		return "", fmt.Errorf("%q already exists; use update_note or append_to_note", p)
	}
	hash, err := s.put(text)
	if err != nil {
		return "", err
	}
	res, err := s.commit(ctx, c, v, []store.Op{{Path: p, Hash: hash, Size: int64(len(text)), Mtime: nowMs()}})
	if err != nil {
		return "", err
	}
	if !res[0].OK {
		if res[0].Error == store.ErrCodeConflict {
			return "", fmt.Errorf("%q already exists", p)
		}
		return "", opError(res[0])
	}
	return fmt.Sprintf("Created %s (hash %s).", res[0].Path, hash), nil
}

func updateNote(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, true)
	if err != nil {
		return "", err
	}
	f, err := s.find(ctx, v, argStr(args, "path"))
	if err != nil {
		return "", fmt.Errorf("%v; use create_note for a new note", err)
	}
	if !isText(f.Path) {
		return "", fmt.Errorf("%q is not a text file", f.Path)
	}
	text, err := content(args)
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(argStr(args, "base_hash"))
	if base == "" {
		base = f.Hash
	}
	return s.save(ctx, c, v, f.Path, base, text)
}

func appendNote(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, true)
	if err != nil {
		return "", err
	}
	p, err := textPath(argStr(args, "path"))
	if err != nil {
		return "", err
	}
	add, err := content(args)
	if err != nil {
		return "", err
	}
	// Retry when someone saves the same note at that very moment.
	for try := 0; try < 5; try++ {
		cur := s.live(ctx, v, p)
		base, text := "", add
		if cur != nil {
			old, ok := s.text(cur.Hash)
			if !ok {
				return "", fmt.Errorf("%q is too large or not valid text", p)
			}
			if old != "" && !strings.HasSuffix(old, "\n") {
				old += "\n"
			}
			base, text = cur.Hash, old+add
		}
		hash, err := s.put(text)
		if err != nil {
			return "", err
		}
		res, err := s.commit(ctx, c, v, []store.Op{{Path: p, Hash: hash, Size: int64(len(text)), Mtime: nowMs(), BaseHash: base}})
		if err != nil {
			return "", err
		}
		if res[0].OK {
			if cur == nil {
				return fmt.Sprintf("Created %s.", p), nil
			}
			return fmt.Sprintf("Appended to %s.", p), nil
		}
		if res[0].Error != store.ErrCodeConflict {
			return "", opError(res[0])
		}
	}
	return "", fmt.Errorf("%q keeps changing, try again later", p)
}

func moveNote(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, true)
	if err != nil {
		return "", err
	}
	f, err := s.find(ctx, v, argStr(args, "from"))
	if err != nil {
		return "", err
	}
	to, err := notePath(argStr(args, "to"), path.Ext(f.Path) == ".md")
	if err != nil {
		return "", err
	}
	if to == f.Path {
		return "", errors.New("the new path is the same as the old one")
	}
	if s.live(ctx, v, to) != nil {
		return "", fmt.Errorf("%q already exists", to)
	}
	res, err := s.commit(ctx, c, v, []store.Op{{Path: to, Hash: f.Hash, Size: f.Size, Mtime: f.Mtime}})
	if err != nil {
		return "", err
	}
	if !res[0].OK {
		return "", opError(res[0])
	}
	// The old path goes only once the new one is there, so nothing is lost.
	if _, err := s.commit(ctx, c, v, []store.Op{{Path: f.Path, Deleted: true, BaseHash: f.Hash, Mtime: nowMs()}}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Moved %s to %s.", f.Path, res[0].Path), nil
}

func deleteNote(ctx context.Context, s *Server, c *caller, args map[string]any) (string, error) {
	v, err := s.vault(ctx, c, args, true)
	if err != nil {
		return "", err
	}
	f, err := s.find(ctx, v, argStr(args, "path"))
	if err != nil {
		return "", err
	}
	res, err := s.commit(ctx, c, v, []store.Op{{Path: f.Path, Deleted: true, BaseHash: f.Hash, Mtime: nowMs()}})
	if err != nil {
		return "", err
	}
	if !res[0].OK {
		return "", opError(res[0])
	}
	return fmt.Sprintf("Deleted %s (it can be restored from the vault's trash).", f.Path), nil
}
