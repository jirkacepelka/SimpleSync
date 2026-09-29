// Package api implements the sync protocol used by the Obsidian plugin.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/jirkacepelka/obsisync/server/internal/auth"
	"github.com/jirkacepelka/obsisync/server/internal/blobs"
	"github.com/jirkacepelka/obsisync/server/internal/hub"
	"github.com/jirkacepelka/obsisync/server/internal/i18n"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

type API struct {
	Store   *store.Store
	Blobs   *blobs.Store
	Hub     *hub.Hub
	Guard   *auth.LoginGuard
	Version string
	Log     *slog.Logger
}

const maxOpsPerCommit = 500

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/ping", a.ping)
	mux.HandleFunc("POST /api/v1/auth/login", a.login)
	mux.HandleFunc("POST /api/v1/auth/logout", a.authed(a.logout))
	mux.HandleFunc("GET /api/v1/me", a.authed(a.me))
	mux.HandleFunc("GET /api/v1/vaults", a.authed(a.listVaults))
	mux.HandleFunc("POST /api/v1/vaults", a.authed(a.createVault))
	mux.HandleFunc("GET /api/v1/vaults/{id}/changes", a.vault(store.RoleViewer, a.changes))
	mux.HandleFunc("POST /api/v1/vaults/{id}/blobs/missing", a.vault(store.RoleEditor, a.missingBlobs))
	mux.HandleFunc("PUT /api/v1/vaults/{id}/blobs/{hash}", a.vault(store.RoleEditor, a.putBlob))
	mux.HandleFunc("GET /api/v1/vaults/{id}/blobs/{hash}", a.vault(store.RoleViewer, a.getBlob))
	mux.HandleFunc("POST /api/v1/vaults/{id}/commit", a.vault(store.RoleEditor, a.commit))
	mux.HandleFunc("GET /api/v1/vaults/{id}/ws", a.vault(store.RoleViewer, a.ws))
}

// ---- helpers ----

type reqCtx struct {
	user   *store.User
	device *store.Device
	vault  *store.Vault
	role   string
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(v)
}

// ClientIP returns the remote address. Behind a reverse proxy on a local or
// private address it uses the last X-Forwarded-For entry: the address the
// proxy itself saw (earlier entries are whatever the client sent).
func ClientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if f := r.Header.Values("X-Forwarded-For"); len(f) > 0 {
			parts := strings.Split(f[len(f)-1], ",")
			if last := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); last != nil {
				return last.String()
			}
		}
	}
	return host
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	// Browser WebSockets cannot set headers, so the socket passes it in the URL.
	return r.URL.Query().Get("token")
}

func (a *API) authed(h func(http.ResponseWriter, *http.Request, *reqCtx)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
			return
		}
		dev, user, err := a.Store.DeviceByToken(r.Context(), auth.HashToken(tok))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "The login expired or this device was logged out. Log in again.")
			return
		}
		h(w, r, &reqCtx{user: user, device: dev})
	}
}

func (a *API) vault(minRole string, h func(http.ResponseWriter, *http.Request, *reqCtx)) http.HandlerFunc {
	return a.authed(func(w http.ResponseWriter, r *http.Request, c *reqCtx) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "Vault not found")
			return
		}
		role, err := a.Store.Role(r.Context(), c.user, id)
		if err != nil || role == "" {
			writeErr(w, http.StatusNotFound, "not_found", "Vault not found or you have no access")
			return
		}
		if !store.RoleAtLeast(role, minRole) {
			writeErr(w, http.StatusForbidden, "forbidden", "You have read-only access to this vault")
			return
		}
		v, err := a.Store.Vault(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "Vault not found")
			return
		}
		c.vault, c.role = v, role
		a.Store.SetDeviceVault(r.Context(), c.device.ID, v.ID)
		h(w, r, c)
	})
}

// ---- handlers ----

func (a *API) ping(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"app": "obsisync", "version": a.Version})
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request")
		return
	}
	ip := ClientIP(r)
	if !a.Guard.Allowed(ip, req.Username) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Too many attempts, try again in 15 minutes")
		return
	}
	u, err := a.Store.UserByName(r.Context(), req.Username)
	hash := ""
	if err == nil {
		hash = u.PasswordHash()
	}
	if !a.Guard.Check(ip, req.Username, hash, req.Password) {
		a.Log.Warn("failed device login", "user", req.Username, "ip", ip)
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "Wrong name or password")
		return
	}
	tok, hash := auth.NewToken("osd_")
	if _, err := a.Store.CreateDevice(r.Context(), u.ID, req.DeviceName, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	a.Log.Info("device login", "user", u.Username, "device", req.DeviceName)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "username": u.Username})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	a.Store.DeleteDevice(r.Context(), c.device.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) me(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	writeJSON(w, http.StatusOK, map[string]any{"username": c.user.Username, "device": c.device.Name, "is_admin": c.user.IsAdmin})
}

type vaultJSON struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	HeadRev   int64  `json:"head_rev"`
	FileCount int64  `json:"file_count"`
}

func (a *API) userVaults(ctx context.Context, u *store.User) ([]*store.Vault, error) {
	if u.IsAdmin {
		return a.Store.ListVaults(ctx)
	}
	return a.Store.UserVaults(ctx, u.ID)
}

func (a *API) listVaults(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	vs, err := a.userVaults(r.Context(), c.user)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := []vaultJSON{}
	for _, v := range vs {
		st, _ := a.Store.VaultStats(r.Context(), v.ID)
		out = append(out, vaultJSON{v.ID, v.Name, v.Role, v.HeadRev, st.Files})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"vaults":     out,
		"can_create": c.user.IsAdmin || a.Store.Settings(r.Context()).UsersCanCreateVaults,
	})
}

func (a *API) createVault(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	if !c.user.IsAdmin && !a.Store.Settings(r.Context()).UsersCanCreateVaults {
		writeErr(w, http.StatusForbidden, "forbidden", "Only an administrator can create vaults")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request")
		return
	}
	v, err := a.Store.CreateVault(r.Context(), req.Name, store.DefaultBackupPolicy, c.user.ID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrVaultNameTaken) {
			status = http.StatusConflict
		}
		writeErr(w, status, "invalid", i18n.Err("en", err))
		return
	}
	writeJSON(w, http.StatusCreated, vaultJSON{v.ID, v.Name, store.RoleOwner, v.HeadRev, 0})
}

func (a *API) changes(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	entries, head, more, err := a.Store.Changes(r.Context(), c.vault.ID, since, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if entries == nil {
		entries = []store.FileEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": entries, "head": head, "more": more, "role": c.role})
}

func (a *API) missingBlobs(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	var req struct {
		Hashes []string `json:"hashes"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Hashes) > 10000 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request")
		return
	}
	missing := []string{}
	for _, h := range req.Hashes {
		if !blobs.ValidHash(h) {
			continue
		}
		// Content of other vaults is not disclosed: unless this vault already
		// references the hash, the client is asked to upload it.
		in, _ := a.Store.HashInVault(r.Context(), c.vault.ID, h)
		if !in || !a.Blobs.Has(h) {
			missing = append(missing, h)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

func (a *API) putBlob(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	hash := r.PathValue("hash")
	max := a.Store.Settings(r.Context()).MaxFileMB << 20
	if r.ContentLength > max {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "The file is larger than the server limit")
		return
	}
	n, err := a.Blobs.Put(hash, r.Body, max)
	switch {
	case errors.Is(err, blobs.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "The file is larger than the server limit")
	case errors.Is(err, blobs.ErrHashMismatch), errors.Is(err, blobs.ErrInvalidHash):
		writeErr(w, http.StatusBadRequest, "hash_mismatch", "Content does not match its hash")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "size": n})
	}
}

func (a *API) getBlob(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	hash := r.PathValue("hash")
	if in, err := a.Store.HashInVault(r.Context(), c.vault.ID, hash); err != nil || !in {
		writeErr(w, http.StatusNotFound, "not_found", "Content not found")
		return
	}
	f, err := a.Blobs.Open(hash)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "Content not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (a *API) commit(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	var req struct {
		Ops []store.Op `json:"ops"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Ops) == 0 || len(req.Ops) > maxOpsPerCommit {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request")
		return
	}
	author := store.Author{DeviceID: c.device.ID, Name: c.user.Username + " (" + c.device.Name + ")"}
	results, head, err := a.Store.Commit(r.Context(), c.vault.ID, req.Ops, author, a.Blobs.Has)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if head != c.vault.HeadRev {
		a.Hub.Notify(c.vault.ID, head)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "head": head})
}

func (a *API) ws(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	// Obsidian connects from app://obsidian.md or capacitor://localhost; the
	// token (not the origin) authenticates the socket.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	sub := a.Hub.Subscribe(c.vault.ID, c.device.ID)
	defer a.Hub.Unsubscribe(sub)

	ctx := conn.CloseRead(r.Context())
	send := func(rev int64) error {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		b, _ := json.Marshal(map[string]int64{"rev": rev})
		return conn.Write(wctx, websocket.MessageText, b)
	}
	if send(c.vault.HeadRev) != nil {
		return
	}
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case rev := <-sub.C:
			if send(rev) != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
