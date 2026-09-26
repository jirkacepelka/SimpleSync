package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"github.com/jirkacepelka/obsisync/server/internal/pathutil"
)

// Publishing modes: which notes of a vault are public.
const (
	PublishMarked = "marked" // notes with "publish: true" in their properties
	PublishFolder = "folder" // every note inside Folder ("" = the whole vault)
)

// Publish is the public-site configuration of a vault.
type Publish struct {
	VaultID int64
	Enabled bool
	Slug    string // the site lives at /p/<slug>/
	Title   string
	Mode    string
	Folder  string
	Home    string // path of the note shown on the site's front page
}

var (
	ErrInvalidSlug = errors.New("err.invalidSlug")
	ErrSlugTaken   = errors.New("err.slugTaken")
	ErrInvalidPath = errors.New("err.invalidPath")
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$`)

// ValidSlug reports whether s can be used in a public site address.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

const publishCols = "vault_id, enabled, slug, title, mode, folder, home"

func scanPublish(row interface{ Scan(...any) error }) (Publish, error) {
	var p Publish
	err := row.Scan(&p.VaultID, &p.Enabled, &p.Slug, &p.Title, &p.Mode, &p.Folder, &p.Home)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// PublishOf returns the vault's publishing settings; a vault that was never
// configured gets disabled defaults.
func (s *Store) PublishOf(ctx context.Context, vaultID int64) (Publish, error) {
	p, err := scanPublish(s.db.QueryRowContext(ctx, "SELECT "+publishCols+" FROM vault_publish WHERE vault_id = ?", vaultID))
	if errors.Is(err, ErrNotFound) {
		return Publish{VaultID: vaultID, Mode: PublishMarked}, nil
	}
	return p, err
}

// PublishBySlug finds an enabled site by its address.
func (s *Store) PublishBySlug(ctx context.Context, slug string) (Publish, error) {
	return scanPublish(s.db.QueryRowContext(ctx, "SELECT "+publishCols+" FROM vault_publish WHERE slug = ? AND enabled = 1", strings.ToLower(slug)))
}

// SavePublish validates and stores the settings.
func (s *Store) SavePublish(ctx context.Context, p Publish) error {
	p.Slug = strings.ToLower(strings.TrimSpace(p.Slug))
	if !ValidSlug(p.Slug) {
		return ErrInvalidSlug
	}
	if p.Mode != PublishMarked && p.Mode != PublishFolder {
		p.Mode = PublishMarked
	}
	p.Title = strings.TrimSpace(p.Title)
	if len(p.Title) > 120 {
		p.Title = p.Title[:120]
	}
	for _, f := range []*string{&p.Folder, &p.Home} {
		*f = strings.Trim(strings.TrimSpace(*f), "/")
		if *f == "" {
			continue
		}
		n, err := pathutil.Normalize(*f)
		if err != nil {
			return ErrInvalidPath
		}
		*f = n
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO vault_publish(`+publishCols+`) VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(vault_id) DO UPDATE SET enabled = excluded.enabled, slug = excluded.slug, title = excluded.title,
		mode = excluded.mode, folder = excluded.folder, home = excluded.home`,
		p.VaultID, boolInt(p.Enabled), p.Slug, p.Title, p.Mode, p.Folder, p.Home)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrSlugTaken
	}
	return err
}
