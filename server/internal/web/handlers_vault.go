package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jirkacepelka/obsisync/server/internal/store"
)

type vaultRow struct {
	*store.Vault
	Stats store.VaultStats
}

func (w *Web) visibleVaults(r *http.Request, u *store.User) ([]vaultRow, error) {
	var vs []*store.Vault
	var err error
	if u.IsAdmin {
		vs, err = w.Store.ListVaults(r.Context())
	} else {
		vs, err = w.Store.UserVaults(r.Context(), u.ID)
	}
	if err != nil {
		return nil, err
	}
	out := make([]vaultRow, 0, len(vs))
	for _, v := range vs {
		st, _ := w.Store.VaultStats(r.Context(), v.ID)
		out = append(out, vaultRow{v, st})
	}
	return out, nil
}

func (w *Web) dashboard(rw http.ResponseWriter, r *http.Request, p *page) {
	p.Title, p.Nav = w.tr(r, "title.overview"), "home"
	vaults, err := w.visibleVaults(r, p.User)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	d := map[string]any{"Vaults": vaults, "Online": len(w.Hub.OnlineDevices())}
	if p.User.IsAdmin {
		users, _ := w.Store.ListUsers(r.Context())
		d["Users"] = len(users)
		d["Disk"] = w.Blobs.DiskUsage()
		var failing []vaultRow
		for _, v := range vaults {
			if v.LastBackupError != "" {
				failing = append(failing, v)
			}
		}
		d["Failing"] = failing
	}
	p.D = d
	w.render(rw, r, "dashboard", p)
}

func (w *Web) vaultList(rw http.ResponseWriter, r *http.Request, p *page) {
	p.Title, p.Nav = w.tr(r, "title.vaults"), "vaults"
	vaults, err := w.visibleVaults(r, p.User)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	p.D = map[string]any{"Vaults": vaults}
	w.render(rw, r, "vaults", p)
}

func (w *Web) vaultNewForm(rw http.ResponseWriter, r *http.Request, p *page) {
	p.Title, p.Nav = w.tr(r, "title.newVault"), "vaults"
	users, _ := w.Store.ListUsers(r.Context())
	p.D = map[string]any{"Users": users, "Interval": store.DefaultBackupPolicy.IntervalSec, "Retention": store.DefaultBackupPolicy.RetentionDays}
	w.render(rw, r, "vault_new", p)
}

func backupPolicyFromForm(r *http.Request) store.BackupPolicy {
	return store.BackupPolicy{
		IntervalSec:   formInt(r, "backup_interval"),
		RetentionDays: int(formInt(r, "backup_retention_days")),
		Zip:           r.FormValue("backup_zip") == "1",
	}
}

func (w *Web) vaultCreate(rw http.ResponseWriter, r *http.Request, p *page) {
	if r.FormValue("backup_interval") == "" || r.FormValue("backup_retention_days") == "" {
		redirect(rw, r, "/vaults/new", "", w.tr(r, "msg.pickBackupPolicy"))
		return
	}
	v, err := w.Store.CreateVault(r.Context(), r.FormValue("name"), backupPolicyFromForm(r), 0)
	if err != nil {
		redirect(rw, r, "/vaults/new", "", w.errText(r, err))
		return
	}
	r.ParseForm()
	for _, uid := range r.Form["members"] {
		var id int64
		fmt.Sscan(uid, &id)
		if id != 0 {
			w.Store.SetMember(r.Context(), v.ID, id, store.RoleOwner)
		}
	}
	redirect(rw, r, fmt.Sprintf("/vaults/%d", v.ID), w.tr(r, "msg.vaultCreated", v.Name), "")
}

// ---- file browser ----

type dirEntry struct {
	Name  string
	Path  string
	IsDir bool
	File  store.FileEntry
	Count int
}

func (w *Web) vaultFiles(rw http.ResponseWriter, r *http.Request, p *page) {
	p.Title, p.Tab = p.Vault.Name, "files"
	dir := strings.Trim(r.URL.Query().Get("dir"), "/")
	files, err := w.Store.ListFiles(r.Context(), p.Vault.ID, false)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	dirs := map[string]*dirEntry{}
	var entries []*dirEntry
	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(f.Path, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			name := rest[:i]
			d, ok := dirs[name]
			if !ok {
				d = &dirEntry{Name: name, Path: prefix + name, IsDir: true}
				dirs[name] = d
				entries = append(entries, d)
			}
			d.Count++
		} else {
			entries = append(entries, &dirEntry{Name: rest, Path: f.Path, File: f})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	var crumbs []dirEntry
	if dir != "" {
		acc := ""
		for _, seg := range strings.Split(dir, "/") {
			acc = path.Join(acc, seg)
			crumbs = append(crumbs, dirEntry{Name: seg, Path: acc})
		}
	}
	st, _ := w.Store.VaultStats(r.Context(), p.Vault.ID)
	p.D = map[string]any{"Entries": entries, "Dir": dir, "Crumbs": crumbs, "Stats": st}
	w.render(rw, r, "vault_files", p)
}

func isTextName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".txt", ".canvas", ".json", ".css", ".js", ".csv", ".yaml", ".yml", ".html", ".xml", ".svg", ".base":
		return true
	}
	return false
}

func isImageName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".avif":
		return true
	}
	return false
}

// readText returns the blob as text if it is small UTF-8, else "", false.
func (w *Web) readText(hash string, name string) (string, bool) {
	if !isTextName(name) {
		return "", false
	}
	f, err := w.Blobs.Open(hash)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 512<<10+1))
	if err != nil || len(b) > 512<<10 || !utf8.Valid(b) {
		return "", false
	}
	return string(b), true
}

func (w *Web) fileHistory(rw http.ResponseWriter, r *http.Request, p *page) {
	fp := r.URL.Query().Get("path")
	p.Title, p.Tab = path.Base(fp), "files"
	versions, err := w.Store.Versions(r.Context(), p.Vault.ID, fp)
	if err != nil || len(versions) == 0 {
		w.fail(rw, r, http.StatusNotFound, w.tr(r, "msg.fileNotFound"))
		return
	}
	sel := versions[0]
	if vid := formIntQuery(r, "v"); vid != 0 {
		for _, v := range versions {
			if v.ID == vid {
				sel = v
			}
		}
	}
	// Preview the selected version, or the newest non-deleted one.
	if sel.Deleted {
		for _, v := range versions {
			if !v.Deleted {
				sel = v
				break
			}
		}
	}
	d := map[string]any{"Path": fp, "Dir": path.Dir(fp), "Versions": versions, "Sel": sel, "Current": versions[0]}
	if !sel.Deleted {
		if text, ok := w.readText(sel.Hash, fp); ok {
			d["Text"] = text
		} else if isImageName(fp) {
			d["Image"] = true
		}
	}
	p.D = d
	w.render(rw, r, "file", p)
}

func formIntQuery(r *http.Request, key string) int64 {
	var n int64
	fmt.Sscan(r.URL.Query().Get(key), &n)
	return n
}

// serveBlob sends stored content. Only images are rendered inline; anything
// else is a download, so uploaded HTML/SVG can never run on this origin.
func (w *Web) serveBlob(rw http.ResponseWriter, r *http.Request, hash, name string) {
	f, err := w.Blobs.Open(hash)
	if err != nil {
		w.fail(rw, r, http.StatusNotFound, w.tr(r, "msg.fileNotFound"))
		return
	}
	defer f.Close()
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if r.URL.Query().Get("inline") == "1" && (isImageName(name) || strings.EqualFold(path.Ext(name), ".svg")) {
		rw.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	} else {
		rw.Header().Set("Content-Type", "application/octet-stream")
		rw.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
	}
	io.Copy(rw, f)
}

func (w *Web) versionRaw(rw http.ResponseWriter, r *http.Request, p *page) {
	v, err := w.Store.Version(r.Context(), p.Vault.ID, formIntPath(r, "vid"))
	if err != nil || v.Deleted {
		w.fail(rw, r, http.StatusNotFound, w.tr(r, "msg.fileNotFound"))
		return
	}
	w.serveBlob(rw, r, v.Hash, v.Path)
}

func (w *Web) webAuthor(p *page) store.Author {
	return store.Author{Name: p.User.Username + " (web)"}
}

func (w *Web) commitForce(r *http.Request, p *page, e store.FileEntry) error {
	res, head, err := w.Store.Commit(r.Context(), p.Vault.ID, []store.Op{store.ForceOp(e)}, w.webAuthor(p), w.Blobs.Has)
	if err != nil {
		return err
	}
	if !res[0].OK {
		if res[0].Error == store.ErrCodeCaseConflict {
			return errors.New(w.tr(r, "msg.caseConflict", res[0].Current.Path))
		}
		return errors.New(w.tr(r, "msg.restoreFailed", res[0].Error))
	}
	w.Hub.Notify(p.Vault.ID, head)
	return nil
}

func (w *Web) versionRestore(rw http.ResponseWriter, r *http.Request, p *page) {
	v, err := w.Store.Version(r.Context(), p.Vault.ID, formIntPath(r, "vid"))
	if err != nil || v.Deleted {
		w.fail(rw, r, http.StatusNotFound, w.tr(r, "msg.fileNotFound"))
		return
	}
	back := "/vaults/" + r.PathValue("id") + "/file?path=" + url.QueryEscape(v.Path)
	if err := w.commitForce(r, p, v.FileEntry); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	redirect(rw, r, back, w.tr(r, "msg.versionRestored", w.date(r, v.CreatedAt)), "")
}

func (w *Web) trash(rw http.ResponseWriter, r *http.Request, p *page) {
	p.Title, p.Tab = p.Vault.Name+" – "+w.tr(r, "tab.trash"), "trash"
	files, err := w.Store.ListFiles(r.Context(), p.Vault.ID, true)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	p.D = map[string]any{"Files": files}
	w.render(rw, r, "vault_trash", p)
}

func (w *Web) vaultZip(rw http.ResponseWriter, r *http.Request, p *page) {
	files, err := w.Store.ListFiles(r.Context(), p.Vault.ID, false)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	rw.Header().Set("Content-Type", "application/zip")
	rw.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": p.Vault.Name + ".zip"}))
	if err := w.Backup.WriteZip(rw, files); err != nil {
		w.Log.Error("zip", "err", err)
	}
}

// ---- members & settings ----

func (w *Web) members(rw http.ResponseWriter, r *http.Request, p *page) {
	p.Title, p.Tab = p.Vault.Name+" – "+w.tr(r, "tab.members"), "members"
	members, err := w.Store.Members(r.Context(), p.Vault.ID)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	d := map[string]any{"Members": members}
	if p.User.IsAdmin {
		users, _ := w.Store.ListUsers(r.Context())
		d["Users"] = users
	}
	p.D = d
	w.render(rw, r, "vault_members", p)
}

func (w *Web) memberSet(rw http.ResponseWriter, r *http.Request, p *page) {
	back := "/vaults/" + r.PathValue("id") + "/members"
	var u *store.User
	var err error
	if name := strings.TrimSpace(r.FormValue("username")); name != "" {
		u, err = w.Store.UserByName(r.Context(), name)
	} else {
		u, err = w.Store.UserByID(r.Context(), formInt(r, "user_id"))
	}
	if err != nil {
		redirect(rw, r, back, "", w.tr(r, "msg.userNotFound"))
		return
	}
	if err := w.Store.SetMember(r.Context(), p.Vault.ID, u.ID, r.FormValue("role")); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	redirect(rw, r, back, w.tr(r, "msg.memberAdded", u.Username), "")
}

func (w *Web) memberRemove(rw http.ResponseWriter, r *http.Request, p *page) {
	back := "/vaults/" + r.PathValue("id") + "/members"
	uid := formInt(r, "user_id")
	if uid == p.User.ID && !p.User.IsAdmin {
		redirect(rw, r, back, "", w.tr(r, "msg.cannotRemoveSelf"))
		return
	}
	w.Store.RemoveMember(r.Context(), p.Vault.ID, uid)
	redirect(rw, r, back, w.tr(r, "msg.memberRemoved"), "")
}

func (w *Web) vaultSettings(rw http.ResponseWriter, r *http.Request, p *page) {
	p.Title, p.Tab = p.Vault.Name+" – "+w.tr(r, "tab.settings"), "settings"
	pub, _ := w.Store.PublishOf(r.Context(), p.Vault.ID)
	slug := pub.Slug
	if slug == "" {
		slug = suggestSlug(p.Vault.Name)
	}
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	p.D = map[string]any{"Publish": pub, "Slug": slug, "Origin": scheme + "://" + r.Host}
	w.render(rw, r, "vault_settings", p)
}

func (w *Web) vaultSettingsSave(rw http.ResponseWriter, r *http.Request, p *page) {
	back := "/vaults/" + r.PathValue("id") + "/settings"
	if name := strings.TrimSpace(r.FormValue("name")); name != p.Vault.Name {
		if err := w.Store.RenameVault(r.Context(), p.Vault.ID, name); err != nil {
			redirect(rw, r, back, "", w.errText(r, err))
			return
		}
	}
	if err := w.Store.SetBackupPolicy(r.Context(), p.Vault.ID, backupPolicyFromForm(r)); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	redirect(rw, r, back, w.tr(r, "msg.settingsSaved"), "")
}

func (w *Web) vaultDelete(rw http.ResponseWriter, r *http.Request, p *page) {
	back := "/vaults/" + r.PathValue("id") + "/settings"
	if r.FormValue("confirm") != p.Vault.Name {
		redirect(rw, r, back, "", w.tr(r, "msg.confirmVaultName"))
		return
	}
	backups, _ := w.Store.ListBackups(r.Context(), p.Vault.ID)
	for _, b := range backups {
		w.Backup.Delete(r.Context(), b)
	}
	if err := w.Store.DeleteVault(r.Context(), p.Vault.ID); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	redirect(rw, r, "/vaults", w.tr(r, "msg.vaultDeleted", p.Vault.Name), "")
}
