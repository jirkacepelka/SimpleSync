import { FOLDER_MARKER } from "./types";

// Paths that are never synchronized.

export interface IgnoreOptions {
	configDir: string; // usually ".obsidian"
	syncConfig: boolean; // sync the settings folder too
	pluginId: string;
}

export function makeIgnore(o: IgnoreOptions): (path: string) => boolean {
	const cfg = o.configDir + "/";
	const own = `${cfg}plugins/${o.pluginId}/`;
	const volatile = new Set([`${cfg}workspace.json`, `${cfg}workspace-mobile.json`, `${cfg}workspaces.json`]);
	return (path: string) => {
		if (path.startsWith(".trash/") || path.startsWith(".git/")) return true;
		const name = path.slice(path.lastIndexOf("/") + 1);
		if (name === ".DS_Store" || name === "Thumbs.db" || name.startsWith("~$") || name.endsWith(".tmp")) return true;
		if (path.startsWith(cfg)) {
			if (!o.syncConfig) return true;
			if (path.startsWith(own) || volatile.has(path) || path.startsWith(`${cfg}cache/`)) return true;
			return false;
		}
		// Other hidden folders (e.g. .git, .stfolder) are not part of notes.
		const segs = path.split("/");
		if (name === FOLDER_MARKER) segs.pop(); // empty-folder marker: judged by its folder
		return segs.some((seg) => seg.startsWith("."));
	};
}
