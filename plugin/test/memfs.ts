import { FOLDER_MARKER, isFolderMarker, type FileStat, type LocalFS } from "../src/engine/types";

let clock = 1_700_000_000_000;

/** In-memory vault used to simulate devices in tests. */
export class MemFS implements LocalFS {
	files = new Map<string, { data: Uint8Array; mtime: number }>();
	/** Folders that exist without notes in them (like an empty folder in Obsidian). */
	folders = new Set<string>();

	private emptyFolders(): string[] {
		return [...this.folders].filter((d) => ![...this.files.keys()].some((p) => p.startsWith(d + "/")) && ![...this.folders].some((o) => o.startsWith(d + "/")));
	}

	set(path: string, text: string) {
		this.files.set(path, { data: new TextEncoder().encode(text), mtime: clock++ });
	}
	get(path: string): string | undefined {
		const f = this.files.get(path);
		return f && new TextDecoder().decode(f.data);
	}
	mkdir(path: string) {
		this.folders.add(path);
	}
	del(path: string) {
		this.files.delete(path);
	}
	snapshot(): Record<string, string> {
		const out: Record<string, string> = {};
		for (const k of [...this.files.keys()].sort()) out[k] = this.get(k)!;
		return out;
	}

	async list(): Promise<FileStat[]> {
		const out = [...this.files].map(([path, f]) => ({ path, size: f.data.byteLength, mtime: f.mtime }));
		for (const d of this.emptyFolders()) out.push({ path: `${d}/${FOLDER_MARKER}`, size: 0, mtime: 0 });
		return out;
	}
	async stat(path: string): Promise<FileStat | null> {
		if (isFolderMarker(path)) return this.emptyFolders().includes(path.slice(0, -FOLDER_MARKER.length - 1)) ? { path, size: 0, mtime: 0 } : null;
		const f = this.files.get(path);
		return f ? { path, size: f.data.byteLength, mtime: f.mtime } : null;
	}
	async read(path: string): Promise<ArrayBuffer> {
		if (isFolderMarker(path)) return new ArrayBuffer(0);
		const f = this.files.get(path);
		if (!f) throw new Error("ENOENT " + path);
		return f.data.slice().buffer;
	}
	async write(path: string, data: ArrayBuffer, mtime: number): Promise<FileStat> {
		if (isFolderMarker(path)) {
			this.folders.add(path.slice(0, -FOLDER_MARKER.length - 1));
			return { path, size: 0, mtime: 0 };
		}
		this.files.set(path, { data: new Uint8Array(data.slice(0)), mtime });
		return { path, size: data.byteLength, mtime };
	}
	async remove(path: string): Promise<void> {
		if (isFolderMarker(path)) {
			this.folders.delete(path.slice(0, -FOLDER_MARKER.length - 1));
			return;
		}
		this.files.delete(path);
	}
	async trash(path: string): Promise<void> {
		if (isFolderMarker(path)) return this.remove(path);
		const f = this.files.get(path);
		if (!f) return;
		this.files.delete(path);
		this.files.set(".trash/" + path, f);
	}
	/** Files outside the trash. */
	notes(): Record<string, string> {
		return Object.fromEntries(Object.entries(this.snapshot()).filter(([k]) => !k.startsWith(".trash/")));
	}
}
