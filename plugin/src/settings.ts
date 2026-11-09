import { App, ButtonComponent, Notice, DropdownComponent, Modal, PluginSettingTab, requireApiVersion, Setting, type SettingDefinitionItem } from "obsidian";
import { insecureRemote, normalizeServerUrl, type VaultInfo } from "./engine/client";
import { LANGUAGES, t } from "./i18n";
import type SimpleSyncPlugin from "./main";

interface Choice {
	label: string;
	cta?: boolean;
	warning?: boolean;
	action: () => void;
}

/** A small dialog with a message and a few buttons. */
export class ConfirmModal extends Modal {
	constructor(
		app: App,
		private heading: string,
		private message: string,
		private choices: Choice[],
	) {
		super(app);
	}

	onOpen() {
		this.titleEl.setText(this.heading);
		this.contentEl.createEl("p", { text: this.message, cls: "obsisync-message" });
		const row = new Setting(this.contentEl);
		for (const c of this.choices) {
			row.addButton((b) => {
				b.setButtonText(c.label).onClick(() => {
					this.close();
					c.action();
				});
				if (c.cta) b.setCta();
				if (c.warning) b.setWarning();
			});
		}
	}

	onClose() {
		this.contentEl.empty();
	}
}

/** Asks before a password would cross the internet unencrypted. */
export function confirmInsecure(app: App, server: string): Promise<boolean> {
	if (!insecureRemote(server)) return Promise.resolve(true);
	return new Promise((resolve) =>
		new ConfirmModal(app, t("http.title"), t("http.body"), [
			{ label: t("replace.cancel"), cta: true, action: () => resolve(false) },
			{ label: t("http.continue"), warning: true, action: () => resolve(true) },
		]).open(),
	);
}

/** Lists the installed plugins and lets the user pick which ones are synced. */
export class PluginSyncModal extends Modal {
	constructor(
		app: App,
		private plugin: SimpleSyncPlugin,
	) {
		super(app);
	}

	async onOpen() {
		this.titleEl.setText(t("plugins.title"));
		const dir = `${this.app.vault.configDir}/plugins`;
		let ids: string[] = [];
		try {
			ids = (await this.app.vault.adapter.list(dir)).folders.map((f) => f.slice(f.lastIndexOf("/") + 1));
		} catch {
			// no plugins folder
		}
		const s = this.plugin.settings;
		ids = ids.filter((id) => id !== this.plugin.manifest.id);
		for (const id of s.excludedPlugins) if (!ids.includes(id)) ids.push(id);
		ids.sort((a, b) => a.localeCompare(b));
		this.contentEl.createEl("p", { text: t("plugins.desc"), cls: "obsisync-message" });
		if (!ids.length) this.contentEl.createEl("p", { text: t("plugins.none") });
		for (const id of ids) {
			new Setting(this.contentEl).setName(id).addToggle((c) =>
				c.setValue(!s.excludedPlugins.includes(id)).onChange((on) => {
					s.excludedPlugins = on ? s.excludedPlugins.filter((x) => x !== id) : [...s.excludedPlugins, id];
					this.changed = true;
				}),
			);
		}
	}

	private changed = false;

	async onClose() {
		this.contentEl.empty();
		if (!this.changed) return;
		await this.plugin.saveSettings();
		await this.plugin.restart(true);
	}
}

/**
 * Shown on the first start of a vault downloaded from the web admin: the
 * server, name and vault are preset, only the password is missing.
 */
export class LoginModal extends Modal {
	constructor(
		app: App,
		private plugin: SimpleSyncPlugin,
	) {
		super(app);
	}

	onOpen() {
		const s = this.plugin.settings;
		let user = s.username;
		let pass = "";
		let busy = false;
		this.titleEl.setText(t("loginModal.title"));
		this.contentEl.createEl("p", {
			text: s.pendingVaultName ? t("loginModal.introVault", { vault: s.pendingVaultName, server: s.serverUrl }) : t("loginModal.intro", { server: s.serverUrl }),
		});
		new Setting(this.contentEl).setName(t("login.name")).addText((c) => {
			c.setValue(user).onChange((v) => (user = v));
			c.inputEl.autocapitalize = "off";
			c.inputEl.autocomplete = "username";
		});
		const errorEl = this.contentEl.createDiv({ cls: "obsisync-error" });
		let button: ButtonComponent | undefined;
		const submit = async () => {
			if (busy) return;
			if (!user.trim() || !pass) {
				errorEl.setText(t("login.missing"));
				return;
			}
			if (!(await confirmInsecure(this.app, s.serverUrl))) return;
			busy = true;
			errorEl.setText("");
			button?.setButtonText(t("login.busy")).setDisabled(true);
			try {
				await this.plugin.login(s.serverUrl, user.trim(), pass);
				this.close();
				new Notice(this.plugin.connected ? t("loginModal.connected", { vault: this.plugin.settings.vaultName }) : t("loginModal.pickVault"));
			} catch (e) {
				errorEl.setText(e instanceof Error ? e.message : String(e));
			}
			busy = false;
			button?.setButtonText(t("login.button")).setDisabled(false);
		};
		new Setting(this.contentEl).setName(t("login.password")).addText((c) => {
			c.inputEl.type = "password";
			c.inputEl.autocomplete = "current-password";
			c.onChange((v) => (pass = v));
			c.inputEl.addEventListener("keydown", (e) => e.key === "Enter" && void submit());
			window.setTimeout(() => c.inputEl.focus(), 50);
		});
		new Setting(this.contentEl).addButton((b) => {
			button = b.setButtonText(t("login.button")).setCta().onClick(() => void submit());
		});
	}

	onClose() {
		this.contentEl.empty();
	}
}

/**
 * One settings row. build() fills a fresh Setting and may return a sync
 * function that brings the row up to date after the state changed.
 */
interface Row {
	name: string;
	desc?: string;
	visible: () => boolean;
	build: (s: Setting) => (() => void) | void;
}

/**
 * The settings tab. On Obsidian 1.13+ the rows are handed over as setting
 * definitions (so they show up in the settings search); older versions
 * render the same rows in display().
 */
export class SimpleSyncSettingTab extends PluginSettingTab {
	private error = "";
	private busy = false;
	private vaults: VaultInfo[] | null = null;
	private canCreate = false;
	/** What the user is typing into the login form (the password is never saved). */
	private form = { url: "", user: "", pass: "" };
	/** Sync functions of the rows currently on screen. */
	private syncs = new Set<() => void>();

	constructor(
		app: App,
		private plugin: SimpleSyncPlugin,
	) {
		super(app, plugin);
		this.resetForm();
	}

	private resetForm() {
		this.form = { url: this.plugin.settings.serverUrl, user: this.plugin.settings.username, pass: "" };
	}

	private get declarative(): boolean {
		return requireApiVersion("1.13.0");
	}

	getSettingDefinitions(): SettingDefinitionItem[] {
		return this.rows().map((r) => ({
			name: r.name,
			desc: r.desc,
			visible: r.visible,
			render: (setting: Setting) => this.mount(r, setting),
		}));
	}

	/** Fallback for Obsidian older than 1.13. */
	display() {
		const { containerEl } = this;
		containerEl.empty();
		containerEl.addClass("obsisync-settings");
		this.syncs.clear();
		for (const r of this.rows()) {
			if (!r.visible()) continue;
			const setting = new Setting(containerEl).setName(r.name);
			if (r.desc) setting.setDesc(r.desc);
			this.mount(r, setting);
		}
	}

	hide() {
		this.plugin.onStatusChange = undefined;
		this.syncs.clear();
	}

	private mount(r: Row, setting: Setting): () => void {
		this.plugin.onStatusChange = () => this.syncAll();
		const sync = r.build(setting);
		if (!sync) return () => {};
		this.syncs.add(sync);
		sync();
		return () => this.syncs.delete(sync);
	}

	private syncAll() {
		for (const sync of [...this.syncs]) sync();
	}

	/** Shows the current state: which rows are visible and what they say. */
	private refresh() {
		if (!this.declarative) {
			this.display();
			return;
		}
		this.update();
		this.refreshDomState();
		this.syncAll();
	}

	private async run(fn: () => Promise<void>) {
		this.busy = true;
		this.error = "";
		this.refresh();
		try {
			await fn();
		} catch (e) {
			this.error = e instanceof Error ? e.message : String(e);
		}
		this.busy = false;
		this.refresh();
	}

	private rows(): Row[] {
		const s = this.plugin.settings;
		const loggedOut = () => !s.token;
		const picking = () => !!s.token && s.vaultId === null;
		const connected = () => !!s.token && s.vaultId !== null;
		return [
			{
				name: t("replace.warningTitle"),
				desc: t("replace.warning", { create: t("vaults.createButton") }),
				visible: () => !connected(),
				build: (row) => {
					row.settingEl.addClass("obsisync-warning");
				},
			},
			...this.loginRows(loggedOut),
			...this.pickerRows(picking),
			...this.connectedRows(connected),
			{
				name: t("settings.error"),
				visible: () => !!this.error,
				build: (row) => {
					row.settingEl.addClass("obsisync-error");
					return () => {
						row.setDesc(this.error);
						row.settingEl.toggle(!!this.error);
					};
				},
			},
			{
				name: t("settings.language"),
				desc: t("settings.languageDesc"),
				visible: () => true,
				build: (row) => {
					row.addDropdown((d) => {
						d.addOption("auto", t("settings.languageAuto"));
						for (const l of LANGUAGES) d.addOption(l.code, l.name);
						d.setValue(s.language).onChange(async (v) => {
							await this.plugin.setLanguage(v);
							this.refresh();
						});
					});
				},
			},
		];
	}

	// ---- step 1: server, name, password ----

	private loginRows(visible: () => boolean): Row[] {
		const login = () =>
			this.run(async () => {
				const server = normalizeServerUrl(this.form.url);
				const user = this.form.user.trim();
				if (!server || !user || !this.form.pass) throw new Error(t("login.missing"));
				await this.plugin.login(server, user, this.form.pass);
				this.form.pass = "";
				this.vaults = null;
			});
		const submit = async () => {
			if (await confirmInsecure(this.app, normalizeServerUrl(this.form.url))) await login();
		};
		return [
			{
				name: t("login.server"),
				desc: `${t("login.intro")} ${t("login.serverDesc")}`,
				visible,
				build: (row) => {
					row.addText((c) =>
						c
							.setPlaceholder("https://…")
							.setValue(this.form.url)
							.onChange((v) => (this.form.url = v)),
					);
				},
			},
			{
				name: t("login.name"),
				visible,
				build: (row) => {
					row.addText((c) => {
						c.setValue(this.form.user).onChange((v) => (this.form.user = v));
						c.inputEl.autocapitalize = "off";
						c.inputEl.autocomplete = "username";
					});
				},
			},
			{
				name: t("login.password"),
				visible,
				build: (row) => {
					row.addText((c) => {
						c.inputEl.type = "password";
						c.inputEl.autocomplete = "current-password";
						c.setValue(this.form.pass).onChange((v) => (this.form.pass = v));
						c.inputEl.addEventListener("keydown", (e) => e.key === "Enter" && void submit());
					});
				},
			},
			{
				name: t("login.button"),
				visible,
				build: (row) => {
					let button: ButtonComponent | undefined;
					row.addButton((b) => {
						button = b.setCta().onClick(() => void submit());
					});
					return () => {
						button?.setButtonText(this.busy ? t("login.busy") : t("login.button")).setDisabled(this.busy);
					};
				},
			},
		];
	}

	// ---- step 2: pick a vault ----

	private accountRow(visible: () => boolean): Row {
		const s = this.plugin.settings;
		return {
			name: t("account.logout"),
			visible,
			build: (row) => {
				row.addButton((b) =>
					b.setButtonText(t("account.logout")).onClick(() =>
						this.run(async () => {
							await this.plugin.logout();
							this.resetForm();
						}),
					),
				);
				return () => {
					row.setName(t("account.loggedIn", { user: s.username }));
					row.setDesc(s.serverUrl);
				};
			},
		};
	}

	/** Loads the vault list once; deferred so it never re-renders mid-render. */
	private loadVaults() {
		const needed = () => this.vaults === null && !this.error && !this.busy && !!this.plugin.settings.token && this.plugin.settings.vaultId === null;
		if (!needed()) return;
		window.setTimeout(() => {
			if (!needed()) return;
			void this.run(async () => {
				const r = await this.plugin.client().vaults();
				this.vaults = r.vaults;
				this.canCreate = r.can_create;
			});
		}, 0);
	}

	private pickerRows(visible: () => boolean): Row[] {
		let selected: number | undefined;
		return [
			this.accountRow(visible),
			{
				name: t("vaults.label"),
				visible,
				build: (row) => {
					let dropdown: DropdownComponent | undefined;
					let button: ButtonComponent | undefined;
					let shown: VaultInfo[] | null = null;
					row.addDropdown((d) => {
						dropdown = d.onChange((v) => (selected = Number(v)));
					});
					row.addButton((b) => {
						button = b
							.setButtonText(t("vaults.connect"))
							.setCta()
							.onClick(() => {
								const v = this.vaults?.find((x) => x.id === selected);
								if (v) this.connect(v);
							});
					});
					return () => {
						this.loadVaults();
						const vaults = this.vaults;
						if (vaults !== shown && dropdown) {
							shown = vaults;
							dropdown.selectEl.empty();
							for (const v of vaults ?? []) dropdown.addOption(String(v.id), `${v.name} (${t(`role.${v.role}`)})`);
							selected = vaults?.[0]?.id;
						}
						const has = !!vaults?.length;
						row.setDesc(vaults === null ? (this.error ? "" : t("vaults.loading")) : has ? t("vaults.desc") : t("vaults.none"));
						dropdown?.selectEl.toggle(has);
						button?.buttonEl.toggle(has);
						button?.setDisabled(this.busy);
					};
				},
			},
			{
				name: t("vaults.create"),
				desc: t("vaults.createDesc"),
				visible: () => visible() && this.canCreate,
				build: (row) => {
					let name = this.app.vault.getName();
					let button: ButtonComponent | undefined;
					row.addText((c) => c.setValue(name).onChange((v) => (name = v)));
					row.addButton((b) => {
						button = b.setButtonText(t("vaults.createButton")).onClick(() =>
							this.run(async () => {
								const v = await this.plugin.client().createVault(name.trim());
								await this.plugin.connect(v);
							}),
						);
					});
					return () => {
						row.settingEl.toggle(visible() && this.canCreate);
						button?.setDisabled(this.busy);
					};
				},
			},
			{
				name: t("vaults.refresh"),
				visible,
				build: (row) => {
					row.addButton((b) =>
						b.setButtonText(t("vaults.refresh")).onClick(() => {
							this.vaults = null;
							this.error = "";
							this.refresh();
						}),
					);
				},
			},
		];
	}

	/**
	 * A server vault with content always wins: this vault becomes its copy.
	 * Confirm first when that would move local notes to the trash.
	 */
	private connect(vault: VaultInfo) {
		const serverHasContent = (vault.file_count ?? vault.head_rev) > 0;
		if (!serverHasContent || this.app.vault.getFiles().length === 0) {
			void this.run(() => this.plugin.connect(vault));
			return;
		}
		new ConfirmModal(this.app, t("replace.title", { vault: vault.name }), t("replace.body"), [
			{ label: t("replace.confirm"), cta: true, action: () => void this.run(() => this.plugin.connect(vault)) },
			{ label: t("replace.cancel"), action: () => {} },
		]).open();
	}

	// ---- step 3: connected ----

	private connectedRows(visible: () => boolean): Row[] {
		const s = this.plugin.settings;
		return [
			{
				name: t("connected.syncNow"),
				visible,
				build: (row) => {
					row.addButton((b) =>
						b
							.setButtonText(t("connected.syncNow"))
							.setCta()
							.onClick(() => this.plugin.requestSync(0)),
					);
					const status = row.infoEl.createDiv({ cls: "obsisync-statusline" });
					return () => {
						row.setName(t("connected.title", { vault: s.vaultName }));
						row.setDesc(`${s.username} @ ${s.serverUrl}`);
						const st = this.plugin.status;
						status.setText(st.text + (st.at ? ` · ${st.at.toLocaleTimeString()}` : ""));
						status.toggleClass("obsisync-error", st.kind === "error");
					};
				},
			},
			{
				name: t("connected.syncConfig"),
				desc: t("connected.syncConfigDesc"),
				visible,
				build: (row) => {
					row.addToggle((c) =>
						c.setValue(s.syncConfig).onChange(async (v) => {
							s.syncConfig = v;
							await this.plugin.saveSettings();
							await this.plugin.restart(v);
							this.refresh();
						}),
					);
				},
			},
			{
				name: t("connected.syncPlugins"),
				desc: t("connected.syncPluginsDesc"),
				visible: () => visible() && s.syncConfig,
				build: (row) => {
					row.addButton((b) => b.setButtonText(t("connected.syncPluginsButton")).onClick(() => new PluginSyncModal(this.app, this.plugin).open()));
					return () => row.settingEl.toggle(visible() && s.syncConfig);
				},
			},
			{
				name: t("connected.disconnect"),
				desc: t("connected.disconnectDesc"),
				visible,
				build: (row) => {
					row.addButton((b) =>
						b.setButtonText(t("connected.disconnectButton")).onClick(() =>
							this.run(async () => {
								await this.plugin.disconnect();
								this.vaults = null;
							}),
						),
					);
				},
			},
			this.accountRow(visible),
		];
	}
}
