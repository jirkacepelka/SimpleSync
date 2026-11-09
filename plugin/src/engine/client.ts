import { hasText, t } from "../i18n";
import type { VaultAccess } from "./access";
import type { CommitOp, Http, HttpResponse, OpResult, RemoteEntry } from "./types";

export class ApiError extends Error {
	constructor(
		public status: number,
		public code: string,
		message: string,
	) {
		super(message);
	}
}

export interface VaultInfo {
	id: number;
	name: string;
	role: "owner" | "editor" | "viewer";
	head_rev: number;
	/** Number of files on the server (missing on servers older than 0.2.0). */
	file_count?: number;
}

export interface ChangesPage {
	changes: RemoteEntry[];
	head: number;
	more: boolean;
	role: string;
	/** Folder access of this member (missing on servers older than 0.8.0). */
	access?: VaultAccess;
}

/**
 * Normalizes what the user typed as server address. Without a scheme, local
 * addresses get http:// and everything else https://.
 */
export function normalizeServerUrl(input: string): string {
	let s = input.trim().replace(/\/+$/, "");
	if (!s) return s;
	if (!/^https?:\/\//i.test(s)) {
		const host = s.split(/[/:]/)[0];
		const local = /^(localhost|127\.|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/.test(host) || host.endsWith(".local") || host.endsWith(".lan");
		s = (local ? "http://" : "https://") + s;
	}
	return s;
}

/**
 * Hosts where a plain http:// login is acceptable: the home network, and Tailscale
 * (100.64.0.0/10, *.ts.net), whose traffic is encrypted by WireGuard.
 */
export function isLocalHost(host: string): boolean {
	host = host.toLowerCase();
	return (
		/^(localhost|127\.|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.)/.test(host) ||
		[".local", ".lan", ".home.arpa", ".ts.net"].some((d) => host.endsWith(d))
	);
}

/** True when a login to url would send the password unencrypted over the internet. */
export function insecureRemote(url: string): boolean {
	const m = /^http:\/\/([^/:]+)/i.exec(url);
	return !!m && !isLocalHost(m[1]);
}

export class Client {
	constructor(
		public baseUrl: string,
		public token: string,
		private http: Http,
	) {}

	private async req(method: string, path: string, body?: unknown, raw?: ArrayBuffer): Promise<HttpResponse> {
		const headers: Record<string, string> = {};
		if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
		let res: HttpResponse;
		try {
			res = await this.http({
				url: this.baseUrl + path,
				method,
				headers,
				body: raw ?? (body === undefined ? undefined : JSON.stringify(body)),
				contentType: raw ? "application/octet-stream" : "application/json",
			});
		} catch (e) {
			throw new ApiError(0, "network", t("err.network", { msg: e instanceof Error ? e.message : String(e) }));
		}
		if (res.status >= 400) {
			let code = "http_" + res.status;
			let msg = t("err.server", { status: res.status });
			try {
				const j = res.json as { error?: string; message?: string } | undefined;
				if (j?.error) code = j.error;
				if (j?.message) msg = j.message;
			} catch {
				/* not JSON */
			}
			// Known errors in the user's language; others as the server says.
			const key = "err." + code;
			if (hasText(key)) msg = t(key);
			throw new ApiError(res.status, code, msg);
		}
		return res;
	}

	/** Sends a request and returns its JSON body. */
	private async json<T>(method: string, path: string, body?: unknown): Promise<T> {
		return (await this.req(method, path, body)).json as T;
	}

	/** Verifies that an SimpleSync server answers at baseUrl. */
	async ping(): Promise<void> {
		let ok = false;
		try {
			ok = (await this.json<{ app?: string } | undefined>("GET", "/api/v1/ping"))?.app === "obsisync";
		} catch (e) {
			if (e instanceof ApiError && e.status === 0) throw e;
		}
		if (!ok) throw new ApiError(0, "not_obsisync", t("err.notObsisync"));
	}

	async login(username: string, password: string, deviceName: string): Promise<string> {
		const res = await this.json<{ token: string }>("POST", "/api/v1/auth/login", { username, password, device_name: deviceName });
		this.token = res.token;
		return this.token;
	}

	async logout(): Promise<void> {
		await this.req("POST", "/api/v1/auth/logout");
	}

	async vaults(): Promise<{ vaults: VaultInfo[]; can_create: boolean }> {
		return this.json("GET", "/api/v1/vaults");
	}

	async createVault(name: string): Promise<VaultInfo> {
		return this.json("POST", "/api/v1/vaults", { name });
	}

	async changes(vaultId: number, since: number): Promise<ChangesPage> {
		return this.json("GET", `/api/v1/vaults/${vaultId}/changes?since=${since}&limit=1000`);
	}

	async missing(vaultId: number, hashes: string[]): Promise<string[]> {
		return (await this.json<{ missing: string[] }>("POST", `/api/v1/vaults/${vaultId}/blobs/missing`, { hashes })).missing;
	}

	async upload(vaultId: number, hash: string, data: ArrayBuffer): Promise<void> {
		await this.req("PUT", `/api/v1/vaults/${vaultId}/blobs/${hash}`, undefined, data);
	}

	/** Downloads content by hash; null if the server no longer has it. */
	async download(vaultId: number, hash: string): Promise<ArrayBuffer | null> {
		try {
			return (await this.req("GET", `/api/v1/vaults/${vaultId}/blobs/${hash}`)).arrayBuffer;
		} catch (e) {
			if (e instanceof ApiError && e.status === 404) return null;
			throw e;
		}
	}

	async commit(vaultId: number, ops: CommitOp[]): Promise<{ results: OpResult[]; head: number }> {
		return this.json("POST", `/api/v1/vaults/${vaultId}/commit`, { ops });
	}

	wsUrl(vaultId: number): string {
		return this.baseUrl.replace(/^http/, "ws") + `/api/v1/vaults/${vaultId}/ws?token=${encodeURIComponent(this.token)}`;
	}
}
