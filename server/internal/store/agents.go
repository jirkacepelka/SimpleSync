package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Tables for AI agent access over MCP: API tokens (made in the web admin or
// issued through OAuth), OAuth clients that registered themselves and the
// short-lived authorization codes handed to them.
const tokenSchema = `
CREATE TABLE IF NOT EXISTS api_tokens (
	id         INTEGER PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	vault_id   INTEGER REFERENCES vaults(id) ON DELETE CASCADE, -- NULL = every vault the user can open
	can_write  INTEGER NOT NULL DEFAULT 0,
	client_id  TEXT NOT NULL DEFAULT '', -- OAuth client it was issued to, '' when made by hand
	created_at INTEGER NOT NULL,
	last_used  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS oauth_clients (
	client_id     TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	redirect_uris TEXT NOT NULL, -- JSON array
	used          INTEGER NOT NULL DEFAULT 0, -- got a token once; kept for good
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_codes (
	code_hash    TEXT PRIMARY KEY,
	client_id    TEXT NOT NULL,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	redirect_uri TEXT NOT NULL,
	challenge    TEXT NOT NULL,
	vault_id     INTEGER REFERENCES vaults(id) ON DELETE CASCADE,
	can_write    INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL
);
`

// APIToken gives an AI agent access to a user's vaults: all of them or one,
// read-only or read-write. It never grants more than the user's own role.
type APIToken struct {
	ID        int64
	UserID    int64
	Username  string
	Name      string
	VaultID   int64 // 0 = every vault the user can open
	VaultName string
	CanWrite  bool
	ClientID  string
	CreatedAt time.Time
	LastUsed  time.Time // zero when never used
}

func cleanName(name, def string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = def
	}
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}
	return name
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name, tokenHash string, vaultID int64, canWrite bool, clientID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens(user_id, name, token_hash, vault_id, can_write, client_id, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, userID, cleanName(name, "AI agent"), tokenHash, nullID(vaultID), boolInt(canWrite), clientID, s.now())
	if err != nil {
		return 0, err
	}
	if clientID != "" {
		s.db.ExecContext(ctx, "UPDATE oauth_clients SET used = 1 WHERE client_id = ?", clientID)
	}
	return res.LastInsertId()
}

const tokenCols = `t.id, t.user_id, u.username, t.name, COALESCE(t.vault_id, 0), COALESCE(v.name, ''), t.can_write, t.client_id, t.created_at, t.last_used
	FROM api_tokens t JOIN users u ON u.id = t.user_id LEFT JOIN vaults v ON v.id = t.vault_id`

func scanToken(row interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var created, used int64
	if err := row.Scan(&t.ID, &t.UserID, &t.Username, &t.Name, &t.VaultID, &t.VaultName, &t.CanWrite, &t.ClientID, &created, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.CreatedAt = time.Unix(created, 0)
	if used > 0 {
		t.LastUsed = time.Unix(used, 0)
	}
	return &t, nil
}

// APITokenByHash resolves a token to itself and its user and records the
// use (at most once a minute to avoid needless writes).
func (s *Store) APITokenByHash(ctx context.Context, tokenHash string) (*APIToken, *User, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, "SELECT "+tokenCols+" WHERE t.token_hash = ?", tokenHash))
	if err != nil {
		return nil, nil, err
	}
	u, err := s.UserByID(ctx, t.UserID)
	if err != nil {
		return nil, nil, err
	}
	if now := s.now(); now-t.LastUsed.Unix() > 60 {
		s.db.ExecContext(ctx, "UPDATE api_tokens SET last_used = ? WHERE id = ?", now, t.ID)
	}
	return t, u, nil
}

func (s *Store) APIToken(ctx context.Context, id int64) (*APIToken, error) {
	return scanToken(s.db.QueryRowContext(ctx, "SELECT "+tokenCols+" WHERE t.id = ?", id))
}

// ListAPITokens lists tokens of one user, or of everybody when userID is 0.
func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]*APIToken, error) {
	q, args := "SELECT "+tokenCols, []any{}
	if userID != 0 {
		q += " WHERE t.user_id = ?"
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY t.created_at DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM api_tokens WHERE id = ?", id)
	return err
}

// ---- OAuth ----

type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// maxOAuthClients bounds the table: anyone may register a client.
const maxOAuthClients = 1000

var ErrTooManyClients = errors.New("too many OAuth clients")

// CreateOAuthClient stores a dynamically registered client. Clients that
// registered over a day ago and never got a token are dropped first (a
// client that did is kept even after its tokens are revoked, since it
// remembers its id to connect again).
func (s *Store) CreateOAuthClient(ctx context.Context, c *OAuthClient) error {
	uris, _ := json.Marshal(c.RedirectURIs)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM oauth_clients WHERE created_at < ? AND used = 0`,
			s.now()-86400); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM oauth_clients").Scan(&n); err != nil {
			return err
		}
		if n >= maxOAuthClients {
			return ErrTooManyClients
		}
		_, err := tx.Exec("INSERT INTO oauth_clients(client_id, name, redirect_uris, created_at) VALUES(?, ?, ?, ?)",
			c.ID, cleanName(c.Name, "MCP client"), string(uris), s.now())
		return err
	})
}

func (s *Store) OAuthClient(ctx context.Context, id string) (*OAuthClient, error) {
	var c OAuthClient
	var uris string
	var created int64
	err := s.db.QueryRowContext(ctx, "SELECT client_id, name, redirect_uris, created_at FROM oauth_clients WHERE client_id = ?", id).
		Scan(&c.ID, &c.Name, &uris, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(uris), &c.RedirectURIs)
	c.CreatedAt = time.Unix(created, 0)
	return &c, nil
}

// OAuthCode is what the user approved on the consent page.
type OAuthCode struct {
	ClientID    string
	UserID      int64
	RedirectURI string
	Challenge   string // PKCE S256 code challenge
	VaultID     int64
	CanWrite    bool
}

func (s *Store) CreateOAuthCode(ctx context.Context, codeHash string, c OAuthCode, ttl time.Duration) error {
	s.db.ExecContext(ctx, "DELETE FROM oauth_codes WHERE expires_at < ?", s.now())
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_codes(code_hash, client_id, user_id, redirect_uri, challenge, vault_id, can_write, expires_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, codeHash, c.ClientID, c.UserID, c.RedirectURI, c.Challenge, nullID(c.VaultID), boolInt(c.CanWrite), s.Now().Add(ttl).Unix())
	return err
}

// TakeOAuthCode returns an unexpired code and deletes it: a code works once.
func (s *Store) TakeOAuthCode(ctx context.Context, codeHash string) (*OAuthCode, error) {
	var c OAuthCode
	var expires int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRow(`SELECT client_id, user_id, redirect_uri, challenge, COALESCE(vault_id, 0), can_write, expires_at
			FROM oauth_codes WHERE code_hash = ?`, codeHash).
			Scan(&c.ClientID, &c.UserID, &c.RedirectURI, &c.Challenge, &c.VaultID, &c.CanWrite, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM oauth_codes WHERE code_hash = ?", codeHash)
		return err
	})
	if err != nil {
		return nil, err
	}
	if expires < s.now() {
		return nil, ErrNotFound
	}
	return &c, nil
}

// RecentVersions returns the latest changes in a vault, newest first.
func (s *Store) RecentVersions(ctx context.Context, vaultID int64, limit int) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, "+fileCols+", author, created_at FROM file_versions WHERE vault_id = ? ORDER BY rev DESC LIMIT ?", vaultID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var created int64
		if err := rows.Scan(&v.ID, &v.Path, &v.Hash, &v.Size, &v.Mtime, &v.Deleted, &v.Rev, &v.Author, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = time.Unix(created, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}
