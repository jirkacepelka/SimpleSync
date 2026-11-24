import { TFile, TFolder, type App } from "obsidian";
import { describe, expect, it, vi } from "vitest";
import { ObsidianFS } from "../src/obsidian-fs";

vi.mock("obsidian", () => ({
	normalizePath: (path: string) => path,
	TFile: class {},
	TFolder: class { children = []; },
}));

function testVault() {
	const entries = new Map<string, TFile | TFolder>();
	const vault = {
		adapter: {
			exists: async (path: string) => entries.has(path),
			stat: async () => null,
		},
		getAbstractFileByPath: (path: string) => entries.get(path) ?? null,
		createBinary: async (path: string) => entries.set(path, Object.assign(new TFile(), { path })),
		createFolder: async (path: string) => entries.set(path, Object.assign(new TFolder(), { path })),
		trash: async (file: TFile) => entries.delete(file.path),
		delete: async (folder: TFolder) => entries.delete(folder.path),
	};
	const fs = new ObsidianFS({ vault } as unknown as App, () => false);
	return { entries, vault, fs };
}

describe("ObsidianFS parent folders", () => {
	it("writes a note at the vault root without creating a folder", async () => {
		const { entries, fs } = testVault();
		await fs.write("Inbox.md", new ArrayBuffer(0), 1);
		expect([...entries.keys()]).toEqual(["Inbox.md"]);
	});

	it("keeps an existing empty sibling folder when removing a root note", async () => {
		const { entries, vault, fs } = testVault();
		await vault.createFolder("Inbox.m");
		await vault.createBinary("Inbox.md");
		await fs.remove("Inbox.md");
		expect([...entries.keys()]).toEqual(["Inbox.m"]);
	});

	it("creates and prunes the parent folder of a nested note", async () => {
		const { entries, fs } = testVault();
		await fs.write("Projects/Plan.md", new ArrayBuffer(0), 1);
		expect(entries.get("Projects")).toBeInstanceOf(TFolder);
		expect(entries.get("Projects/Plan.md")).toBeInstanceOf(TFile);
		await fs.remove("Projects/Plan.md");
		expect(entries.size).toBe(0);
	});
});
