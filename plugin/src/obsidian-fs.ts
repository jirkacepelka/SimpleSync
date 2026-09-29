import { normalizePath, TFile, TFolder, type App } from "obsidian";
import { FOLDER_MARKER, isFolderMarker, type FileStat, type LocalFS } from "./engine/types";

/** LocalFS backed by the Obsidian vault (works on desktop and mobile). */
export class ObsidianFS implements LocalFS {
	constructor(
		private app: App,
		private includeConfig: () => boolean,
	) {}

	private get adapter() {
		return this.app.vault.adapter;
	}

	private hidden(path: string): boolean {
		return path.split("/").some((s) => s.startsWith("."));
	}

	private markerOf(folder: string): string {
		return `${folder}/${FOLDER_MARKER}`;
	}

	/** Empty folder behind a marker path, or null when it is missing or not empty. */
	private emptyFolder(marker: string): string | null {
		const dir = marker.slice(0, marker.length - FOLDER_MARKER.length - 1);
		const f = dir ? this.app.vault.getAbstractFileByPath(dir) : null;
		return f instanceof TFolder && !f.children.length ? dir : null;
	}

	async list(): Promise<FileStat[]> {
		const out: FileStat[] = this.app.vault.getFiles().map((f) => ({ path: f.path, size: f.stat.size, mtime: f.stat.mtime }));
		// Empty folders are presented as marker files so they sync too.
		for (const f of this.app.vault.getAllLoadedFiles()) {
			if (f instanceof TFolder && f.path !== "/" && !f.isRoot() && !f.children.length) out.push({ path: this.markerOf(f.path), size: 0, mtime: 0 });
		}
		if (this.includeConfig()) {
			// The vault index does not contain dot-folders; walk the config dir.
			const walk = async (dir: string) => {
				const res = await this.adapter.list(dir);
				for (const f of res.files) {
					const st = await this.adapter.stat(f);
					if (st?.type === "file") out.push({ path: f, size: st.size, mtime: st.mtime });
				}
				for (const d of res.folders) await walk(d);
			};
			if (await this.adapter.exists(this.app.vault.configDir)) await walk(this.app.vault.configDir);
		}
		return out;
	}

	async stat(path: string): Promise<FileStat | null> {
		if (isFolderMarker(path)) return this.emptyFolder(normalizePath(path)) ? { path, size: 0, mtime: 0 } : null;
		const st = await this.adapter.stat(normalizePath(path));
		return st?.type === "file" ? { path, size: st.size, mtime: st.mtime } : null;
	}

	read(path: string): Promise<ArrayBuffer> {
		if (isFolderMarker(path)) return Promise.resolve(new ArrayBuffer(0));
		return this.adapter.readBinary(normalizePath(path));
	}

	private async ensureFolder(path: string) {
		const dir = path.slice(0, path.lastIndexOf("/"));
		if (!dir || (await this.adapter.exists(dir))) return;
		if (this.hidden(dir)) await this.adapter.mkdir(dir);
		else await this.app.vault.createFolder(dir).catch(() => this.adapter.mkdir(dir));
	}

	async write(path: string, data: ArrayBuffer, mtime: number): Promise<FileStat> {
		path = normalizePath(path);
		if (isFolderMarker(path)) {
			// A marker means "this folder exists": create it, no file is written.
			await this.ensureFolder(path);
			return { path, size: 0, mtime: 0 };
		}
		await this.ensureFolder(path);
		const opts = { mtime: mtime > 0 ? mtime : Date.now() };
		const file = this.app.vault.getAbstractFileByPath(path);
		// Going through the vault API keeps Obsidian's index and open editors
		// up to date; hidden files are not indexed, so use the adapter.
		if (file instanceof TFile) await this.app.vault.modifyBinary(file, data, opts);
		else if (this.hidden(path) || file) await this.adapter.writeBinary(path, data, opts);
		else await this.app.vault.createBinary(path, data, opts);
		const st = await this.adapter.stat(path);
		return { path, size: st?.size ?? data.byteLength, mtime: st?.mtime ?? opts.mtime };
	}

	async trash(path: string): Promise<void> {
		path = normalizePath(path);
		if (isFolderMarker(path)) return this.remove(path);
		const file = this.app.vault.getAbstractFileByPath(path);
		if (file instanceof TFile) await this.app.vault.trash(file, false);
		else if (await this.adapter.exists(path)) await this.adapter.trashLocal(path);
	}

	async remove(path: string): Promise<void> {
		path = normalizePath(path);
		if (isFolderMarker(path)) {
			// Only an empty folder goes away; one with notes in it is kept.
			const dir = this.emptyFolder(path);
			if (!dir) return;
			const folder = this.app.vault.getAbstractFileByPath(dir);
			if (folder instanceof TFolder) await this.app.vault.delete(folder, true);
			path = dir + "/x"; // fall through to prune emptied parents
		}
		const file = this.app.vault.getAbstractFileByPath(path);
		if (file instanceof TFile) {
			// Local Obsidian trash (never synced): a safety net for deletions
			// arriving from other devices.
			await this.app.vault.trash(file, false);
		} else if (await this.adapter.exists(path)) {
			await this.adapter.remove(path);
		}
		// Remove folders left empty by the deletion.
		let dir = path.slice(0, path.lastIndexOf("/"));
		while (dir) {
			const folder = this.app.vault.getAbstractFileByPath(dir);
			if (folder instanceof TFolder) {
				if (folder.children.length) break;
				await this.app.vault.delete(folder, true);
			} else if (this.hidden(dir) && (await this.adapter.exists(dir))) {
				const res = await this.adapter.list(dir);
				if (res.files.length || res.folders.length) break;
				await this.adapter.rmdir(dir, false);
			} else break;
			dir = dir.slice(0, Math.max(0, dir.lastIndexOf("/")));
		}
	}
}
