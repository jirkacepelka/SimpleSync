// Package app wires all server components together.
package app

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jirkacepelka/obsisync/server/internal/api"
	"github.com/jirkacepelka/obsisync/server/internal/auth"
	"github.com/jirkacepelka/obsisync/server/internal/backup"
	"github.com/jirkacepelka/obsisync/server/internal/blobs"
	"github.com/jirkacepelka/obsisync/server/internal/hub"
	"github.com/jirkacepelka/obsisync/server/internal/mcp"
	"github.com/jirkacepelka/obsisync/server/internal/store"
	"github.com/jirkacepelka/obsisync/server/internal/web"
)

type Config struct {
	DataDir   string
	BackupDir string // default <DataDir>/backups
	PluginDir string
	Version   string
	Log       *slog.Logger
}

type App struct {
	Handler http.Handler
	Store   *store.Store
	Blobs   *blobs.Store
	Hub     *hub.Hub
	Backup  *backup.Service
}

func New(cfg Config) (*App, error) {
	if cfg.BackupDir == "" {
		cfg.BackupDir = filepath.Join(cfg.DataDir, "backups")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "obsisync.db"))
	if err != nil {
		return nil, err
	}
	bl, err := blobs.New(filepath.Join(cfg.DataDir, "blobs"))
	if err != nil {
		st.Close()
		return nil, err
	}
	h := hub.New()
	guard := auth.NewLoginGuard()
	bk := &backup.Service{Store: st, Blobs: bl, Hub: h, Dir: cfg.BackupDir, Now: time.Now, Log: cfg.Log}

	mux := http.NewServeMux()
	(&api.API{Store: st, Blobs: bl, Hub: h, Guard: guard, Version: cfg.Version, Log: cfg.Log}).Register(mux)
	(&mcp.Server{Store: st, Blobs: bl, Hub: h, Version: cfg.Version, Log: cfg.Log}).Register(mux)
	w := &web.Web{Store: st, Blobs: bl, Hub: h, Backup: bk, Guard: guard, Version: cfg.Version, Log: cfg.Log, PluginDir: cfg.PluginDir}
	if err := w.Register(mux); err != nil {
		st.Close()
		return nil, err
	}
	return &App{Handler: mux, Store: st, Blobs: bl, Hub: h, Backup: bk}, nil
}

func (a *App) Close() error { return a.Store.Close() }
