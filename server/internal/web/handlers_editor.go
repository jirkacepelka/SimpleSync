package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jirkacepelka/obsisync/server/internal/i18n"
	"github.com/jirkacepelka/obsisync/server/internal/markdown"
	"github.com/jirkacepelka/obsisync/server/internal/pathutil"
	"github.com/jirkacepelka/obsisync/server/internal/store"
	"github.com/jirkacepelka/obsisync/server/internal/textmerge"
)

// editorMax is the largest note the web editor opens or saves.
const editorMax = 2 << 20

// editorKeys are the translations the editor's script needs.
var editorKeys = []string{
	"editor.saved", "editor.saving", "editor.unsaved", "editor.offline", "editor.readOnly",
	"editor.newNote", "editor.newFolder", "editor.rename", "editor.delete", "editor.deleteConfirm", "editor.deleteFolderConfirm",
	"editor.namePrompt", "editor.folderPrompt", "editor.untitled", "editor.cancel", "editor.ok",
	"editor.merged", "editor.conflictCopy", "editor.changedElsewhere", "editor.deletedElsewhere", "editor.exists",
	"editor.uploadFailed", "editor.uploading", "editor.saveFailed", "editor.sessionExpired", "editor.binary",
	"editor.publish", "editor.unpublish", "editor.published", "editor.viewPublished",
	"editor.search", "editor.empty", "editor.emptyHint", "editor.tooLarge", "editor.caseConflict", "editor.invalidName",
}

type editorFile struct {
	Path  string `json:"path"`
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}

func jsonErr(rw http.ResponseWriter, status int, code, msg string) {
	writeJSON(rw, status, map[string]string{"error": code, "message": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, editorMax+64<<10)).Decode(v)
}

// ---- the editor page ----

func (w *Web) editor(rw http.ResponseWriter, r *http.Request, p *page) {
	p.Title, p.Tab, p.Wide = p.Vault.Name, "notes", true
	lang := i18n.FromRequest(r)
	tr := map[string]string{}
	for _, k := range editorKeys {
		tr[strings.TrimPrefix(k, "editor.")] = i18n.T(lang, k)
	}
	pub, _ := w.Store.PublishOf(r.Context(), p.Vault.ID)
	cfg := map[string]any{
		"vault":     p.Vault.ID,
		"csrf":      p.CSRF,
		"editable":  store.RoleAtLeast(p.Role, store.RoleEditor),
		"path":      r.URL.Query().Get("path"),
		"user":      p.User.Username,
		"vaultName": p.Vault.Name,
		"publish":   map[string]any{"enabled": pub.Enabled, "mode": pub.Mode, "folder": pub.Folder, "slug": pub.Slug},
		"t":         tr,
	}
	p.D = map[string]any{"Config": cfg}
	w.render(rw, r, "editor", p)
}

// ---- JSON API used by static/editor.js ----

func (w *Web) apiTree(rw http.ResponseWriter, r *http.Request, p *page) {
	if since := r.URL.Query().Get("rev"); since != "" && since == fmt.Sprint(p.Vault.HeadRev) {
		writeJSON(rw, 200, map[string]any{"same": true, "rev": p.Vault.HeadRev})
		return
	}
	files, err := w.Store.ListFiles(r.Context(), p.Vault.ID, false)
	if err != nil {
		jsonErr(rw, 500, "internal", err.Error())
		return
	}
	out := []editorFile{}
	for _, f := range files {
		if !hiddenPath(f.Path) {
			out = append(out, editorFile{f.Path, f.Hash, f.Size, f.Mtime})
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path) })
	writeJSON(rw, 200, map[string]any{"files": out, "rev": p.Vault.HeadRev})
}

// current returns the live entry for fp, or nil when it does not exist.
func (w *Web) current(r *http.Request, vaultID int64, fp string) *store.FileEntry {
	f, err := w.Store.File(r.Context(), vaultID, fp)
	if err != nil || f.Deleted {
		return nil
	}
	return f
}

// blobText reads a blob as UTF-8 text of at most editorMax bytes.
func (w *Web) blobText(hash string) (string, bool) {
	f, err := w.Blobs.Open(hash)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, editorMax+1))
	if err != nil || len(b) > editorMax || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

func (w *Web) apiNote(rw http.ResponseWriter, r *http.Request, p *page) {
	fp := r.URL.Query().Get("path")
	f := w.current(r, p.Vault.ID, fp)
	if f == nil || hiddenPath(fp) {
		jsonErr(rw, 404, "not_found", w.tr(r, "msg.fileNotFound"))
		return
	}
	out := map[string]any{"path": f.Path, "hash": f.Hash, "size": f.Size, "mtime": f.Mtime}
	if r.URL.Query().Get("meta") == "1" {
		writeJSON(rw, 200, out)
		return
	}
	if isTextName(f.Path) {
		if text, ok := w.blobText(f.Hash); ok {
			out["text"] = text
			writeJSON(rw, 200, out)
			return
		}
	}
	out["binary"] = true
	out["image"] = markdown.IsImage(f.Path)
	writeJSON(rw, 200, out)
}

// validNotePath normalizes a path typed in the editor.
func validNotePath(p string) (string, bool) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	n, err := pathutil.Normalize(p)
	if err != nil || hiddenPath(n) || strings.ContainsAny(n, "\\:*?\"<>|") {
		return "", false
	}
	for _, seg := range strings.Split(n, "/") {
		if strings.TrimSpace(seg) != seg {
			return "", false
		}
	}
	return n, true
}

func (w *Web) commit(r *http.Request, p *page, ops []store.Op) ([]store.OpResult, error) {
	res, head, err := w.Store.Commit(r.Context(), p.Vault.ID, ops, w.webAuthor(p), w.Blobs.Has)
	if err != nil {
		return nil, err
	}
	w.Hub.Notify(p.Vault.ID, head)
	return res, nil
}

func nowMs() int64 { return time.Now().UnixMilli() }

var unsafeName = regexp.MustCompile(`[\\/:*?"<>|#^\[\]]`)

// conflictPath names a copy the way the Obsidian plugin does:
// "Note (conflict 2026-09-26 1530 jirka).md".
func (w *Web) conflictPath(r *http.Request, vaultID int64, fp, user string) string {
	dir, name := path.Split(fp)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	stamp := time.Now().Format("2006-01-02 1504")
	who := strings.TrimSpace(unsafeName.ReplaceAllString(user, ""))
	word := w.tr(r, "editor.conflictWord")
	for n := 1; ; n++ {
		suffix := ""
		if n > 1 {
			suffix = fmt.Sprintf(" %d", n)
		}
		p := fmt.Sprintf("%s%s (%s %s %s%s)%s", dir, stem, word, stamp, who, suffix, ext)
		if w.current(r, vaultID, p) == nil {
			return p
		}
	}
}

func (w *Web) apiSave(rw http.ResponseWriter, r *http.Request, p *page) {
	var req struct {
		Path   string `json:"path"`
		Base   string `json:"base"`
		Text   string `json:"text"`
		Create bool   `json:"create"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErr(rw, 400, "bad_request", err.Error())
		return
	}
	fp, ok := validNotePath(req.Path)
	if !ok || !isTextName(fp) {
		jsonErr(rw, 400, "invalid_name", w.tr(r, "editor.invalidName"))
		return
	}
	if len(req.Text) > editorMax {
		jsonErr(rw, 413, "too_large", w.tr(r, "editor.tooLarge"))
		return
	}
	res, status := w.saveText(r, p, fp, req.Base, req.Text, req.Create)
	writeJSON(rw, status, res)
}

// saveText writes text to fp if the server still has base; otherwise it
// merges with the newer server version or keeps a conflict copy, exactly
// like a device would.
func (w *Web) saveText(r *http.Request, p *page, fp, base, text string, create bool) (map[string]any, int) {
	hash, err := w.Blobs.PutBytes([]byte(text))
	if err != nil {
		return map[string]any{"error": "internal", "message": err.Error()}, 500
	}
	op := store.Op{Path: fp, Hash: hash, Size: int64(len(text)), Mtime: nowMs(), BaseHash: base}
	res, err := w.commit(r, p, []store.Op{op})
	if err != nil {
		return map[string]any{"error": "internal", "message": err.Error()}, 500
	}
	switch res[0].Error {
	case "":
		return map[string]any{"ok": true, "path": res[0].Path, "hash": hash}, 200
	case store.ErrCodeCaseConflict:
		return map[string]any{"error": "case_conflict", "message": w.tr(r, "editor.caseConflict", res[0].Current.Path)}, 409
	case store.ErrCodeConflict:
	default:
		return map[string]any{"error": res[0].Error, "message": w.tr(r, "editor.saveFailed")}, 500
	}
	if create {
		return map[string]any{"error": "exists", "message": w.tr(r, "editor.exists")}, 409
	}
	cur := res[0].Current
	if cur == nil || cur.Deleted {
		// Deleted elsewhere meanwhile: an edit beats a delete.
		op.BaseHash = ""
		res, err := w.commit(r, p, []store.Op{op})
		if err != nil || !res[0].OK {
			return map[string]any{"error": "conflict", "message": w.tr(r, "editor.saveFailed")}, 409
		}
		return map[string]any{"ok": true, "path": fp, "hash": hash}, 200
	}
	theirs, okT := w.blobText(cur.Hash)
	baseText, okB := "", base == ""
	if base != "" {
		baseText, okB = w.blobText(base)
	}
	if okT && okB {
		if merged, ok := textmerge.Merge(baseText, text, theirs); ok {
			mh, err := w.Blobs.PutBytes([]byte(merged))
			if err == nil {
				res, err := w.commit(r, p, []store.Op{{Path: fp, Hash: mh, Size: int64(len(merged)), Mtime: nowMs(), BaseHash: cur.Hash}})
				if err == nil && res[0].OK {
					return map[string]any{"ok": true, "path": fp, "hash": mh, "merged": true, "text": merged, "sent": hash}, 200
				}
			}
		}
	}
	// Overlapping edits: the server version stays, ours goes next to it.
	copyPath := w.conflictPath(r, p.Vault.ID, fp, p.User.Username)
	cres, err := w.commit(r, p, []store.Op{{Path: copyPath, Hash: hash, Size: int64(len(text)), Mtime: nowMs()}})
	if err != nil || !cres[0].OK {
		return map[string]any{"error": "conflict", "message": w.tr(r, "editor.saveFailed")}, 409
	}
	out := map[string]any{"ok": false, "conflict": true, "copy": copyPath, "path": fp, "hash": cur.Hash}
	if okT {
		out["text"] = theirs
	}
	return out, 200
}

// liveUnder returns the live files at p, or under the folder p.
func (w *Web) liveUnder(r *http.Request, vaultID int64, p string, folder bool) ([]store.FileEntry, error) {
	files, err := w.Store.ListFiles(r.Context(), vaultID, false)
	if err != nil {
		return nil, err
	}
	var out []store.FileEntry
	for _, f := range files {
		if (!folder && f.Path == p) || (folder && strings.HasPrefix(f.Path, p+"/")) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (w *Web) apiRename(rw http.ResponseWriter, r *http.Request, p *page) {
	var req struct {
		From   string `json:"from"`
		To     string `json:"to"`
		Folder bool   `json:"folder"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErr(rw, 400, "bad_request", err.Error())
		return
	}
	from, ok1 := validNotePath(req.From)
	to, ok2 := validNotePath(req.To)
	if !ok1 || !ok2 || from == to || (req.Folder && strings.HasPrefix(to+"/", from+"/")) {
		jsonErr(rw, 400, "invalid_name", w.tr(r, "editor.invalidName"))
		return
	}
	files, err := w.liveUnder(r, p.Vault.ID, from, req.Folder)
	if err != nil || len(files) == 0 {
		jsonErr(rw, 404, "not_found", w.tr(r, "msg.fileNotFound"))
		return
	}
	var creates []store.Op
	for _, f := range files {
		np := to
		if req.Folder {
			np = to + strings.TrimPrefix(f.Path, from)
		}
		if w.current(r, p.Vault.ID, np) != nil {
			jsonErr(rw, 409, "exists", w.tr(r, "editor.exists"))
			return
		}
		creates = append(creates, store.Op{Path: np, Hash: f.Hash, Size: f.Size, Mtime: f.Mtime})
	}
	res, err := w.commit(r, p, creates)
	if err != nil {
		jsonErr(rw, 500, "internal", err.Error())
		return
	}
	// Remove the old paths only where the new copy landed, so nothing is
	// ever lost halfway through.
	var deletes []store.Op
	failed := ""
	for i, f := range files {
		if res[i].OK {
			deletes = append(deletes, store.Op{Path: f.Path, Deleted: true, BaseHash: f.Hash, Mtime: nowMs()})
		} else if failed == "" {
			failed = res[i].Error
		}
	}
	if len(deletes) > 0 {
		if _, err := w.commit(r, p, deletes); err != nil {
			jsonErr(rw, 500, "internal", err.Error())
			return
		}
	}
	if failed != "" {
		msg := w.tr(r, "editor.saveFailed")
		if failed == store.ErrCodeCaseConflict {
			msg = w.tr(r, "editor.exists")
		}
		jsonErr(rw, 409, failed, msg)
		return
	}
	writeJSON(rw, 200, map[string]any{"ok": true, "path": res[0].Path})
}

func (w *Web) apiDelete(rw http.ResponseWriter, r *http.Request, p *page) {
	var req struct {
		Path   string `json:"path"`
		Folder bool   `json:"folder"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErr(rw, 400, "bad_request", err.Error())
		return
	}
	target, ok := validNotePath(req.Path)
	if !ok {
		jsonErr(rw, 400, "invalid_name", w.tr(r, "editor.invalidName"))
		return
	}
	files, err := w.liveUnder(r, p.Vault.ID, target, req.Folder)
	if err != nil || len(files) == 0 {
		jsonErr(rw, 404, "not_found", w.tr(r, "msg.fileNotFound"))
		return
	}
	var ops []store.Op
	for _, f := range files {
		ops = append(ops, store.Op{Path: f.Path, Deleted: true, BaseHash: f.Hash, Mtime: nowMs()})
	}
	if _, err := w.commit(r, p, ops); err != nil {
		jsonErr(rw, 500, "internal", err.Error())
		return
	}
	writeJSON(rw, 200, map[string]any{"ok": true})
}

var uploadName = regexp.MustCompile(`[\\/:*?"<>|#^\[\]\x00-\x1f]`)

func (w *Web) apiUpload(rw http.ResponseWriter, r *http.Request, p *page) {
	max := w.Store.Settings(r.Context()).MaxFileMB << 20
	r.Body = http.MaxBytesReader(rw, r.Body, max+1<<20)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		jsonErr(rw, 400, "bad_request", w.tr(r, "editor.uploadFailed"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(data)) > max {
		jsonErr(rw, 413, "too_large", w.tr(r, "editor.tooLarge"))
		return
	}
	name := strings.TrimSpace(uploadName.ReplaceAllString(path.Base(hdr.Filename), ""))
	ext := strings.ToLower(path.Ext(name))
	if name == "" || strings.HasPrefix(name, ".") || ext == "" || strings.HasPrefix(name, "image.") {
		if ext == "" {
			ext = ".png"
		}
		name = "Pasted image " + time.Now().Format("20060102150405") + ext
	}
	dir := strings.Trim(r.FormValue("dir"), "/")
	if dir != "" {
		if d, ok := validNotePath(dir); ok {
			dir = d
		} else {
			dir = ""
		}
	}
	stem := strings.TrimSuffix(name, path.Ext(name))
	target := ""
	for n := 0; ; n++ {
		cand := name
		if n > 0 {
			cand = fmt.Sprintf("%s %d%s", stem, n, path.Ext(name))
		}
		target = path.Join(dir, cand)
		if w.current(r, p.Vault.ID, target) == nil {
			break
		}
	}
	if _, ok := validNotePath(target); !ok {
		jsonErr(rw, 400, "invalid_name", w.tr(r, "editor.invalidName"))
		return
	}
	hash, err := w.Blobs.PutBytes(data)
	if err != nil {
		jsonErr(rw, 500, "internal", err.Error())
		return
	}
	res, err := w.commit(r, p, []store.Op{{Path: target, Hash: hash, Size: int64(len(data)), Mtime: nowMs()}})
	if err != nil || !res[0].OK {
		jsonErr(rw, 500, "internal", w.tr(r, "editor.uploadFailed"))
		return
	}
	writeJSON(rw, 200, map[string]any{"ok": true, "path": res[0].Path})
}

func (w *Web) apiRender(rw http.ResponseWriter, r *http.Request, p *page) {
	var req struct {
		Path string `json:"path"`
		Text string `json:"text"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErr(rw, 400, "bad_request", err.Error())
		return
	}
	files, err := w.Store.ListFiles(r.Context(), p.Vault.ID, false)
	if err != nil {
		jsonErr(rw, 500, "internal", err.Error())
		return
	}
	ix := filesIndex(files, nil)
	base := fmt.Sprintf("/vaults/%d", p.Vault.ID)
	res, err := markdown.Render([]byte(req.Text), markdown.Options{
		Properties: true,
		Link: func(t string) (string, bool) {
			fp, anchor, ok := ix.resolve(req.Path, t)
			if !ok {
				return "", false
			}
			if fp == req.Path && anchor != "" {
				return "#" + anchor, true
			}
			return withAnchor(base+"/notes?path="+url.QueryEscape(fp), anchor), true
		},
		Embed: func(t string) (string, bool) {
			fp, _, ok := ix.resolve(req.Path, t)
			if !ok {
				return "", false
			}
			return base + "/raw?inline=1&path=" + url.QueryEscape(fp), true
		},
	})
	if err != nil {
		jsonErr(rw, 500, "internal", err.Error())
		return
	}
	writeJSON(rw, 200, map[string]any{"html": res.HTML})
}

// raw serves the current content of a file (images inline, anything else
// as a download).
func (w *Web) raw(rw http.ResponseWriter, r *http.Request, p *page) {
	fp := r.URL.Query().Get("path")
	f := w.current(r, p.Vault.ID, fp)
	if f == nil {
		w.fail(rw, r, http.StatusNotFound, w.tr(r, "msg.fileNotFound"))
		return
	}
	w.serveBlob(rw, r, f.Hash, f.Path)
}
