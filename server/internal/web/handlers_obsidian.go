package web

import (
	"archive/zip"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// Ready-to-open vaults: a ZIP with a folder that Obsidian opens as a vault,
// with the SimpleSync plugin installed, enabled and pre-configured (server
// address, name, vault). The user only types the password. The ZIP never
// contains a token or password.

const pluginID = "simplesync"

// pluginPreset is the plugin's data.json; field names match the plugin's
// Settings.
type pluginPreset struct {
	ServerURL        string `json:"serverUrl"`
	Username         string `json:"username"`
	LoginPrompt      bool   `json:"loginPrompt"` // ask for the password on first start
	PendingVaultID   *int64 `json:"pendingVaultId"`
	PendingVaultName string `json:"pendingVaultName"`
}

func serverURL(r *http.Request) string {
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// folderName turns a vault name into a folder name that works on Windows,
// macOS and Linux.
func folderName(name string) string {
	name = strings.Map(func(c rune) rune {
		if c < 32 || strings.ContainsRune(`<>:"/\|?*`, c) {
			return '_'
		}
		return c
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		return "Vault"
	}
	return name
}

// obsidianVaultZip downloads a vault with its notes and the plugin.
func (w *Web) obsidianVaultZip(rw http.ResponseWriter, r *http.Request, p *page) {
	if !w.pluginAvailable() {
		http.NotFound(rw, r)
		return
	}
	files, err := p.Scope.ListFiles(r.Context(), false)
	if err != nil {
		w.fail(rw, r, 500, err.Error())
		return
	}
	id := p.Vault.ID
	preset := pluginPreset{ServerURL: serverURL(r), Username: p.User.Username, LoginPrompt: true, PendingVaultID: &id, PendingVaultName: p.Vault.Name}
	w.writeObsidianZip(rw, r, folderName(p.Vault.Name), preset, files)
}

// obsidianStarterZip downloads an empty vault with the plugin; after
// logging in the user picks a vault.
func (w *Web) obsidianStarterZip(rw http.ResponseWriter, r *http.Request, p *page) {
	if !w.pluginAvailable() {
		http.NotFound(rw, r)
		return
	}
	w.writeObsidianZip(rw, r, "SimpleSync", pluginPreset{ServerURL: serverURL(r), Username: p.User.Username, LoginPrompt: true}, nil)
}

func (w *Web) writeObsidianZip(rw http.ResponseWriter, r *http.Request, folder string, preset pluginPreset, files []store.FileEntry) {
	cfgDir := ".obsidian/"
	pluginDir := cfgDir + "plugins/" + pluginID + "/"
	enabled := []string{pluginID}
	for _, f := range files {
		// A vault that syncs its settings has its own list of plugins.
		if f.Path == cfgDir+"community-plugins.json" && !f.Deleted {
			if b, err := w.readBlob(f.Hash); err == nil {
				var list []string
				if json.Unmarshal(b, &list) == nil {
					for _, id := range list {
						if id != pluginID {
							enabled = append(enabled, id)
						}
					}
				}
			}
		}
	}

	rw.Header().Set("Content-Type", "application/zip")
	rw.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": folder + ".zip"}))
	zw := zip.NewWriter(rw)
	prefix := folder + "/"
	add := func(name string, data []byte) error {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: prefix + name, Method: zip.Deflate, Modified: time.Now()})
		if err == nil {
			_, err = fw.Write(data)
		}
		return err
	}
	fail := func(err error) {
		w.Log.Error("obsidian zip", "folder", folder, "err", err)
	}

	skip := func(path string) bool {
		return path == cfgDir+"community-plugins.json" || strings.HasPrefix(path, pluginDir)
	}
	if err := w.Backup.AddToZip(zw, prefix, files, skip); err != nil {
		fail(err)
		return
	}
	for _, name := range pluginFiles {
		data, err := os.ReadFile(filepath.Join(w.PluginDir, name))
		if err == nil {
			err = add(pluginDir+name, data)
		}
		if err != nil {
			fail(err)
			return
		}
	}
	presetJSON, _ := json.MarshalIndent(preset, "", "  ")
	enabledJSON, _ := json.Marshal(enabled)
	for name, data := range map[string][]byte{pluginDir + "data.json": presetJSON, cfgDir + "community-plugins.json": enabledJSON} {
		if err := add(name, data); err != nil {
			fail(err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		fail(err)
	}
}

func (w *Web) readBlob(hash string) ([]byte, error) {
	f, err := w.Blobs.Open(hash)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}
