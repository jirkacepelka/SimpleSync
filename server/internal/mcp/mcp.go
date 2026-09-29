// Package mcp lets AI agents work with vaults over the Model Context
// Protocol (Streamable HTTP transport, stateless, JSON responses).
//
// Agents authenticate with an API token: made by hand in the web admin
// (AI agents page) or issued through OAuth, which is how Claude's custom
// connectors sign in. Changes go through the same commit path as the
// Obsidian plugin, so they are versioned and reach devices within seconds.
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jirkacepelka/obsisync/server/internal/auth"
	"github.com/jirkacepelka/obsisync/server/internal/blobs"
	"github.com/jirkacepelka/obsisync/server/internal/hub"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// Path is where the MCP endpoint is served.
const Path = "/mcp"

type Server struct {
	Store   *store.Store
	Blobs   *blobs.Store
	Hub     *hub.Hub
	Version string
	Log     *slog.Logger
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc(Path, s.handle)
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.resourceMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource"+Path, s.resourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.authServerMetadata)
	mux.HandleFunc("/oauth/register", s.register)
	mux.HandleFunc("/oauth/token", s.token)
}

// BaseURL is the address clients reach this server at.
func BaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// cors lets browser-based MCP clients call the endpoints. Nothing here
// uses cookies, so any origin is fine.
func cors(rw http.ResponseWriter, r *http.Request) bool {
	h := rw.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, Mcp-Protocol-Version")
	h.Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	if r.Method == http.MethodOptions {
		rw.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}

// ---- authentication ----

// caller is who is calling and what the token allows.
type caller struct {
	user  *store.User
	token *store.APIToken
}

func (s *Server) authenticate(r *http.Request) *caller {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil
	}
	t, u, err := s.Store.APITokenByHash(r.Context(), auth.HashToken(strings.TrimPrefix(h, "Bearer ")))
	if err != nil {
		return nil
	}
	return &caller{user: u, token: t}
}

// unauthorized points the client at the OAuth metadata so it can sign in.
func (s *Server) unauthorized(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+BaseURL(r)+`/.well-known/oauth-protected-resource"`)
	writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "invalid_token", "error_description": "Missing or revoked token"})
}

// ---- JSON-RPC ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
)

// Protocol versions this server speaks, newest first.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const instructions = `This server holds Obsidian vaults that sync to the user's devices.
Paths are relative to the vault root and use forward slashes, e.g. "Projects/Plan.md".
Notes are Markdown; Obsidian links look like [[Note name]].
When you have access to one vault only, the vault argument can be left out.
Every change is kept in the file's history and appears on all devices within seconds; deleted notes go to the vault's trash.`

func (s *Server) handle(rw http.ResponseWriter, r *http.Request) {
	if cors(rw, r) {
		return
	}
	c := s.authenticate(r)
	if c == nil {
		s.unauthorized(rw, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		// Stateless: there is no session to end.
		rw.WriteHeader(http.StatusNoContent)
		return
	default:
		// No server-initiated messages, so no event stream.
		rw.Header().Set("Allow", "POST, DELETE, OPTIONS")
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(rw, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "cannot read request"}})
		return
	}
	body = []byte(strings.TrimSpace(string(body)))
	// Older clients may send a batch.
	if len(body) > 0 && body[0] == '[' {
		var reqs []rpcRequest
		if err := json.Unmarshal(body, &reqs); err != nil || len(reqs) == 0 || len(reqs) > 50 {
			writeJSON(rw, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "invalid batch"}})
			return
		}
		var out []rpcResponse
		for i := range reqs {
			if res := s.dispatch(r.Context(), c, &reqs[i]); res != nil {
				out = append(out, *res)
			}
		}
		if len(out) == 0 {
			rw.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(rw, http.StatusOK, out)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(rw, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "invalid JSON"}})
		return
	}
	res := s.dispatch(r.Context(), c, &req)
	if res == nil {
		// A notification or a response: nothing to answer.
		rw.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(rw, http.StatusOK, res)
}

// dispatch handles one message; it returns nil for notifications.
func (s *Server) dispatch(ctx context.Context, c *caller, req *rpcRequest) *rpcResponse {
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil
	}
	res := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	fail := func(code int, msg string) *rpcResponse {
		res.Error = &rpcError{code, msg}
		return res
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return fail(codeInvalidRequest, "not a JSON-RPC 2.0 request")
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		for _, v := range protocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		res.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "obsisync", "title": "SimpleSync", "version": s.Version},
			"instructions":    instructions,
		}
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		res.Result = map[string]any{"tools": toolList(c.token.CanWrite)}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(codeInvalidParams, "invalid params")
		}
		t := findTool(p.Name, c.token.CanWrite)
		if t == nil {
			return fail(codeInvalidParams, "unknown tool: "+p.Name)
		}
		args := map[string]any{}
		if len(p.Arguments) > 0 && string(p.Arguments) != "null" {
			if err := json.Unmarshal(p.Arguments, &args); err != nil {
				return fail(codeInvalidParams, "arguments must be an object")
			}
		}
		text, err := t.run(ctx, s, c, args)
		if err != nil {
			res.Result = toolResult(err.Error(), true)
		} else {
			res.Result = toolResult(text, false)
		}
		if s.Log != nil {
			s.Log.Info("mcp tool", "user", c.user.Username, "token", c.token.Name, "tool", p.Name, "error", err != nil)
		}
	case "resources/list":
		res.Result = map[string]any{"resources": []any{}}
	case "prompts/list":
		res.Result = map[string]any{"prompts": []any{}}
	default:
		return fail(codeNoMethod, "method not found: "+req.Method)
	}
	return res
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isError}
}
