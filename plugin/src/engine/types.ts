// Interfaces that decouple the sync engine from Obsidian, so it can run (and
// be tested) in plain Node as well.

export interface FileStat {
	path: string;
	size: number;
	mtime: number; // milliseconds
}

/**
 * Empty folders sync as an empty file with this name inside them. Real files
 * only exist on the server, so this is how a folder without notes reaches the
 * other devices. The LocalFS implementations present each empty folder as such
 * a file and turn writes and removals of it into folder operations.
 */
export const FOLDER_MARKER = ".obsisync-folder";

export function isFolderMarker(path: string): boolean {
	return path === FOLDER_MARKER || path.endsWith("/" + FOLDER_MARKER);
}

export interface LocalFS {
	/** All files of the vault (the engine applies ignore rules itself). */
	list(): Promise<FileStat[]>;
	stat(path: string): Promise<FileStat | null>;
	read(path: string): Promise<ArrayBuffer>;
	/** Writes a file (creating parent folders) and returns its resulting stat. */
	write(path: string, data: ArrayBuffer, mtime: number): Promise<FileStat>;
	remove(path: string): Promise<void>;
	/** Moves a file to the local trash (kept on this device, never synced). */
	trash(path: string): Promise<void>;
}

export interface HttpRequest {
	url: string;
	method: string;
	headers?: Record<string, string>;
	body?: string | ArrayBuffer;
	contentType?: string;
}

export interface HttpResponse {
	status: number;
	json: unknown;
	arrayBuffer: ArrayBuffer;
}

export type Http = (req: HttpRequest) => Promise<HttpResponse>;

/** What the device last agreed on with the server for one path. */
export interface BaseEntry {
	hash: string;
	size: number;
	mtime: number;
}

export interface SyncState {
	vaultId: number;
	lastRev: number;
	base: Record<string, BaseEntry>;
	/**
	 * Set when connecting to a vault that already has content on the server:
	 * until the first sync completes, this device becomes an exact copy of the
	 * server and uploads nothing.
	 */
	initialFromServer?: boolean;
}

export interface RemoteEntry {
	path: string;
	hash: string;
	size: number;
	mtime: number;
	deleted: boolean;
	rev: number;
}

export interface CommitOp {
	path: string;
	hash: string;
	size: number;
	mtime: number;
	deleted: boolean;
	base_hash: string;
}

export interface OpResult {
	path: string;
	ok: boolean;
	rev?: number;
	error?: string;
	current?: RemoteEntry;
}
