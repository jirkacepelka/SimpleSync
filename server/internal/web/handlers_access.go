package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jirkacepelka/obsisync/server/internal/pathutil"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// ---- per-folder access (vault owners) ----

// accessChanged wakes connected devices so they pick up the new rules.
func (w *Web) accessChanged(r *http.Request, p *page) {
	if v, err := w.Store.Vault(r.Context(), p.Vault.ID); err == nil {
		w.Hub.Notify(v.ID, v.HeadRev)
	}
}

// folderAccessSave switches per-folder access on or off (vault settings).
func (w *Web) folderAccessSave(rw http.ResponseWriter, r *http.Request, p *page) {
	back := fmt.Sprintf("/vaults/%d/settings", p.Vault.ID)
	on := r.FormValue("folder_access") == "1"
	if err := w.Store.SetFolderAccess(r.Context(), p.Vault.ID, on); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	w.accessChanged(r, p)
	if on {
		redirect(rw, r, fmt.Sprintf("/vaults/%d/roles", p.Vault.ID), w.tr(r, "msg.folderAccessOn"), "")
		return
	}
	redirect(rw, r, back+"#access", w.tr(r, "msg.folderAccessOff"), "")
}

// accessPageData adds what the rule forms need: folder suggestions and the
// rules that point at nothing (e.g. the folder was renamed in Obsidian).
func (w *Web) accessPageData(r *http.Request, p *page, rules []store.AccessRule) {
	files, _ := w.Store.ListFiles(r.Context(), p.Vault.ID, false)
	exists := map[string]bool{"": true}
	folders := map[string]bool{}
	for _, f := range files {
		exists[pathutil.Fold(f.Path)] = true
		parts := strings.Split(f.Path, "/")
		for i := 1; i < len(parts); i++ {
			d := strings.Join(parts[:i], "/")
			exists[pathutil.Fold(d)] = true
			if !hiddenPath(d) {
				folders[d] = true
			}
		}
	}
	missing := map[string]bool{}
	for _, rule := range rules {
		if !exists[pathutil.Fold(rule.Path)] {
			missing[rule.Path] = true
		}
	}
	list := make([]string, 0, len(folders))
	for f := range folders {
		list = append(list, f)
	}
	sort.Strings(list)
	p.D["Folders"] = list
	p.D["Missing"] = missing
}

func (w *Web) roles(rw http.ResponseWriter, r *http.Request, p *page) {
	if !p.Vault.FolderAccess {
		http.Redirect(rw, r, fmt.Sprintf("/vaults/%d/settings#access", p.Vault.ID), http.StatusSeeOther)
		return
	}
	p.Title, p.Tab = p.Vault.Name+" – "+w.tr(r, "tab.roles"), "roles"
	roles, err := w.Store.VaultRoles(r.Context(), p.Vault.ID)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	var rules []store.AccessRule
	for _, role := range roles {
		rules = append(rules, role.Rules...)
	}
	p.D = map[string]any{"Roles": roles}
	w.accessPageData(r, p, rules)
	w.render(rw, r, "vault_roles", p)
}

func (w *Web) roleCreate(rw http.ResponseWriter, r *http.Request, p *page) {
	back := fmt.Sprintf("/vaults/%d/roles", p.Vault.ID)
	role, err := w.Store.CreateVaultRole(r.Context(), p.Vault.ID, r.FormValue("name"), r.FormValue("base"))
	if err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	redirect(rw, r, fmt.Sprintf("%s#role-%d", back, role.ID), w.tr(r, "msg.roleCreated", role.Name), "")
}

func (w *Web) roleUpdate(rw http.ResponseWriter, r *http.Request, p *page) {
	id := formIntPath(r, "rid")
	back := fmt.Sprintf("/vaults/%d/roles#role-%d", p.Vault.ID, id)
	if err := w.Store.UpdateVaultRole(r.Context(), p.Vault.ID, id, r.FormValue("name"), r.FormValue("base")); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	w.accessChanged(r, p)
	redirect(rw, r, back, w.tr(r, "msg.settingsSaved"), "")
}

func (w *Web) roleDelete(rw http.ResponseWriter, r *http.Request, p *page) {
	back := fmt.Sprintf("/vaults/%d/roles", p.Vault.ID)
	if err := w.Store.DeleteVaultRole(r.Context(), p.Vault.ID, formIntPath(r, "rid")); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	w.accessChanged(r, p)
	redirect(rw, r, back, w.tr(r, "msg.roleDeleted"), "")
}

// ruleBack is where a rule form returns to: the roles or members page.
func ruleBack(r *http.Request, p *page) string {
	if b := r.FormValue("back"); b == "members" {
		return fmt.Sprintf("/vaults/%d/members#member-%s", p.Vault.ID, r.FormValue("user_id"))
	}
	return fmt.Sprintf("/vaults/%d/roles#role-%s", p.Vault.ID, r.FormValue("role_id"))
}

func (w *Web) ruleSet(rw http.ResponseWriter, r *http.Request, p *page) {
	back := ruleBack(r, p)
	if err := w.Store.SetAccessRule(r.Context(), p.Vault.ID, formInt(r, "role_id"), formInt(r, "user_id"), r.FormValue("path"), r.FormValue("level")); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	w.accessChanged(r, p)
	redirect(rw, r, back, w.tr(r, "msg.ruleSaved"), "")
}

func (w *Web) ruleDelete(rw http.ResponseWriter, r *http.Request, p *page) {
	back := ruleBack(r, p)
	if err := w.Store.RemoveAccessRule(r.Context(), p.Vault.ID, formIntPath(r, "ruleid")); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	w.accessChanged(r, p)
	redirect(rw, r, back, w.tr(r, "msg.ruleRemoved"), "")
}

// setMemberRole applies the role picked in the members form: a built-in
// role, or "r:<id>" for a custom role.
func (w *Web) setMemberRole(r *http.Request, p *page, userID int64, value string) error {
	if id, ok := strings.CutPrefix(value, "r:"); ok {
		rid, _ := strconv.ParseInt(id, 10, 64)
		return w.Store.SetMemberCustomRole(r.Context(), p.Vault.ID, userID, rid)
	}
	return w.Store.SetMember(r.Context(), p.Vault.ID, userID, value)
}
