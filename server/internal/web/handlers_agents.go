package web

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jirkacepelka/obsisync/server/internal/auth"
	"github.com/jirkacepelka/obsisync/server/internal/mcp"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// ---- AI agents: API tokens for the MCP endpoint ----

func (w *Web) userVaults(r *http.Request, u *store.User) []*store.Vault {
	var vs []*store.Vault
	if u.IsAdmin {
		vs, _ = w.Store.ListVaults(r.Context())
	} else {
		vs, _ = w.Store.UserVaults(r.Context(), u.ID)
	}
	return vs
}

func (w *Web) agentsPage(rw http.ResponseWriter, r *http.Request, p *page, newToken string) {
	p.Title, p.Nav = w.tr(r, "title.agents"), "agents"
	var uid int64
	if !p.User.IsAdmin {
		uid = p.User.ID
	}
	toks, err := w.Store.ListAPITokens(r.Context(), uid)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	p.D = map[string]any{"Tokens": toks, "Vaults": w.userVaults(r, p.User), "MCPURL": mcp.BaseURL(r) + mcp.Path, "NewToken": newToken}
	w.render(rw, r, "agents", p)
}

func (w *Web) agents(rw http.ResponseWriter, r *http.Request, p *page) {
	w.agentsPage(rw, r, p, "")
}

// vaultChoice checks the vault picked in a form: 0 (all) or one the user can open.
func (w *Web) vaultChoice(r *http.Request, u *store.User, raw string) (int64, bool) {
	if raw == "" || raw == "0" {
		return 0, true
	}
	vid, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	role, err := w.Store.Role(r.Context(), u, vid)
	return vid, err == nil && role != ""
}

func (w *Web) agentCreate(rw http.ResponseWriter, r *http.Request, p *page) {
	vid, ok := w.vaultChoice(r, p.User, r.FormValue("vault"))
	if !ok {
		redirect(rw, r, "/agents", "", w.tr(r, "msg.vaultNotFound"))
		return
	}
	tok, hash := auth.NewToken("osa_")
	if _, err := w.Store.CreateAPIToken(r.Context(), p.User.ID, r.FormValue("name"), hash, vid, r.FormValue("access") == "write", ""); err != nil {
		redirect(rw, r, "/agents", "", err.Error())
		return
	}
	w.Log.Info("api token created", "user", p.User.Username, "name", r.FormValue("name"))
	// The token is shown once, right here; only its hash is stored.
	w.agentsPage(rw, r, p, tok)
}

func (w *Web) agentDelete(rw http.ResponseWriter, r *http.Request, p *page) {
	t, err := w.Store.APIToken(r.Context(), formIntPath(r, "tid"))
	if err != nil || (t.UserID != p.User.ID && !p.User.IsAdmin) {
		redirect(rw, r, "/agents", "", w.tr(r, "msg.tokenNotFound"))
		return
	}
	w.Store.DeleteAPIToken(r.Context(), t.ID)
	redirect(rw, r, "/agents", w.tr(r, "msg.tokenRevoked", t.Name), "")
}

// ---- OAuth consent page (MCP clients such as Claude sign in here) ----

func (w *Web) oauthAuthorize(rw http.ResponseWriter, r *http.Request, p *page) {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		q = r.PostForm
	}
	ar, oauthErr, err := mcp.ParseAuthorize(r, w.Store, q)
	if err != nil { // mcp.ErrBadAuthorize: never redirect to an unchecked address
		w.fail(rw, r, http.StatusBadRequest, w.tr(r, "oauth.badRequest"))
		return
	}
	if oauthErr != "" {
		http.Redirect(rw, r, ar.RedirectWith(url.Values{"error": {oauthErr}}), http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet {
		redirectURL, _ := url.Parse(ar.RedirectURI)
		p.Title = w.tr(r, "title.oauth")
		p.D = map[string]any{"Client": ar.Client.Name, "Host": redirectURL.Host, "Vaults": w.userVaults(r, p.User), "Q": q}
		// The form's answer redirects to the client, which the default
		// form-action 'self' would block.
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self' "+
			redirectURL.Scheme+"://"+redirectURL.Host+"; frame-ancestors 'none'")
		w.render(rw, r, "oauth_authorize", p)
		return
	}
	if r.FormValue("decision") != "allow" {
		http.Redirect(rw, r, ar.RedirectWith(url.Values{"error": {"access_denied"}}), http.StatusSeeOther)
		return
	}
	vid, ok := w.vaultChoice(r, p.User, r.FormValue("vault"))
	if !ok {
		w.fail(rw, r, http.StatusBadRequest, w.tr(r, "msg.vaultNotFound"))
		return
	}
	code, hash := auth.NewToken("")
	err = w.Store.CreateOAuthCode(r.Context(), hash, store.OAuthCode{
		ClientID: ar.Client.ID, UserID: p.User.ID, RedirectURI: ar.RedirectURI, Challenge: ar.Challenge,
		VaultID: vid, CanWrite: r.FormValue("access") == "write",
	}, 5*time.Minute)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	w.Log.Info("oauth access granted", "user", p.User.Username, "client", ar.Client.Name)
	http.Redirect(rw, r, ar.RedirectWith(url.Values{"code": {code}}), http.StatusSeeOther)
}
