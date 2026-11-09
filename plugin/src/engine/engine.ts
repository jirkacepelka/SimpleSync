import { t } from "../i18n";
import { Access } from "./access";
import { ApiError, Client } from "./client";
import { sha256 } from "./hash";
import { isMergeable, mergeText } from "./merge";
import type { BaseEntry, CommitOp, LocalFS, RemoteEntry, SyncState } from "./types";

interface LocalFile {
	path: string; // actual path on this device (may be NFD on Apple systems)
	size: number;
	mtime: number;
	hash: string;
}

export interface EngineOptions {
	fs: LocalFS;
	client: Client;
	state: SyncState;
	saveState: (s: SyncState) => Promise<void>;
	ignore: (path: string) => boolean;
	/** Who made the local edits; named in conflict copies (the user name). */
	author: string;
	log?: (msg: string) => void;
	/** Called when a conflict could not be merged and a copy was created. */
	onConflict?: (path: string, copyPath: string) => void;
	now?: () => Date;
}

export interface SyncResult {
	pulled: number;
	pushed: number;
	conflicts: number;
	readOnly: boolean;
	/** Paths that could not be synced (e.g. case collisions, too large). */
	problems: string[];
	/** Local files moved to the trash while taking over the server state. */
	trashed: number;
	/** Files moved to the trash because the member may no longer see them. */
	hidden: number;
	/** Local changes not uploaded because the folder is read-only (or hidden) for this member. */
	notAllowed: number;
}

/** Thrown instead of deleting a large part of the vault on the server. */
export class MassDeleteError extends Error {
	constructor(public count: number) {
		super(t("engine.massDelete", { count }));
	}
}

const MAX_MERGE_SIZE = 2 * 1024 * 1024;
const COMMIT_BATCH = 200;
const dec = new TextDecoder("utf-8", { fatal: true });
const enc = new TextEncoder();

/**
 * A path from the server must stay inside the vault. The server already
 * enforces this; checking again means even a broken or hostile server
 * cannot make this device write elsewhere.
 */
export function safePath(p: string): boolean {
	if (!p || p.length > 1024 || p.startsWith("/") || p.includes("\\") || p.includes("\0")) return false;
	return p.split("/").every((seg) => seg !== "" && seg !== "." && seg !== "..");
}

function nfc(p: string): string {
	return p.normalize("NFC");
}

function toBuffer(u: Uint8Array): ArrayBuffer {
	return u.buffer.slice(u.byteOffset, u.byteOffset + u.byteLength) as ArrayBuffer;
}

/**
 * The sync algorithm. Each device remembers, per path, the content hash it
 * last agreed on with the server ("base"). A sync pass:
 *  1. scans local files and hashes the ones whose size/mtime changed,
 *  2. pulls server changes since the last seen revision and applies them,
 *     resolving conflicts (3-way merge for text, conflict copy otherwise),
 *  3. pushes local changes with compare-and-swap commits against the base.
 *
 * When connecting to a vault that already has content on the server
 * (state.initialFromServer), the first sync only pulls: differing and
 * local-only files are moved to the local trash and nothing is uploaded.
 */
export class SyncEngine {
	/** When set, the next sync may delete many files on the server. */
	allowMassDelete = false;

	constructor(private o: EngineOptions) {}

	get state(): SyncState {
		return this.o.state;
	}

	private log(msg: string) {
		this.o.log?.(msg);
	}

	async sync(): Promise<SyncResult> {
		const result: SyncResult = { pulled: 0, pushed: 0, conflicts: 0, readOnly: false, problems: [], trashed: 0, hidden: 0, notAllowed: 0 };
		// Pushing can race with another device; a rejected commit is resolved
		// by pulling again, so a few rounds may be needed.
		try {
			for (let round = 0; round < 4; round++) {
				if (!(await this.pass(result))) break;
			}
		} finally {
			this.allowMassDelete = false;
		}
		// Cleared only after a complete pass, so an interrupted first sync
		// resumes in "copy the server" mode.
		if (this.o.state.initialFromServer) {
			this.o.state.initialFromServer = false;
			await this.o.saveState(this.o.state);
		}
		return result;
	}

	private async scan(): Promise<{ local: Map<string, LocalFile>; unreadable: Set<string> }> {
		const base = this.o.state.base;
		const local = new Map<string, LocalFile>();
		const unreadable = new Set<string>();
		for (const st of await this.o.fs.list()) {
			const key = nfc(st.path);
			if (this.o.ignore(key)) continue;
			const b = base[key];
			let hash: string;
			if (b && b.size === st.size && b.mtime === st.mtime) {
				hash = b.hash;
			} else {
				try {
					hash = await sha256(await this.o.fs.read(st.path));
				} catch (e) {
					this.log(`Cannot read ${st.path}: ${e}`);
					unreadable.add(key);
					continue;
				}
				// Content unchanged, only touched: remember the new stat.
				if (b && b.hash === hash) base[key] = { hash, size: st.size, mtime: st.mtime };
			}
			local.set(key, { path: st.path, size: st.size, mtime: st.mtime, hash });
		}
		return { local, unreadable };
	}

	/** One scan/pull/push pass. Returns true if another pass is needed. */
	private async pass(result: SyncResult): Promise<boolean> {
		const { local, unreadable } = await this.scan();

		// ---- pull ----
		let since = this.o.state.lastRev;
		let access = new Access("editor");
		let accessChecked = false;
		for (;;) {
			const page = await this.o.client.changes(this.o.state.vaultId, since);
			if (page.head < since) {
				// The server was reset or restored from an old backup: start over.
				since = 0;
				continue;
			}
			result.readOnly = page.role === "viewer";
			if (!accessChecked) {
				accessChecked = true;
				access = new Access(page.role, page.access?.rules);
				const rev = page.access?.rev ?? 0;
				if ((this.o.state.accessRev ?? 0) !== rev) {
					// Folder access changed: drop what is no longer visible and
					// read the whole change list again for what became visible.
					await this.dropHidden(access, local, result);
					this.o.state.accessRev = rev;
					await this.o.saveState(this.o.state);
					if (since > 0) {
						since = 0;
						continue;
					}
				}
			}
			for (const r of page.changes) {
				if (this.o.ignore(r.path) || !safePath(r.path)) continue;
				if (await this.applyRemote(r, local, result)) result.pulled++;
			}
			since = page.more && page.changes.length ? page.changes[page.changes.length - 1].rev : page.head;
			this.o.state.lastRev = since;
			await this.o.saveState(this.o.state);
			if (!page.more) break;
		}
		const base = this.o.state.base;
		if (this.o.state.initialFromServer) {
			// Files that exist only here are set aside, not uploaded.
			for (const [key, f] of local) {
				if (!base[key]) {
					await this.o.fs.trash(f.path);
					local.delete(key);
					result.trashed++;
				}
			}
			return false;
		}
		if (result.readOnly) return false;

		// ---- push ----
		// Changes in folders this member may not write stay on this device.
		let notAllowed = 0;
		const ops: { op: CommitOp; file?: LocalFile }[] = [];
		for (const [key, f] of local) {
			const b = base[key];
			if (!b || b.hash !== f.hash) {
				if (!access.canWrite(key)) {
					notAllowed++;
					continue;
				}
				ops.push({ op: { path: key, hash: f.hash, size: f.size, mtime: f.mtime, deleted: false, base_hash: b?.hash ?? "" }, file: f });
			}
		}
		let deletes = 0;
		for (const key of Object.keys(base)) {
			if (!local.has(key) && !unreadable.has(key) && !this.o.ignore(key)) {
				if (!access.canWrite(key)) {
					notAllowed++;
					continue;
				}
				ops.push({ op: { path: key, hash: "", size: 0, mtime: Date.now(), deleted: true, base_hash: base[key].hash } });
				deletes++;
			}
		}
		result.notAllowed = notAllowed;
		if (!ops.length) return false;
		const known = Object.keys(base).length;
		if (deletes > 20 && deletes > known / 2 && !this.allowMassDelete) throw new MassDeleteError(deletes);

		// Upload content the server does not have yet.
		const hashes = [...new Set(ops.filter((o) => !o.op.deleted).map((o) => o.op.hash))];
		const missing = new Set<string>();
		for (let i = 0; i < hashes.length; i += 1000) {
			for (const h of await this.o.client.missing(this.o.state.vaultId, hashes.slice(i, i + 1000))) missing.add(h);
		}
		const ready: CommitOp[] = [];
		const byPath = new Map<string, { op: CommitOp; file?: LocalFile }>();
		for (const o of ops) {
			if (!o.op.deleted && missing.has(o.op.hash)) {
				const data = await this.o.fs.read(o.file!.path);
				if ((await sha256(data)) !== o.op.hash) continue; // edited meanwhile; next sync
				try {
					await this.o.client.upload(this.o.state.vaultId, o.op.hash, data);
				} catch (e) {
					if (e instanceof ApiError && (e.code === "too_large" || e.status === 413)) {
						result.problems.push(t("problem.tooLarge", { path: o.op.path }));
						continue;
					}
					throw e;
				}
				missing.delete(o.op.hash);
			}
			ready.push(o.op);
			byPath.set(o.op.path, o);
		}

		let again = false;
		for (let i = 0; i < ready.length; i += COMMIT_BATCH) {
			const { results } = await this.o.client.commit(this.o.state.vaultId, ready.slice(i, i + COMMIT_BATCH));
			for (const r of results) {
				const o = byPath.get(r.path);
				if (!o) continue;
				if (r.ok) {
					result.pushed++;
					if (o.op.deleted) delete base[r.path];
					else base[r.path] = { hash: o.op.hash, size: o.file!.size, mtime: o.file!.mtime };
				} else if (r.error === "conflict" || r.error === "missing_blob") {
					again = true;
				} else if (r.error === "forbidden") {
					// Access changed while syncing; the next sync knows the new rules.
					result.notAllowed++;
				} else if (r.error === "case_conflict") {
					result.problems.push(t("problem.case", { path: r.path, other: r.current?.path ?? "" }));
				} else {
					result.problems.push(`${r.path}: ${r.error}`);
				}
			}
			await this.o.saveState(this.o.state);
		}
		return again;
	}

	/**
	 * Removes what the member may no longer see: unchanged files go to the
	 * local trash, files edited here are kept but no longer synced.
	 */
	private async dropHidden(access: Access, local: Map<string, LocalFile>, result: SyncResult): Promise<void> {
		const base = this.o.state.base;
		for (const key of Object.keys(base)) {
			if (access.canRead(key)) continue;
			const l = local.get(key);
			if (l && l.hash === base[key].hash) {
				await this.o.fs.trash(l.path);
				local.delete(key);
				result.hidden++;
			}
			delete base[key];
		}
	}

	private async download(hash: string): Promise<ArrayBuffer> {
		const data = await this.o.client.download(this.o.state.vaultId, hash);
		if (!data) throw new Error(t("engine.missingContent", { hash: hash.slice(0, 8) }));
		if ((await sha256(data)) !== hash) throw new Error(t("engine.corrupt", { hash: hash.slice(0, 8) }));
		return data;
	}

	private async writeLocal(key: string, path: string, data: ArrayBuffer, mtime: number, hash: string, local: Map<string, LocalFile>): Promise<BaseEntry> {
		const st = await this.o.fs.write(path, data, mtime);
		local.set(key, { path, size: st.size, mtime: st.mtime, hash });
		return { hash, size: st.size, mtime: st.mtime };
	}

	/** Applies one server change. Returns true if the local vault changed. */
	private async applyRemote(r: RemoteEntry, local: Map<string, LocalFile>, result: SyncResult): Promise<boolean> {
		const base = this.o.state.base;
		const key = r.path;
		const b = base[key]?.hash ?? "";
		const rHash = r.deleted ? "" : r.hash;
		if (rHash === b) return false;

		let l = local.get(key);
		// The user may have edited the file since the scan; refresh it.
		if (l) {
			const st = await this.o.fs.stat(l.path);
			if (!st) {
				local.delete(key);
				l = undefined;
			} else if (st.size !== l.size || st.mtime !== l.mtime) {
				l = { path: l.path, size: st.size, mtime: st.mtime, hash: await sha256(await this.o.fs.read(l.path)) };
				local.set(key, l);
			}
		}
		const lHash = l?.hash ?? "";

		if (lHash === rHash) {
			// Both sides already agree.
			if (rHash === "") delete base[key];
			else base[key] = { hash: rHash, size: l!.size, mtime: l!.mtime };
			return false;
		}

		if (this.o.state.initialFromServer && l) {
			// Taking over the server state: keep the differing local file in the trash.
			await this.o.fs.trash(l.path);
			local.delete(key);
			l = undefined;
			result.trashed++;
			if (rHash === "") return true;
			base[key] = await this.writeLocal(key, key, await this.download(rHash), r.mtime, rHash, local);
			return true;
		}

		if (lHash === b) {
			// No local edits: take the server version.
			if (rHash === "") {
				if (l) await this.o.fs.remove(l.path);
				local.delete(key);
				delete base[key];
			} else {
				base[key] = await this.writeLocal(key, l?.path ?? key, await this.download(rHash), r.mtime, rHash, local);
			}
			return true;
		}

		// ---- conflict: changed on both sides ----
		if (rHash === "") {
			// Deleted remotely but edited here: keep the edit, upload it as new.
			delete base[key];
			return false;
		}
		if (lHash === "") {
			// Deleted here but edited remotely: the edit wins, restore it.
			base[key] = await this.writeLocal(key, key, await this.download(rHash), r.mtime, rHash, local);
			return true;
		}

		const localData = await this.o.fs.read(l!.path);
		const remoteData = await this.download(rHash);
		if (isMergeable(key) && localData.byteLength < MAX_MERGE_SIZE && remoteData.byteLength < MAX_MERGE_SIZE) {
			try {
				const baseData = b ? await this.o.client.download(this.o.state.vaultId, b) : new ArrayBuffer(0);
				if (baseData) {
					const merged = mergeText(key, dec.decode(localData), dec.decode(baseData), dec.decode(remoteData));
					if (merged !== null) {
						const data = toBuffer(enc.encode(merged));
						const mergedHash = await sha256(data);
						const st = await this.o.fs.write(l!.path, data, Date.now());
						local.set(key, { path: l!.path, size: st.size, mtime: st.mtime, hash: mergedHash });
						// Base is the server version; the merge result is pushed on top of it.
						base[key] = mergedHash === rHash ? { hash: rHash, size: st.size, mtime: st.mtime } : { hash: rHash, size: -1, mtime: -1 };
						this.log(`Merged changes in ${key}`);
						return true;
					}
				}
			} catch {
				/* not valid UTF-8 or base unavailable: fall back to a copy */
			}
		}

		const copy = await this.conflictPath(key, local);
		await this.writeLocal(copy, copy, localData, l!.mtime, lHash, local);
		base[key] = await this.writeLocal(key, l!.path, remoteData, r.mtime, rHash, local);
		result.conflicts++;
		this.log(`Conflict in ${key}, local version saved as ${copy}`);
		this.o.onConflict?.(key, copy);
		return true;
	}

	private async conflictPath(key: string, local: Map<string, LocalFile>): Promise<string> {
		const d = this.o.now?.() ?? new Date();
		const pad = (n: number) => String(n).padStart(2, "0");
		const stamp = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}${pad(d.getMinutes())}`;
		const slash = key.lastIndexOf("/");
		const dir = key.slice(0, slash + 1);
		const name = key.slice(slash + 1);
		const dot = name.lastIndexOf(".");
		const stem = dot > 0 ? name.slice(0, dot) : name;
		const ext = dot > 0 ? name.slice(dot) : "";
		const author = this.o.author.replace(/[\\/:*?"<>|#^[\]]/g, "").trim() || t("engine.device");
		for (let n = 1; ; n++) {
			const suffix = n === 1 ? "" : ` ${n}`;
			const p = `${dir}${stem} (${t("engine.conflictWord")} ${stamp} ${author}${suffix})${ext}`;
			if (!local.has(p) && !(await this.o.fs.stat(p))) return p;
		}
	}
}
