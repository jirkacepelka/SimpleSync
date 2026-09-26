# SimpleSync

Simple self-hosted sync for [Obsidian](https://obsidian.md): a small server plus an Obsidian plugin. No CouchDB, no S3 keys, no config files to tune.

In Obsidian you enter **server address, name and password**, pick a vault from the list, and you're done.

- 🖥️ **Server**: one small Docker container (Go + SQLite, ~27 MB image, tens of MB of RAM). It runs on ZimaOS, a NAS or the cheapest VPS.
- ✍️ **Web editor**: read and edit notes in the browser, no Obsidian needed. Changes reach every device within seconds.
- 🌍 **Publishing**: turn chosen notes into a public website at `/p/<name>/`.
- 🌐 **Web admin**: users, vaults, sharing, devices, version history, trash and backups. Available in English, Čeština, Slovenčina, Deutsch, Français, Español, Italiano and Polski.
- 📱 **Obsidian plugin**: desktop and mobile (iOS/Android), changes arrive within seconds. Same languages as the web admin.
- 🔀 **Safe conflicts**: concurrent edits of different parts of a note are merged. When that's not possible, nothing is lost: a copy named `… (conflict …)` is kept.
- 💾 **Vault backups**: for each vault you choose how often to back up and how long to keep backups. Restoring takes one click.

---

## Quick start

### A) ZimaOS (or CasaOS)

1. Open **App Store → "+" → Install a customized app**, switch to **YAML**, delete what's there (Ctrl+A, Delete) and paste this (also in [`deploy/zimaos/docker-compose.yml`](deploy/zimaos/docker-compose.yml)):

   ```yaml
   name: obsisync
   services:
     obsisync:
       image: ghcr.io/jirkacepelka/obsisync:latest
       container_name: obsisync
       restart: unless-stopped
       ports:
         - target: 8080
           published: "8080"
           protocol: tcp
       environment:
         TZ: Europe/Prague
       volumes:
         - type: bind
           source: /DATA/AppData/obsisync/data
           target: /data
   x-casaos:
     architectures:
       - amd64
       - arm64
     main: obsisync
     category: Utilities
     title:
       en_us: SimpleSync
     tagline:
       en_us: Simple self-hosted sync for Obsidian
     icon: https://raw.githubusercontent.com/jirkacepelka/SimpleSync/main/server/internal/web/static/icon.svg
     index: /
     port_map: "8080"
     scheme: http
   ```

2. Click **Install**, then open `http://<zimaos-ip>:8080`.
3. Create the administrator account (first-start wizard).

Data lives in `/DATA/AppData/obsisync/data`.

> **Access from outside your home network** (a phone on mobile data): the easiest option is [Tailscale](https://tailscale.com) (available in the ZimaOS App Store) or a Cloudflare Tunnel. HTTPS is recommended for iOS; Tailscale provides free HTTPS certificates (`tailscale serve`).

### B) Cheap VPS with your own domain (automatic HTTPS)

A 1 GB RAM VPS with Docker is enough.

```bash
# 1) DNS: an A record  sync.example.com → the VPS IP address
# 2) on the VPS:
mkdir obsisync && cd obsisync
curl -O https://raw.githubusercontent.com/jirkacepelka/SimpleSync/main/deploy/docker-compose.caddy.yml
DOMAIN=sync.example.com docker compose -f docker-compose.caddy.yml up -d
```

Open `https://sync.example.com` and create the administrator. Caddy obtains the Let's Encrypt certificate by itself.

### C) Anything else with Docker

```bash
docker run -d --name obsisync -p 8080:8080 -e TZ=Europe/Prague \
  -v $PWD/data:/data --restart unless-stopped ghcr.io/jirkacepelka/obsisync:latest
```

### Updating

Pull the new image and recreate the container (on ZimaOS: the app's settings → update / reinstall). Your data in `/data` is kept.

---

## Connecting Obsidian

### The easy way: download a ready-made vault

In the web admin click **Download for Obsidian** next to a vault (or **Plugin → Download ready-made vault** for an empty one). You get a ZIP with a folder that contains the vault's notes and the SimpleSync plugin, already installed and set up for your server and name (no password or token is ever put in the ZIP).

1. Unzip it, for example into Documents.
2. In Obsidian choose **Open folder as vault** and pick the unzipped folder.
3. Click **Trust author and enable plugins**.
4. Enter your password in the window that appears. The vault connects by itself and starts syncing.

### Manually: an existing vault or a phone

1. **Install the plugin.** In the web admin click **Plugin → Download plugin (ZIP)**, unzip it into `<vault>/.obsidian/plugins/` (a `simplesync` folder appears) and enable *Settings → Community plugins → SimpleSync*. On a phone, the easiest way is the **BRAT** plugin with the repository `jirkacepelka/SimpleSync`.
2. In the plugin settings enter the **server address, name and password**, then click **Log in**.
3. Pick a vault from the list and click **Connect**. Or use **Create a new vault from this one**, which uploads the current vault to the server.

The status bar icon shows the state: ✓ synced, ⟳ syncing, ⚡ server unreachable, ⚠ error. Click it to sync right away. The plugin language follows Obsidian; you can change it in the plugin settings.

### What happens when you connect

> ⚠️ **Connecting to a server vault that already has files replaces the content of this Obsidian vault.** Nothing is uploaded from this device; local files that differ are moved to Obsidian's trash (`.trash`). The plugin shows a warning in its settings and asks before connecting. **If you want to upload an existing vault, use "Create a new vault from this one" instead.**

- **The server vault has content:** it always wins. This device becomes an exact copy of the server and **nothing is uploaded** from it. Local files that differ from the server, or exist only on this device, are moved to Obsidian's trash (`.trash`), so nothing is lost. An empty or brand-new Obsidian vault can never overwrite the server.
- **The server vault is empty:** the content of this device is uploaded to it.
- If the first sync is interrupted (network drops, app closed), the next sync continues in the same "copy the server" mode.

After the first sync, both directions sync normally.

---

## Web editor

Open a vault in the web admin and click **Open notes** (or the **Notes** tab). You get a file tree, an editor and a live preview side by side, so you can write from any browser without installing Obsidian.

- **Saving is automatic**, a moment after you stop typing (or <kbd>Ctrl</kbd>+<kbd>S</kbd>). Saves use the same compare-and-swap commit as the plugin, so they show up on every device within seconds and appear in the version history as `name (web)`.
- **Edits from elsewhere** appear in the open note by themselves. If a note was changed on another device while you were typing, the changes are merged line by line; if both sides changed the same line, the other version stays and yours is kept next to it as a `… (conflict …).md` copy, exactly like on a device.
- **Obsidian syntax** in the preview: `[[wikilinks]]` (with `|alias` and `#heading`), embedded images `![[photo.png]]`, callouts `> [!tip]`, `==highlights==`, `#tags`, task lists, tables and properties (front matter).
- **Files**: create, rename and delete notes and folders from the tree (deleted files go to the vault's Trash). Paste or drop images into the editor to upload them next to the note.
- Typing `[[` suggests notes to link. The toolbar and shortcuts (<kbd>Ctrl</kbd>+<kbd>B</kbd>/<kbd>I</kbd>/<kbd>K</kbd>) cover the usual formatting; lists continue on <kbd>Enter</kbd>.
- **Roles apply**: owners and editors can edit, *read only* members get the reading view.

## Publishing

A vault owner can publish notes as a small public website: **Vault → Settings → Publishing**.

- **Address**: the site lives at `https://<your server>/p/<address>/`.
- **Which notes**: either *chosen notes* (those with `publish: true` in their properties; the **Publish** button in the editor toggles it) or *a whole folder* (empty = the whole vault).
- **Front page**: optionally a note to show at the site's root; otherwise a list of the published notes.

Only published notes are reachable. Links to notes that aren't published are shown as plain text, so their names and paths don't leak, and an image or file is served only when a published note links to or embeds it. Visitors need no account. Raw HTML in notes is never rendered, and public pages send a strict Content-Security-Policy.

## Web admin

| Section | What it does |
|---|---|
| **Overview** | vaults, disk usage, devices online, alerts about failed backups |
| **Vaults** | create a vault (name, **backup frequency**, **how long to keep backups**, members) |
| → Notes | the **web editor** (see above) |
| → Files | browse folders, preview notes and images, **version history** with restore, download the whole vault as ZIP |
| → Trash | deleted files and restoring them |
| → Backups | list of backups, **Back up now**, ZIP download, restore a single file or the **whole vault** |
| → Members | share the vault with other users: *Owner* / *Editor* / *Read only* |
| → Settings | rename, change the backup plan, **publishing**, delete the vault |
| **Users** | create accounts, reset passwords, administrators |
| **Devices** | every Obsidian login; logging a device out removes its access immediately |
| **Settings** | how long to keep version history, maximum file size, whether users may create vaults |

The language picker is at the bottom of every page.

### Backups

When creating a vault, the administrator chooses:

- **Backup frequency:** off / every hour / every 6 hours / daily / weekly
- **Keep backups for:** 7 days / 30 days / 90 days / 1 year / forever
- optionally **also save as ZIP**: every backup is additionally written as a standalone ZIP file to `data/backups/`, handy for copying to another disk

How it works:
- A backup is a snapshot of the whole vault. Identical file content is stored only once on disk, so backups take almost no extra space.
- If the vault hasn't changed since the last backup, the scheduled backup is skipped.
- Old backups are removed according to the retention setting; **the newest backup is never deleted**.
- **Restoring the whole vault** first saves the current state as a "Before restore" backup, then sends the changes to every device like a normal sync.

### Backing up the whole server

Everything (database, file contents, ZIP backups) is in the `data/` folder. Back up that folder. For a consistent copy of the database while running:

```bash
docker exec obsisync obsisync backup-db /data/obsisync-backup.db
```

### Forgotten administrator password

```bash
docker exec obsisync obsisync reset-password admin NewPassword123
```

(If the user doesn't exist, it is created as an administrator.)

---

## Network use and privacy

The SimpleSync plugin communicates **only with the SimpleSync server whose address you enter**, a server you run yourself. It sends your login once to obtain a device token (the password is not stored), then uploads and downloads the files of the connected vault. To do that it lists all files in the vault and compares them with the server; nothing is sent anywhere else. There is no telemetry, no third-party service and no account with anyone else. Content is protected in transit by HTTPS when the server is reachable over HTTPS; it is not end-to-end encrypted, so whoever runs the server can read the notes stored on it.

## Security

- **Every check happens on the server.** The plugin only holds a random device token (never the password); the server verifies it and the user's role in the vault on every request. Tokens are stored only as SHA-256 hashes. A device can be logged out in the web admin at any time.
- **Passwords** are hashed with Argon2id. At most a few password checks run at once, so login floods cannot exhaust memory, and unknown names take as long as wrong passwords.
- **Login throttling** (web admin and plugin): 10 failures per IP address and name, 30 per IP address, and 30 per account from any address, all within 15 minutes. The per-account limit cannot be bypassed with many IP addresses or a forged `X-Forwarded-For`. Devices and browsers that are already logged in keep working while an account is throttled.
- **Web admin**: session cookies are `HttpOnly` and `SameSite=Lax`, every form has a CSRF token, pages send a strict Content-Security-Policy and may not be framed, and files from vaults are only ever downloaded (never rendered as HTML/SVG).
- **Paths** from devices are validated on the server and again in the plugin, so nothing can be written outside the vault. Files in `.obsidian` are only synced when you turn that on; only do so in vaults shared with people you trust, because synced plugins run on every device.
- **Transport**: use HTTPS when the server is reachable from the internet (Caddy, Tailscale or a Cloudflare Tunnel). The plugin warns before sending a password over plain `http://` outside your home network.
- **First start**: until the administrator account exists, whoever opens the web admin first can create it. Create it right after installing.
- **Reverse proxies**: the client address is taken from `X-Forwarded-For` only when the connection comes from a local or private address, and then only the entry added by the nearest proxy.

---

## Architecture

```
Obsidian (desktop / mobile)                 Server (1 Docker container)
┌───────────────────────┐   HTTPS (REST)    ┌────────────────────────────────┐
│ SimpleSync plugin    │◄────────────────►│ SimpleSync server (Go)        │
│  • 3 fields + picker  │   WebSocket       │  • /api/v1  sync               │
│  • sync engine        │◄──────────────────│  • /        web admin          │
│  • 3-way merge        │  ("new revision") │  • SQLite   metadata, history  │
└───────────────────────┘                   │  • blobs/   content (SHA-256)  │
                                            │  • backup scheduler + upkeep   │
                                            └────────────────────────────────┘
```

**How sync works**

- File contents are stored by their SHA-256 hash (content-addressed). This gives deduplication and history, and renames need no re-upload.
- Every vault has a **growing revision number**. A device remembers the last revision it saw and fetches only changes after it.
- For every file, each device remembers the "base" hash: the version it last agreed on with the server. Writes to the server are **compare-and-swap**: a write only succeeds if the server still has that base version. Otherwise it's a conflict:
  - text files: **3-way merge** (changes on different lines are combined),
  - overlapping changes and binary files: the server version wins and the local one is saved as `Note (conflict 2026-09-25 1530 jirka).md` (with the name of the user whose edit it is),
  - edit vs. delete: the edit wins.
- Over WebSocket the server only says "the vault has a new revision"; devices then fetch the changes. As a fallback the plugin syncs every minute, 2 s after a file changes, and when the app comes back to the foreground.
- Paths are normalized to Unicode NFC (macOS/iOS vs. Windows vs. Linux). Paths that differ only in letter case are rejected, because they would collide on Windows/macOS.
- Safety net: if a sync would delete more than half of the files on the server (for example, the vault didn't load), the plugin asks first.
- Not synced: `.trash/`, `.git/`, other hidden folders, `workspace.json` and the plugin's own data (token). The `.obsidian` folder can be enabled with a toggle.

**API**: everything is under `/api/v1`, authorized with `Authorization: Bearer <device token>`:

| Endpoint | Purpose |
|---|---|
| `POST /auth/login` | name + password → device token (the password is not stored on the device) |
| `GET /vaults`, `POST /vaults` | the user's vaults / create one |
| `GET /vaults/{id}/changes?since=REV` | changes since a revision |
| `POST /vaults/{id}/blobs/missing` | which content the server doesn't have yet |
| `PUT` / `GET /vaults/{id}/blobs/{sha256}` | upload / download content |
| `POST /vaults/{id}/commit` | a batch of changes (compare-and-swap) |
| `GET /vaults/{id}/ws` | WebSocket notifications |

### Repository layout

```
server/     Go server (cmd/obsisync, internal/{store,api,web,backup,blobs,auth,hub,i18n,markdown,textmerge})
plugin/     Obsidian plugin (TypeScript); src/engine is Obsidian-independent and tested
deploy/     docker-compose for home network, VPS with Caddy, and ZimaOS
Dockerfile  multi-arch image (amd64 + arm64) with the server and the plugin
```

### Development

```bash
# server
cd server && go test ./... && go run ./cmd/obsisync    # http://localhost:8080, data in ./data

# plugin (tests start a real server and simulate several devices)
cd plugin && npm ci && npm test && npm run build       # output in plugin/dist
```

Translations: the web admin uses `server/internal/i18n/locales/<lang>.json`, the plugin uses `plugin/src/i18n/<lang>.ts`. English is the source; missing keys fall back to English.

Releases: every push to `main` makes GitHub Actions build the Docker image `ghcr.io/jirkacepelka/obsisync` (`latest` and the version from `manifest.json`) and a GitHub release with the plugin files. To publish a new version, bump `version` in `manifest.json`, `plugin/manifest.json` and `versions.json`.

### Environment variables (optional)

| Variable | Default | Meaning |
|---|---|---|
| `TZ` | UTC | time zone for displayed times (e.g. `Europe/Prague`) |
| `OBSISYNC_DATA` | `/data` | data folder |
| `OBSISYNC_ADDR` | `:8080` | listen address and port |
| `OBSISYNC_BACKUP_DIR` | `$OBSISYNC_DATA/backups` | where ZIP backups go |

## License

[MIT](LICENSE)
