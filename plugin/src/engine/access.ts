// Folder access as the server sends it with every change list. Mirrors the
// server's rules (store/access.go): the most specific rule wins, and nothing
// is writable for a read-only member.

export type AccessLevel = "none" | "read" | "write";

export interface AccessRule {
	/** Folder or file; "" = the whole vault. */
	path: string;
	level: AccessLevel;
}

export interface VaultAccess {
	/** Changes whenever the member's access changes. */
	rev: number;
	/** Empty when nothing is limited. */
	rules: AccessRule[];
}

const fold = (p: string) => p.normalize("NFC").toLowerCase();

export class Access {
	constructor(
		private role: string,
		private rules: AccessRule[] = [],
	) {}

	get limited(): boolean {
		return this.rules.length > 0;
	}

	private level(path: string): AccessLevel {
		const f = fold(path);
		let best = -1;
		let lvl: AccessLevel = "read";
		for (const r of this.rules) {
			const rf = fold(r.path);
			if (rf && f !== rf && !f.startsWith(rf + "/")) continue;
			if (rf.length > best) {
				best = rf.length;
				lvl = r.level;
			}
		}
		if (best < 0) return this.role === "viewer" ? "read" : "write";
		return lvl;
	}

	canRead(path: string): boolean {
		return this.level(path) !== "none";
	}

	canWrite(path: string): boolean {
		return this.role !== "viewer" && this.level(path) === "write";
	}
}
