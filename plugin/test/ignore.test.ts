import { describe, expect, it } from "vitest";
import { makeIgnore } from "../src/engine/ignore";

describe("makeIgnore with excluded plugins", () => {
	const base = { configDir: ".obsidian", syncConfig: true, pluginId: "simplesync" };

	it("syncs every plugin by default", () => {
		const ig = makeIgnore(base);
		expect(ig(".obsidian/plugins/dataview/data.json")).toBe(false);
	});

	it("skips only the excluded plugins' folders", () => {
		const ig = makeIgnore({ ...base, excludedPlugins: ["dataview"] });
		expect(ig(".obsidian/plugins/dataview/data.json")).toBe(true);
		expect(ig(".obsidian/plugins/dataview/main.js")).toBe(true);
		expect(ig(".obsidian/plugins/dataview-extra/data.json")).toBe(false);
		expect(ig(".obsidian/app.json")).toBe(false);
	});

	it("syncs nothing in .obsidian when settings sync is off", () => {
		expect(makeIgnore({ ...base, syncConfig: false })(".obsidian/app.json")).toBe(true);
	});
});
