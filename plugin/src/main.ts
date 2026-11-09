import { Notice, Platform, Plugin, requestUrl, TAbstractFile } from "obsidian";
import { ApiError, Client, type VaultInfo } from "./engine/client";
import { MassDeleteError, SyncEngine } from "./engine/engine";
import { makeIgnore } from "./engine/ignore";
import type { Http, SyncState } from "./engine/types";
import { setLanguage, t } from "./i18n";
import { ObsidianFS } from "./obsidian-fs";
import { ConfirmModal, LoginModal, SimpleSyncSettingTab } from "./settings";

export interface Settings {
	serverUrl: string;
	username: string;
	token: string;
	vaultId: number | null;
	vaultName: string;
	syncConfig: boolean;
	/** Plugin folders (under .obsidian/plugins) left out of the settings sync. */
	excludedPlugins: string[];
	/** "auto" (follow Obsidian) or a language code. */
	language: string;
	/**
	 * Set in vaults downloaded from the web admin ("Download for Obsidian"):
	 * ask for the password on start and, after logging in, connect this vault.
	 */
	loginPrompt: boolean;
	pendingVaultId: number | null;
	pendingVaultName: string;
}

const DEFAULTS: Settings = {
	serverUrl: "",
	username: "",
	token: "",
	vaultId: null,
	vaultName: "",
	syncConfig: false,
	excludedPlugins: [],
	language: "auto",
	loginPrompt: false,
	pendingVaultId: null,
	pendingVaultName: "",
};

export type Status = { kind: "off" | "idle" | "syncing" | "error" | "offline"; text: string; at?: Date };

/** Http transport via Obsidian's requestUrl: no CORS, works on mobile. */
const obsidianHttp: Http = async (req) => {
	const res = await requestUrl({
		url: req.url,
		method: req.method,
		headers: req.headers,
		body: req.body,
		contentType: req.body === undefined ? undefined : req.contentType,
		throw: false,
	});
	let json: unknown;
	try {
		json = res.json;
	} catch {
		json = undefined;
	}
	return { status: res.status, json, arrayBuffer: res.arrayBuffer };
};

export default class SimpleSyncPlugin extends Plugin {
	settings: Settings = { ...DEFAULTS };
	status: Status = { kind: "off", text: t("status.off") };
	onStatusChange?: () => void;

	private engine: SyncEngine | null = null;
	private statusEl!: HTMLElement;
	private running: Promise<void> | null = null;
	private pending = false;
	private timer: number | null = null;
	private ws: WebSocket | null = null;
	private wsRetry = 0;
	private wsTimer: number | null = null;

	get deviceName(): string {
		const os = Platform.isIosApp ? "iPhone/iPad" : Platform.isAndroidApp ? "Android" : Platform.isMacOS ? "Mac" : Platform.isWin ? "Windows" : "Linux";
		return `${os} – ${this.app.vault.getName()}`;
	}

	get connected(): boolean {
		return !!this.settings.token && this.settings.vaultId !== null;
	}

	client(): Client {
		return new Client(this.settings.serverUrl, this.settings.token, obsidianHttp);
	}

	async onload() {
		this.settings = { ...DEFAULTS, ...(await this.loadData()) };
		setLanguage(this.settings.language);
		this.addSettingTab(new SimpleSyncSettingTab(this.app, this));

		this.statusEl = this.addStatusBarItem();
		this.statusEl.addClass("obsisync-status");
		this.statusEl.onClickEvent(() => (this.connected ? this.requestSync(0) : new Notice(t("status.clickToSetUp"))));
		this.addCommand({ id: "sync-now", name: t("cmd.syncNow"), callback: () => this.requestSync(0) });

		this.app.workspace.onLayoutReady(async () => {
			const onChange = (f: TAbstractFile) => {
				if (!f.path.startsWith(".")) this.requestSync(2000);
			};
			this.registerEvent(this.app.vault.on("create", onChange));
			this.registerEvent(this.app.vault.on("modify", onChange));
			this.registerEvent(this.app.vault.on("delete", onChange));
			this.registerEvent(this.app.vault.on("rename", onChange));
			this.registerInterval(window.setInterval(() => this.requestSync(0), 60_000));
			this.registerDomEvent(document, "visibilitychange", () => {
				if (document.visibilityState === "visible") {
					this.requestSync(500);
					if (this.connected && !this.ws) this.openSocket();
				}
			});
			if (this.connected) await this.start();
			else this.setStatus({ kind: "off", text: this.settings.token ? t("status.pickVault") : t("status.loggedOut") });
			if (!this.settings.token && this.settings.loginPrompt && this.settings.serverUrl) new LoginModal(this.app, this).open();
		});
	}

	onunload() {
		this.closeSocket();
		if (this.timer) window.clearTimeout(this.timer);
	}

	async saveSettings() {
		await this.saveData(this.settings);
	}

	async setLanguage(code: string) {
		this.settings.language = code;
		setLanguage(code);
		await this.saveSettings();
	}

	// ---- state ----

	private get statePath(): string {
		return `${this.manifest.dir}/state.json`;
	}

	private async loadState(): Promise<SyncState | null> {
		try {
			const s = JSON.parse(await this.app.vault.adapter.read(this.statePath)) as SyncState;
			return s.vaultId === this.settings.vaultId ? s : null;
		} catch {
			return null;
		}
	}

	private async saveState(s: SyncState) {
		await this.app.vault.adapter.write(this.statePath, JSON.stringify(s));
	}

	private makeEngine(state: SyncState): SyncEngine {
		return new SyncEngine({
			fs: new ObsidianFS(this.app, () => this.settings.syncConfig),
			client: this.client(),
			state,
			saveState: (s) => this.saveState(s),
			ignore: makeIgnore({ configDir: this.app.vault.configDir, syncConfig: this.settings.syncConfig, excludedPlugins: this.settings.excludedPlugins, pluginId: this.manifest.id }),
			author: this.settings.username,
			log: (m) => console.debug("[SimpleSync]", m),
			onConflict: (path, copy) => new Notice(t("notice.conflict", { path, copy }), 15000),
		});
	}

	/** Starts syncing the configured vault (after load or after connecting). */
	async start() {
		const state = (await this.loadState()) ?? { vaultId: this.settings.vaultId!, lastRev: 0, base: {} };
		this.engine = this.makeEngine(state);
		this.openSocket();
		this.requestSync(0);
	}

	/**
	 * Rebuilds the engine after a settings change. With rescan, the whole
	 * change feed is read again (files skipped by the old ignore rules).
	 */
	async restart(rescan = false) {
		if (!this.connected || !this.engine) return;
		await this.running;
		const state = this.engine.state;
		if (rescan) state.lastRev = 0;
		this.engine = this.makeEngine(state);
		this.requestSync(0);
	}

	// ---- account & vault ----

	async login(serverUrl: string, username: string, password: string) {
		const client = new Client(serverUrl, "", obsidianHttp);
		await client.ping();
		const token = await client.login(username, password, this.deviceName);
		const pending = this.settings.pendingVaultId;
		const pendingName = this.settings.pendingVaultName;
		this.settings = { ...this.settings, serverUrl, username, token, vaultId: null, vaultName: "", loginPrompt: false, pendingVaultId: null, pendingVaultName: "" };
		await this.saveSettings();
		this.setStatus({ kind: "off", text: t("status.pickVault") });
		if (pending === null) return;
		// A vault downloaded for Obsidian: its files came from this server
		// vault, so it is connected right away without asking.
		const vault = (await this.client().vaults()).vaults.find((v) => v.id === pending);
		if (!vault) throw new Error(t("login.pendingMissing", { vault: pendingName }));
		await this.connect(vault);
	}

	async logout() {
		await this.disconnect();
		try {
			await this.client().logout();
		} catch {
			/* token may already be revoked */
		}
		this.settings.token = "";
		await this.saveSettings();
		this.setStatus({ kind: "off", text: t("status.loggedOut") });
	}

	/**
	 * Connects this Obsidian vault to a server vault. If the server vault has
	 * content, this vault becomes a copy of it (local differences go to the
	 * trash, nothing is uploaded); an empty server vault receives this one.
	 */
	async connect(vault: VaultInfo) {
		await this.disconnect();
		this.settings.vaultId = vault.id;
		this.settings.vaultName = vault.name;
		await this.saveSettings();
		const serverHasContent = (vault.file_count ?? vault.head_rev) > 0;
		await this.saveState({ vaultId: vault.id, lastRev: 0, base: {}, initialFromServer: serverHasContent });
		await this.start();
	}

	async disconnect() {
		this.closeSocket();
		await this.running;
		this.engine = null;
		this.settings.vaultId = null;
		this.settings.vaultName = "";
		await this.saveSettings();
		this.setStatus({ kind: "off", text: t("status.pickVault") });
	}

	// ---- sync loop ----

	requestSync(delay: number) {
		if (!this.engine) return;
		if (this.timer) window.clearTimeout(this.timer);
		this.timer = window.setTimeout(() => {
			this.timer = null;
			void this.runSync();
		}, delay);
	}

	async runSync(): Promise<void> {
		if (!this.engine) return;
		if (this.running) {
			this.pending = true;
			return this.running;
		}
		const engine = this.engine;
		this.running = (async () => {
			this.setStatus({ kind: "syncing", text: t("status.syncing") });
			try {
				const r = await engine.sync();
				for (const p of r.problems) new Notice(`SimpleSync: ${p}`, 10000);
				if (r.trashed) new Notice(t("notice.replaced", { count: r.trashed }), 15000);
				if (r.hidden) new Notice(t("notice.accessRemoved", { count: r.hidden }), 15000);
				const text = r.readOnly ? t("status.syncedReadOnly") : r.notAllowed ? t("status.syncedNotAllowed", { count: r.notAllowed }) : t("status.synced");
				this.setStatus({ kind: "idle", text, at: new Date() });
			} catch (e) {
				await this.handleError(e);
			}
		})();
		await this.running;
		this.running = null;
		if (this.pending) {
			this.pending = false;
			this.requestSync(0);
		}
	}

	private async handleError(e: unknown) {
		console.error("[SimpleSync]", e);
		if (e instanceof MassDeleteError) {
			this.setStatus({ kind: "error", text: t("status.massDelete", { count: e.count }) });
			new ConfirmModal(
				this.app,
				t("massDelete.title"),
				t("massDelete.body", { count: e.count }),
				[
					{ label: t("massDelete.redownload"), cta: true, action: () => this.redownload() },
					{
						label: t("massDelete.delete"),
						warning: true,
						action: () => {
							if (this.engine) this.engine.allowMassDelete = true;
							this.requestSync(0);
						},
					},
				],
			).open();
			return;
		}
		if (e instanceof ApiError) {
			if (e.status === 401) {
				this.closeSocket();
				this.engine = null;
				this.setStatus({ kind: "error", text: t("status.expired") });
				new Notice(t("notice.expired"), 15000);
				return;
			}
			if (e.status === 404 && e.code === "not_found") {
				this.setStatus({ kind: "error", text: t("status.vaultGone") });
				return;
			}
			if (e.status === 0) {
				this.setStatus({ kind: "offline", text: t("status.offline") });
				return;
			}
		}
		this.setStatus({ kind: "error", text: t("status.error", { msg: e instanceof Error ? e.message : String(e) }) });
	}

	/** Forgets what was synced so that missing files are downloaded again. */
	private async redownload() {
		if (!this.engine) return;
		const state = this.engine.state;
		state.base = {};
		state.lastRev = 0;
		await this.saveState(state);
		this.engine = this.makeEngine(state);
		this.requestSync(0);
	}

	// ---- live notifications ----

	private openSocket() {
		this.closeSocket();
		if (!this.connected) return;
		let ws: WebSocket;
		try {
			ws = new WebSocket(this.client().wsUrl(this.settings.vaultId!));
		} catch {
			return;
		}
		this.ws = ws;
		ws.onopen = () => (this.wsRetry = 0);
		ws.onmessage = (ev) => {
			try {
				const msg = JSON.parse(String(ev.data)) as { rev: number; access?: number };
				const state = this.engine?.state;
				// Sync on new files, and when folder access changed (no file did).
				if (state && (msg.rev > state.lastRev || (msg.access !== undefined && msg.access !== (state.accessRev ?? 0)))) this.requestSync(300);
			} catch {
				/* ignore */
			}
		};
		ws.onclose = () => {
			if (this.ws !== ws) return;
			this.ws = null;
			if (!this.connected) return;
			// Reconnect with backoff; the 60 s poll covers the gap.
			const delay = Math.min(60_000, 1000 * 2 ** this.wsRetry++);
			this.wsTimer = window.setTimeout(() => this.openSocket(), delay);
		};
	}

	private closeSocket() {
		if (this.wsTimer) window.clearTimeout(this.wsTimer);
		this.wsTimer = null;
		const ws = this.ws;
		this.ws = null;
		ws?.close();
	}

	// ---- status bar ----

	setStatus(s: Status) {
		this.status = s;
		const icon = { off: "○", idle: "✓", syncing: "⟳", error: "⚠", offline: "⚡" }[s.kind];
		this.statusEl.setText(`${icon} SimpleSync`);
		this.statusEl.setAttr("aria-label", s.text + (s.at ? ` (${s.at.toLocaleTimeString()})` : ""));
		this.statusEl.setAttr("data-tooltip-position", "top");
		this.statusEl.toggleClass("is-error", s.kind === "error");
		this.onStatusChange?.();
	}
}
