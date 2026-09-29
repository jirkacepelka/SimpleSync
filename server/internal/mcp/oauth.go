package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jirkacepelka/obsisync/server/internal/auth"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// OAuth 2.1 for MCP clients such as Claude's custom connectors:
// dynamic client registration, authorization code with PKCE (S256), and
// access tokens that are ordinary API tokens (listed and revocable on the
// AI agents page). The consent page is served by the web admin at
// AuthorizePath, since it needs the browser session.

const AuthorizePath = "/oauth/authorize"

func (s *Server) resourceMetadata(rw http.ResponseWriter, r *http.Request) {
	if cors(rw, r) {
		return
	}
	base := BaseURL(r)
	writeJSON(rw, http.StatusOK, map[string]any{
		"resource":                 base + Path,
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "SimpleSync",
	})
}

func (s *Server) authServerMetadata(rw http.ResponseWriter, r *http.Request) {
	if cors(rw, r) {
		return
	}
	base := BaseURL(r)
	writeJSON(rw, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + AuthorizePath,
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"notes"},
	})
}

func oauthErr(rw http.ResponseWriter, status int, code, desc string) {
	writeJSON(rw, status, map[string]string{"error": code, "error_description": desc})
}

// ValidRedirectURI accepts https addresses and http on this computer only.
func ValidRedirectURI(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Fragment != "" || u.Host == "" || len(s) > 2000 {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

func (s *Server) register(rw http.ResponseWriter, r *http.Request) {
	if cors(rw, r) {
		return
	}
	if r.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		oauthErr(rw, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthErr(rw, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !ValidRedirectURI(u) {
			oauthErr(rw, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must use https (or http on localhost)")
			return
		}
	}
	id, _ := auth.NewToken("osc_")
	c := &store.OAuthClient{ID: id, Name: req.ClientName, RedirectURIs: req.RedirectURIs}
	if err := s.Store.CreateOAuthClient(r.Context(), c); err != nil {
		if errors.Is(err, store.ErrTooManyClients) {
			oauthErr(rw, http.StatusServiceUnavailable, "temporarily_unavailable", "too many clients registered, try again tomorrow")
			return
		}
		oauthErr(rw, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(rw, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_name":                req.ClientName,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// PKCEChallenge is the S256 challenge for a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Server) token(rw http.ResponseWriter, r *http.Request) {
	if cors(rw, r) {
		return
	}
	if r.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(rw, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if gt := r.PostForm.Get("grant_type"); gt != "authorization_code" {
		oauthErr(rw, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}
	clientID := r.PostForm.Get("client_id")
	if id, _, ok := r.BasicAuth(); ok && clientID == "" {
		clientID = id
	}
	code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")
	if code == "" || verifier == "" {
		oauthErr(rw, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}
	c, err := s.Store.TakeOAuthCode(r.Context(), auth.HashToken(code))
	if err != nil {
		oauthErr(rw, http.StatusBadRequest, "invalid_grant", "unknown or expired code")
		return
	}
	if ru := r.PostForm.Get("redirect_uri"); c.ClientID != clientID || (ru != "" && ru != c.RedirectURI) ||
		subtle.ConstantTimeCompare([]byte(PKCEChallenge(verifier)), []byte(c.Challenge)) != 1 {
		oauthErr(rw, http.StatusBadRequest, "invalid_grant", "code does not match the client, redirect URI or verifier")
		return
	}
	client, err := s.Store.OAuthClient(r.Context(), c.ClientID)
	if err != nil {
		oauthErr(rw, http.StatusBadRequest, "invalid_client", "unknown client")
		return
	}
	tok, hash := auth.NewToken("osa_")
	if _, err := s.Store.CreateAPIToken(r.Context(), c.UserID, client.Name, hash, c.VaultID, c.CanWrite, client.ID); err != nil {
		oauthErr(rw, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if s.Log != nil {
		s.Log.Info("mcp token issued", "client", client.Name, "user_id", c.UserID, "write", c.CanWrite)
	}
	writeJSON(rw, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "scope": "notes"})
}

// AuthorizeRequest is a validated request to the consent page.
type AuthorizeRequest struct {
	Client      *store.OAuthClient
	RedirectURI string
	State       string
	Challenge   string
}

// ErrBadAuthorize means the request cannot even be sent back to the client.
var ErrBadAuthorize = errors.New("invalid authorization request")

// ParseAuthorize checks the query of an authorization request. When the
// client or redirect URI is wrong it returns ErrBadAuthorize (show an error,
// never redirect); other problems yield an OAuth error to redirect with.
func ParseAuthorize(r *http.Request, st *store.Store, q url.Values) (*AuthorizeRequest, string, error) {
	client, err := st.OAuthClient(r.Context(), q.Get("client_id"))
	if err != nil {
		return nil, "", ErrBadAuthorize
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(client.RedirectURIs) == 1 {
		redirect = client.RedirectURIs[0]
	}
	found := false
	for _, u := range client.RedirectURIs {
		found = found || u == redirect
	}
	if !found {
		return nil, "", ErrBadAuthorize
	}
	ar := &AuthorizeRequest{Client: client, RedirectURI: redirect, State: q.Get("state"), Challenge: q.Get("code_challenge")}
	if q.Get("response_type") != "code" {
		return ar, "unsupported_response_type", nil
	}
	if ar.Challenge == "" || q.Get("code_challenge_method") != "S256" {
		return ar, "invalid_request", nil
	}
	return ar, "", nil
}

// RedirectWith builds the address to send the browser back to the client.
func (ar *AuthorizeRequest) RedirectWith(params url.Values) string {
	if ar.State != "" {
		params.Set("state", ar.State)
	}
	sep := "?"
	if strings.Contains(ar.RedirectURI, "?") {
		sep = "&"
	}
	return ar.RedirectURI + sep + params.Encode()
}
