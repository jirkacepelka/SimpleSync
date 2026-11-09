import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { Client } from "../src/engine/client";
import { MassDeleteError, SyncEngine } from "../src/engine/engine";
import { makeIgnore } from "../src/engine/ignore";
import type { SyncState } from "../src/engine/types";
import { MemFS } from "./memfs";
import { WebAdmin, fetchHttp, startServer } from "./server";

let server: Awaited<ReturnType<typeof startServer>>;

beforeAll(async () => {
	server = await startServer();
}, 120_000);
afterAll(() => server?.stop());

const ignore = makeIgnore({ configDir: ".obsidian", syncConfig: false, pluginId: "obsisync" });

class Device {
	fs = new MemFS();
	engine!: SyncEngine;
	conflicts: string[] = [];
	constructor(public name: string) {}

	client!: Client;
	async connect(vaultId: number, initialFromServer = false, user = "admin") {
		const client = new Client(server.url, "", fetchHttp);
		await client.login(user, "heslo1234", this.name);
		this.client = client;
		const state: SyncState = { vaultId, lastRev: 0, base: {}, initialFromServer };
		this.engine = new SyncEngine({
			fs: this.fs,
			client,
			state,
			saveState: async () => {},
			ignore,
			author: this.name,
			onConflict: (_p, copy) => this.conflicts.push(copy),
		});
		return this;
	}
	sync() {
		return this.engine.sync();
	}
}

let vaultSeq = 0;
async function newVault(): Promise<number> {
	const c = new Client(server.url, "", fetchHttp);
	await c.login("admin", "heslo1234", "setup");
	return (await c.createVault(`Test ${++vaultSeq}`)).id;
}

async function pair() {
	const id = await newVault();
	return [await new Device("jirka").connect(id), await new Device("petra").connect(id)] as const;
}

describe("sync engine against a real server", () => {
	it("propagates creates, edits, renames and deletes", async () => {
		const [a, b] = await pair();
		a.fs.set("Deník/Dnes.md", "ahoj");
		a.fs.set("obrázek.png", "\u0000binární");
		await a.sync();
		await b.sync();
		expect(b.fs.snapshot()).toEqual(a.fs.snapshot());

		b.fs.set("Deník/Dnes.md", "ahoj světe");
		await b.sync();
		await a.sync();
		expect(a.fs.get("Deník/Dnes.md")).toBe("ahoj světe");

		// Rename = delete + create; content is not uploaded again.
		a.fs.files.set("Archiv/Dnes.md", a.fs.files.get("Deník/Dnes.md")!);
		a.fs.del("Deník/Dnes.md");
		await a.sync();
		await b.sync();
		expect(b.fs.snapshot()).toEqual({ "Archiv/Dnes.md": "ahoj světe", "obrázek.png": "\u0000binární" });

		b.fs.del("obrázek.png");
		await b.sync();
		await a.sync();
		expect(a.fs.get("obrázek.png")).toBeUndefined();
	});

	it("syncs empty folders and their removal", async () => {
		const [a, b] = await pair();
		a.fs.mkdir("Prázdná/Vnořená");
		await a.sync();
		await b.sync();
		expect(b.fs.folders.has("Prázdná/Vnořená")).toBe(true);

		// Filling the folder on b replaces the marker with a real note.
		b.fs.set("Prázdná/Vnořená/Nota.md", "obsah");
		await b.sync();
		await a.sync();
		expect(a.fs.get("Prázdná/Vnořená/Nota.md")).toBe("obsah");

		// Removing an empty folder removes it elsewhere.
		a.fs.mkdir("Dočasná");
		await a.sync();
		await b.sync();
		expect(b.fs.folders.has("Dočasná")).toBe(true);
		a.fs.folders.delete("Dočasná");
		await a.sync();
		await b.sync();
		expect(b.fs.folders.has("Dočasná")).toBe(false);
	});

	it("merges concurrent edits of different lines", async () => {
		const [a, b] = await pair();
		a.fs.set("n.md", "jedna\ndva\ntři\n");
		await a.sync();
		await b.sync();
		a.fs.set("n.md", "JEDNA\ndva\ntři\n");
		b.fs.set("n.md", "jedna\ndva\nTŘI\n");
		await a.sync();
		await b.sync(); // merges and pushes
		await a.sync();
		expect(a.fs.get("n.md")).toBe("JEDNA\ndva\nTŘI\n");
		expect(b.fs.get("n.md")).toBe("JEDNA\ndva\nTŘI\n");
		expect(a.conflicts.length + b.conflicts.length).toBe(0);
	});

	it("keeps both versions on a real conflict", async () => {
		const [a, b] = await pair();
		a.fs.set("n.md", "původní\n");
		await a.sync();
		await b.sync();
		a.fs.set("n.md", "verze A\n");
		b.fs.set("n.md", "verze B\n");
		await a.sync();
		const res = await b.sync();
		expect(res.conflicts).toBe(1);
		expect(b.fs.get("n.md")).toBe("verze A\n");
		const copy = b.conflicts[0];
		expect(copy).toMatch(/^n \(conflict .* petra\)\.md$/);
		expect(b.fs.get(copy)).toBe("verze B\n");
		await a.sync();
		expect(a.fs.snapshot()).toEqual(b.fs.snapshot());
	});

	it("edit beats delete in both directions", async () => {
		const [a, b] = await pair();
		a.fs.set("x.md", "x");
		a.fs.set("y.md", "y");
		await a.sync();
		await b.sync();
		// x: deleted on A, edited on B. y: edited on A, deleted on B.
		a.fs.del("x.md");
		a.fs.set("y.md", "y2");
		b.fs.set("x.md", "x2");
		b.fs.del("y.md");
		await a.sync();
		await b.sync();
		await a.sync();
		expect(a.fs.snapshot()).toEqual({ "x.md": "x2", "y.md": "y2" });
		expect(b.fs.snapshot()).toEqual({ "x.md": "x2", "y.md": "y2" });
	});

	it("initial merge of two non-empty vaults and identical files", async () => {
		const id = await newVault();
		const a = await new Device("A").connect(id);
		const b = await new Device("B").connect(id);
		a.fs.set("same.md", "stejné");
		b.fs.set("same.md", "stejné");
		a.fs.set("only-a.md", "a");
		b.fs.set("only-b.md", "b");
		await a.sync();
		await b.sync();
		await a.sync();
		expect(a.fs.snapshot()).toEqual({ "only-a.md": "a", "only-b.md": "b", "same.md": "stejné" });
		expect(b.fs.snapshot()).toEqual(a.fs.snapshot());
		expect(b.conflicts).toEqual([]);
	});

	it("normalizes NFD file names (macOS/iOS) to NFC", async () => {
		const [a, b] = await pair();
		a.fs.set("Poznámka.md".normalize("NFD"), "diakritika");
		await a.sync();
		await b.sync();
		expect(b.fs.get("Poznámka.md".normalize("NFC"))).toBe("diakritika");
		// Editing on the NFC side updates the NFD file on A, no duplicate.
		b.fs.set("Poznámka.md", "upraveno");
		await b.sync();
		await a.sync();
		expect(a.fs.snapshot()).toEqual({ ["Poznámka.md".normalize("NFD")]: "upraveno" });
	});

	it("ignores the trash, workspace and the plugin's own folder", async () => {
		const [a, b] = await pair();
		a.fs.set(".trash/old.md", "x");
		a.fs.set(".obsidian/workspace.json", "{}");
		a.fs.set(".obsidian/plugins/obsisync/data.json", "{\"token\":\"secret\"}");
		a.fs.set("note.md", "n");
		await a.sync();
		await b.sync();
		expect(b.fs.snapshot()).toEqual({ "note.md": "n" });
	});

	it("refuses to wipe the server when the local vault suddenly looks empty", async () => {
		const [a] = await pair();
		for (let i = 0; i < 30; i++) a.fs.set(`n${i}.md`, String(i));
		await a.sync();
		a.fs.files.clear();
		await expect(a.sync()).rejects.toBeInstanceOf(MassDeleteError);
		a.engine.allowMassDelete = true;
		const res = await a.sync();
		expect(res.pushed).toBe(30);
	});

	it("an empty device joining a full vault downloads everything and changes nothing on the server", async () => {
		const id = await newVault();
		const a = await new Device("A").connect(id);
		a.fs.set("n.md", "server");
		a.fs.set("dir/x.md", "x");
		await a.sync();
		const head = (await a.client.changes(id, 0)).head;

		const b = await new Device("B").connect(id, true);
		const r = await b.sync();
		expect(b.fs.snapshot()).toEqual(a.fs.snapshot());
		expect(r.pushed).toBe(0);
		expect((await b.client.changes(id, 0)).head).toBe(head);
	});

	it("joining a full vault replaces local content with the server's and uploads nothing", async () => {
		const id = await newVault();
		const a = await new Device("A").connect(id);
		a.fs.set("n.md", "server");
		a.fs.set("same.md", "same");
		await a.sync();
		const head = (await a.client.changes(id, 0)).head;

		const b = await new Device("B").connect(id, true);
		b.fs.set("Welcome.md", "default note");
		b.fs.set("n.md", "local version");
		b.fs.set("same.md", "same");
		const r = await b.sync();
		expect(b.fs.notes()).toEqual({ "n.md": "server", "same.md": "same" });
		expect(b.fs.get(".trash/Welcome.md")).toBe("default note");
		expect(b.fs.get(".trash/n.md")).toBe("local version");
		expect(r.trashed).toBe(2);
		expect((await b.client.changes(id, 0)).head).toBe(head);
		expect(b.engine.state.initialFromServer).toBe(false);

		// Afterwards the device syncs normally.
		b.fs.set("n.md", "edited on B");
		await b.sync();
		await a.sync();
		expect(a.fs.get("n.md")).toBe("edited on B");
	});

	it("a vault downloaded for Obsidian connects without trashing or uploading anything", async () => {
		const id = await newVault();
		const a = await new Device("A").connect(id);
		a.fs.set("Deník/Dnes.md", "# Ahoj");
		a.fs.set("obr.png", "PNG");
		await a.sync();
		const head = (await a.client.changes(id, 0)).head;

		// The ZIP holds exactly the server's files; one note changed on the
		// server after the download.
		const b = await new Device("B").connect(id, true);
		b.fs.set("Deník/Dnes.md", "# Ahoj");
		b.fs.set("obr.png", "PNG");
		a.fs.set("obr.png", "PNG v2");
		await a.sync();
		const r = await b.sync();
		expect(b.fs.notes()).toEqual({ "Deník/Dnes.md": "# Ahoj", "obr.png": "PNG v2" });
		expect(r.pushed).toBe(0);
		expect(b.fs.get(".trash/Deník/Dnes.md")).toBeUndefined();
		expect((await b.client.changes(id, 0)).head).toBe(head + 1);
	});

	it("an interrupted first connect stays in server-first mode", async () => {
		const id = await newVault();
		const a = await new Device("A").connect(id);
		a.fs.set("n.md", "server");
		await a.sync();

		const b = await new Device("B").connect(id, true);
		b.fs.set("n.md", "local");
		const realChanges = b.client.changes.bind(b.client);
		b.client.changes = async () => {
			throw new Error("network down");
		};
		await expect(b.sync()).rejects.toThrow("network down");
		expect(b.engine.state.initialFromServer).toBe(true);
		b.client.changes = realChanges;
		await b.sync();
		expect(b.fs.get("n.md")).toBe("server");
		expect(b.fs.get(".trash/n.md")).toBe("local");
	});

	it("connecting to an empty server vault uploads the local vault", async () => {
		const id = await newVault();
		const b = await new Device("B").connect(id, false);
		b.fs.set("mine.md", "moje");
		const r = await b.sync();
		expect(r.pushed).toBe(1);
		const c = await new Device("C").connect(id, true);
		await c.sync();
		expect(c.fs.notes()).toEqual({ "mine.md": "moje" });
	});

	it("paginates large change feeds", async () => {
		const [a, b] = await pair();
		for (let i = 0; i < 1234; i++) a.fs.set(`bulk/${i}.md`, `soubor ${i}`);
		const r = await a.sync();
		expect(r.pushed).toBe(1234);
		const r2 = await b.sync();
		expect(r2.pulled).toBe(1234);
		expect(Object.keys(b.fs.snapshot()).length).toBe(1234);
	}, 60_000);
});

describe("folder access", () => {
	it("syncs only the folders a member may see and uploads only where they may write", async () => {
		const web = await new WebAdmin(server.url).login("admin", "heslo1234");
		await web.post("/users", { username: "eva", password: "heslo1234" });
		const id = await newVault();
		const admin = await new Device("admin").connect(id);
		admin.fs.set("Tým/a.md", "a");
		admin.fs.set("Tajné/b.md", "tajné");
		admin.fs.set("Čtení/c.md", "c");
		await admin.sync();

		const vp = `/vaults/${id}`;
		await web.post(`${vp}/members`, { username: "eva", role: "editor" });
		await web.post(`${vp}/folder-access`, { folder_access: "1" });
		const evaID = /id="member-(\d+)">\s*<td>eva<\/td>/.exec(await web.get(`${vp}/members`))![1];
		const rule = (path: string, level: string) => web.post(`${vp}/rules`, { user_id: evaID, back: "members", path, level });
		await rule("Tajné", "none");
		await rule("Čtení", "read");

		const eva = await new Device("eva").connect(id, false, "eva");
		await eva.sync();
		expect(eva.fs.notes()).toEqual({ "Tým/a.md": "a", "Čtení/c.md": "c" });

		// An edit in a read-only folder stays here; one in a writable folder goes up.
		eva.fs.set("Čtení/c.md", "c upraveno");
		eva.fs.set("Tým/a.md", "a upraveno");
		const r = await eva.sync();
		expect(r.pushed).toBe(1);
		expect(r.notAllowed).toBe(1);
		await admin.sync();
		expect(admin.fs.get("Tým/a.md")).toBe("a upraveno");
		expect(admin.fs.get("Čtení/c.md")).toBe("c");

		// Access to a folder taken away: its files go to the local trash.
		await rule("Tým", "none");
		const r2 = await eva.sync();
		expect(r2.hidden).toBe(1);
		expect(eva.fs.notes()).toEqual({ "Čtení/c.md": "c upraveno" });
		expect(eva.fs.get(".trash/Tým/a.md")).toBe("a upraveno");

		// Access given: the folder arrives without anything changing on the server.
		await rule("Tajné", "read");
		await eva.sync();
		expect(eva.fs.get("Tajné/b.md")).toBe("tajné");

		// Switched off: everything is visible again, and the edit kept here
		// can now be uploaded.
		await web.post(`${vp}/folder-access`, {});
		const r3 = await eva.sync();
		expect(r3.notAllowed).toBe(0);
		expect(eva.fs.get("Tým/a.md")).toBe("a upraveno");
		await admin.sync();
		expect(admin.fs.get("Čtení/c.md")).toBe("c upraveno");
	});
});
