# Changelog

Each `## <version>` section below becomes the description of that GitHub release.

## 0.4.0

- **Web editor**: open a vault's notes in the browser and edit them without Obsidian. File tree with search, edit / side-by-side / reading modes, automatic saving, `[[` link suggestions, toolbar and shortcuts, image paste and drop, create, rename and delete of notes and folders. Saves go through the same sync as the plugin, reach devices within seconds and merge with concurrent edits (or keep a conflict copy). Read-only members get the reading view.
- **Publishing**: vault owners can publish chosen notes (`publish: true`) or a whole folder as a public website at `/p/<address>/`, with a front page, navigation, and Obsidian-style rendering (wikilinks, embeds, callouts, highlights, tags). Unpublished notes and files stay private, including their names.
- **New look** for the web admin, matching the website: floating navigation, segmented tabs, cleaner tables, icons instead of emoji, dark and light theme.

## 0.3.5

- **Download for Obsidian**: every vault in the web admin can be downloaded as a ready-to-open Obsidian vault with its notes and the SimpleSync plugin already installed, enabled and set up (server address, name, vault). Open the folder in Obsidian, trust it, enter your password, and it connects by itself. The Plugin page offers the same as an empty starter vault. The ZIP never contains a password or token.
- Conflict copies are named after the user who made the edit, e.g. `Note (conflict 2026-09-25 1530 jirka).md`, instead of the device.

## 0.3.4

Security hardening after a review:

- Login throttling can no longer be bypassed: besides the per-address limit there is a per-account limit (30 failed attempts in 15 minutes from any address), and a forged `X-Forwarded-For` header is ignored.
- Unknown names and wrong passwords take the same time; at most a few password checks run at once.
- Failed logins are logged with the name and address.
- The web admin sends a strict Content-Security-Policy (no inline scripts).
- The plugin rejects server paths that would leave the vault, and warns before logging in over plain http:// outside the home network.
- The plugin settings explain up front that connecting to a server vault with files replaces this vault's content (nothing is lost: local files go to Obsidian's trash).

## 0.3.3

- Plugin settings use Obsidian's declarative settings API (Obsidian 1.13+), so they show up in the settings search. Older Obsidian versions keep the same settings page.
- Stricter typing of server responses in the plugin (no `any`).
- Releases now contain only `main.js`, `manifest.json` and `styles.css`, with GitHub build provenance attestations.

About vault access: SimpleSync is a sync plugin, so it lists all files in the vault to compare them with your own SimpleSync server. File names and contents are sent only to the server address you enter, nowhere else.

## 0.3.2

- Renamed to SimpleSync (plugin id `simplesync`).

## 0.3.1

- The whole product (server, web admin, plugin) uses one name.

## 0.3.0

- Name that follows the Obsidian plugin naming guidelines.

## 0.2.0

- Server-first connect: when the server vault already has content, this device becomes its copy and nothing is uploaded. Local files that differ go to Obsidian's trash.
- English UI with a language picker (English, Čeština, Slovenčina, Deutsch, Français, Español, Italiano, Polski) in the plugin and the web admin.

## 0.1.0

- First version: sync server with web admin, scheduled vault backups and the Obsidian plugin.
