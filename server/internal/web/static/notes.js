// SimpleSync web editor. Saves go through the same compare-and-swap commit
// the Obsidian plugin uses, so edits reach every device within seconds.
(() => {
  "use strict";
  const root = document.getElementById("editor");
  if (!root) return;
  const cfg = JSON.parse(document.getElementById("ed-config").textContent);
  const t = (k, ...a) => {
    let s = cfg.t[k.replace(/^editor\./, "")] || k;
    for (const v of a) s = s.replace(/%[sdv]/, v);
    return s;
  };
  const base = `/vaults/${cfg.vault}`;
  const $ = (id) => document.getElementById(id);
  const el = {
    tree: $("ed-tree"), filter: $("ed-filter"), title: $("ed-title"), crumb: $("ed-crumb"), status: $("ed-status"),
    mode: $("ed-mode"), toolbar: $("ed-toolbar"), panes: $("ed-panes"), src: $("ed-src"), preview: $("ed-preview"),
    binary: $("ed-binary"), folder: $("ed-folder"), empty: $("ed-empty"), notice: $("ed-notice"), pub: $("ed-pub"), view: $("ed-view"),
    history: $("ed-history"), file: $("ed-file"), dialog: $("ed-dialog"),
  };
  const store = {
    get(k, d) { try { const v = localStorage.getItem("ss." + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
    set(k, v) { try { localStorage.setItem("ss." + k, JSON.stringify(v)); } catch { /* private mode */ } },
  };

  let files = [];          // [{path, hash, size, mtime}]
  let rev = 0;             // vault revision the tree reflects
  let note = null;         // {path, base, binary}
  let dirty = false, saving = false, again = false, offline = false;
  let saveTimer = 0, renderTimer = 0;
  let openDirs = new Set(store.get(`open.${cfg.vault}`, []));
  let selectedDir = "";

  // ---------- server calls ----------
  class HttpError extends Error {}
  async function call(method, url, body, form) {
    const opts = { method, headers: { "X-CSRF-Token": cfg.csrf }, credentials: "same-origin" };
    if (form) opts.body = form;
    else if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetch(url, opts);
    } catch (e) {
      const err = new HttpError(t("editor.offline"));
      err.network = true;
      throw err;
    }
    if (!(res.headers.get("Content-Type") || "").includes("application/json")) {
      const err = new HttpError(t("editor.sessionExpired"));
      err.session = true;
      throw err;
    }
    const data = await res.json();
    if (!res.ok) {
      const err = new HttpError(data.message || String(res.status));
      err.code = data.error;
      err.status = res.status;
      throw err;
    }
    return data;
  }

  // ---------- helpers ----------
  const isNote = (p) => /\.md$/i.test(p);
  const isText = (p) => /\.(md|txt|canvas|json|css|js|csv|ya?ml|html|xml|svg|base)$/i.test(p);
  const dirOf = (p) => (p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "");
  const baseName = (p) => p.slice(p.lastIndexOf("/") + 1);
  const stem = (p) => baseName(p).replace(/\.md$/i, "");
  const join = (d, n) => (d ? `${d}/${n}` : n);
  const escapeHTML = (s) => s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const publicURL = (p) => `/p/${cfg.publish.slug}/` + p.replace(/\.md$/i, "").split("/").map(encodeURIComponent).join("/");
  const icon = (name, cls = "") => `<svg class="icon ${cls}" aria-hidden="true"><use href="#i-${name}"/></svg>`;

  function setStatus(state) {
    el.status.hidden = !note || note.binary;
    el.status.dataset.state = state;
    el.status.textContent = {
      saved: t("editor.saved"), saving: t("editor.saving"), dirty: t("editor.unsaved"),
      error: offline ? t("editor.offline") : t("editor.saveFailed"), readonly: t("editor.readOnly"),
    }[state];
  }

  function notice(msg, kind = "") {
    el.notice.hidden = !msg;
    el.notice.className = "ed-notice " + kind;
    el.notice.querySelector("span").textContent = msg || "";
  }
  el.notice.querySelector("button").addEventListener("click", () => notice(""));

  // A small in-page dialog instead of prompt()/confirm().
  function ask(message, value, { input = true, danger = false } = {}) {
    return new Promise((resolve) => {
      $("ed-dialog-msg").textContent = message;
      const inp = $("ed-dialog-input");
      inp.hidden = !input;
      inp.value = value || "";
      $("ed-dialog-ok").className = danger ? "danger" : "primary";
      el.dialog.returnValue = "";
      el.dialog.showModal();
      if (input) {
        inp.focus();
        inp.select();
      }
      el.dialog.addEventListener("close", function done() {
        el.dialog.removeEventListener("close", done);
        if (el.dialog.returnValue !== "ok") return resolve(null);
        resolve(input ? inp.value.trim() : true);
      });
    });
  }

  // ---------- file tree ----------
  function buildTree(list) {
    const rootNode = { dirs: new Map(), files: [] };
    for (const f of list) {
      const parts = f.path.split("/");
      let n = rootNode, acc = "";
      for (const part of parts.slice(0, -1)) {
        acc = join(acc, part);
        if (!n.dirs.has(part)) n.dirs.set(part, { path: acc, dirs: new Map(), files: [] });
        n = n.dirs.get(part);
      }
      n.files.push(f);
    }
    return rootNode;
  }

  function renderTree() {
    const q = el.filter.value.trim().toLowerCase();
    if (q) {
      const hits = files.filter((f) => f.path.toLowerCase().includes(q)).slice(0, 200);
      el.tree.innerHTML = hits.length
        ? "<ul>" + hits.map((f) => fileItem(f, true)).join("") + "</ul>"
        : `<div class="ed-tree-empty">—</div>`;
      return;
    }
    if (!files.length) {
      el.tree.innerHTML = `<div class="ed-tree-empty">${escapeHTML(t("editor.emptyHint"))}</div>`;
      return;
    }
    const walk = (n) => {
      const dirs = [...n.dirs.values()].sort((a, b) => a.path.localeCompare(b.path, undefined, { sensitivity: "base" }));
      const fs = n.files.slice().sort((a, b) => baseName(a.path).localeCompare(baseName(b.path), undefined, { sensitivity: "base", numeric: true }));
      let h = "<ul>";
      for (const d of dirs) {
        const open = openDirs.has(d.path);
        h += `<li><div class="ed-item dir${open ? " open" : ""}${selectedDir === d.path && !note ? " on" : ""}" data-dir="${escapeHTML(d.path)}" tabindex="0">`
          + icon("chevron", "chev") + `<span class="name">${escapeHTML(baseName(d.path))}</span>`
          + (cfg.editable ? `<button class="more" type="button" data-more aria-label="⋯">${icon("more")}</button>` : "")
          + `</div>${open ? walk(d) : ""}</li>`;
      }
      for (const f of fs) h += fileItem(f, false);
      return h + "</ul>";
    };
    el.tree.innerHTML = walk(buildTree(files));
  }

  function fileItem(f, withDir) {
    const ext = isNote(f.path) ? "" : (f.path.match(/\.([^./]+)$/) || [, ""])[1];
    const on = note && note.path === f.path ? " on" : "";
    const label = isNote(f.path) ? stem(f.path) : baseName(f.path);
    return `<li><a class="ed-item file${on}" href="?path=${encodeURIComponent(f.path)}" data-path="${escapeHTML(f.path)}">`
      + icon(isNote(f.path) ? "note" : /\.(png|jpe?g|gif|webp|svg|avif|bmp)$/i.test(f.path) ? "image" : "file")
      + `<span class="name">${escapeHTML(label)}${withDir && dirOf(f.path) ? `<span class="hit">${escapeHTML(dirOf(f.path))}</span>` : ""}</span>`
      + (ext ? `<span class="ext">${escapeHTML(ext)}</span>` : "")
      + (cfg.editable ? `<button class="more" type="button" data-more aria-label="⋯">${icon("more")}</button>` : "")
      + "</a></li>";
  }

  el.tree.addEventListener("click", (e) => {
    const item = e.target.closest(".ed-item");
    if (!item) return;
    if (e.target.closest("[data-more]")) {
      e.preventDefault();
      e.stopPropagation();
      const r = e.target.closest("[data-more]").getBoundingClientRect();
      menu(item, r.left, r.bottom + 4);
      return;
    }
    if (item.dataset.dir !== undefined) {
      const d = item.dataset.dir;
      openDirs.has(d) ? openDirs.delete(d) : openDirs.add(d);
      store.set(`open.${cfg.vault}`, [...openDirs]);
      showFolder(d);
      return;
    }
    if (e.metaKey || e.ctrlKey || e.shiftKey) return;
    e.preventDefault();
    open(item.dataset.path);
    root.classList.remove("show-side");
  });
  el.tree.addEventListener("contextmenu", (e) => {
    const item = e.target.closest(".ed-item");
    if (!item || !cfg.editable) return;
    e.preventDefault();
    menu(item, e.clientX, e.clientY);
  });
  el.tree.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && e.target.dataset.dir !== undefined) e.target.click();
  });
  el.filter.addEventListener("input", renderTree);

  let menuEl = null;
  function closeMenu() {
    if (menuEl) menuEl.remove();
    menuEl = null;
  }
  document.addEventListener("click", closeMenu);
  document.addEventListener("keydown", (e) => e.key === "Escape" && closeMenu());

  function menu(item, x, y) {
    closeMenu();
    const isDir = item.dataset.dir !== undefined;
    const p = isDir ? item.dataset.dir : item.dataset.path;
    menuEl = document.createElement("div");
    menuEl.className = "ed-menu";
    const add = (label, fn, cls = "") => {
      const b = document.createElement("button");
      b.type = "button";
      b.className = cls;
      b.textContent = label;
      b.addEventListener("click", (e) => {
        e.stopPropagation();
        closeMenu();
        fn();
      });
      menuEl.append(b);
    };
    if (isDir) add(t("editor.newNote"), () => newNote(p));
    add(t("editor.rename"), () => rename(p, isDir));
    add(t("editor.delete"), () => remove(p, isDir), "danger");
    document.body.append(menuEl);
    const r = menuEl.getBoundingClientRect();
    menuEl.style.left = Math.min(x, innerWidth - r.width - 8) + "px";
    menuEl.style.top = Math.min(y, innerHeight - r.height - 8) + "px";
  }

  async function loadTree(force) {
    const data = await call("GET", `${base}/api/tree` + (force ? "" : `?rev=${rev}`));
    if (data.same) return false;
    files = data.files;
    rev = data.rev;
    renderTree();
    return true;
  }

  // ---------- opening notes ----------
  function show(what) {
    el.panes.hidden = what !== "text";
    el.toolbar.hidden = what !== "text" || !cfg.editable;
    el.mode.hidden = what !== "text";
    el.binary.hidden = what !== "binary";
    el.folder.hidden = what !== "folder";
    el.empty.hidden = what !== "empty";
    el.title.hidden = what === "empty" || what === "folder";
    el.history.hidden = what === "empty" || what === "folder";
  }

  // Overview of a folder: its subfolders and files, shown in the main pane.
  async function showFolder(d) {
    if (note && dirty) await save();
    note = null;
    dirty = false;
    selectedDir = d;
    history.replaceState(null, "", location.pathname);
    document.title = `${baseName(d)} · ${cfg.vaultName || "SimpleSync"}`;
    el.crumb.textContent = dirOf(d) ? dirOf(d) + " /" : "";
    el.status.hidden = true;
    el.pub.hidden = el.view.hidden = true;
    show("folder");
    const dirs = new Set(), fs = [];
    for (const f of files) {
      if (!f.path.startsWith(d + "/")) continue;
      const rest = f.path.slice(d.length + 1);
      const i = rest.indexOf("/");
      if (i >= 0) dirs.add(rest.slice(0, i));
      else fs.push(f);
    }
    const byName = (a, b) => a.localeCompare(b, undefined, { sensitivity: "base", numeric: true });
    let h = `<h2>${escapeHTML(baseName(d))}</h2>`;
    if (!dirs.size && !fs.length) h += `<div class="ed-tree-empty">—</div>`;
    else {
      h += "<ul>";
      for (const n of [...dirs].sort(byName)) {
        h += `<li><div class="ed-item dir" data-dir="${escapeHTML(join(d, n))}" tabindex="0">${icon("chevron", "chev")}<span class="name">${escapeHTML(n)}</span></div></li>`;
      }
      for (const f of fs.sort((a, b) => byName(baseName(a.path), baseName(b.path)))) h += fileItem(f, false);
      h += "</ul>";
    }
    el.folder.innerHTML = h;
    renderTree();
  }
  el.folder.addEventListener("click", (e) => {
    const item = e.target.closest(".ed-item");
    if (!item) return;
    if (item.dataset.dir !== undefined) {
      openDirs.add(item.dataset.dir);
      store.set(`open.${cfg.vault}`, [...openDirs]);
      return showFolder(item.dataset.dir);
    }
    if (e.metaKey || e.ctrlKey || e.shiftKey) return;
    e.preventDefault();
    open(item.dataset.path);
  });

  async function open(p, { keepNotice = false } = {}) {
    if (note && dirty) await save();
    let data;
    try {
      data = await call("GET", `${base}/api/note?path=${encodeURIComponent(p)}`);
    } catch (e) {
      if (e.status === 404) {
        history.replaceState(null, "", location.pathname);
        return showEmpty();
      }
      return notice(e.message);
    }
    if (!keepNotice) notice("");
    note = { path: data.path, base: data.hash, binary: !!data.binary };
    dirty = false;
    selectedDir = dirOf(data.path);
    for (let d = selectedDir; d; d = dirOf(d)) openDirs.add(d);
    history.replaceState(null, "", `?path=${encodeURIComponent(data.path)}`);
    document.title = `${stem(data.path)} · ${cfg.vaultName || "SimpleSync"}`;
    el.crumb.textContent = dirOf(data.path) ? dirOf(data.path) + " /" : "";
    el.title.value = isNote(data.path) ? stem(data.path) : baseName(data.path);
    el.title.readOnly = !cfg.editable;
    el.history.href = `${base}/file?path=${encodeURIComponent(data.path)}`;
    if (data.binary) {
      show("binary");
      el.binary.innerHTML = data.image
        ? `<img src="${base}/raw?inline=1&path=${encodeURIComponent(data.path)}" alt="">`
        : `<p>${escapeHTML(t("editor.binary"))}</p><p><a class="button" href="${base}/raw?path=${encodeURIComponent(data.path)}">${icon("download")} ${escapeHTML(baseName(data.path))}</a></p>`;
      updatePublish();
      setStatus("saved");
      renderTree();
      return;
    }
    show("text");
    el.src.value = data.text;
    el.src.readOnly = !cfg.editable;
    el.src.classList.toggle("mono", !isNote(data.path));
    el.src.scrollTop = 0;
    setMode(cfg.editable ? store.get("mode", innerWidth > 1000 ? "split" : "edit") : "read");
    setStatus(cfg.editable ? "saved" : "readonly");
    renderPreview(true);
    updatePublish();
    renderTree();
  }

  function showEmpty() {
    note = null;
    dirty = false;
    show("empty");
    el.crumb.textContent = "";
    el.status.hidden = true;
    el.pub.hidden = el.view.hidden = true;
    renderTree();
  }

  // ---------- modes & preview ----------
  function setMode(m) {
    if (!cfg.editable) m = "read";
    el.panes.dataset.mode = m;
    for (const b of el.mode.querySelectorAll("button")) b.classList.toggle("on", b.dataset.mode === m);
    if (m !== "edit") renderPreview(true);
  }
  el.mode.addEventListener("click", (e) => {
    const b = e.target.closest("[data-mode]");
    if (!b) return;
    store.set("mode", b.dataset.mode);
    setMode(b.dataset.mode);
    if (b.dataset.mode !== "read") el.src.focus();
  });

  async function renderPreview(now) {
    clearTimeout(renderTimer);
    if (!note || note.binary || el.panes.dataset.mode === "edit") return;
    if (!now) {
      renderTimer = setTimeout(() => renderPreview(true), 250);
      return;
    }
    if (!isNote(note.path)) {
      el.preview.innerHTML = `<pre><code>${escapeHTML(el.src.value)}</code></pre>`;
      return;
    }
    try {
      const data = await call("POST", `${base}/api/render`, { path: note.path, text: el.src.value });
      el.preview.innerHTML = data.html;
    } catch { /* keep the last preview */ }
  }

  // Internal links in the preview open inside the editor.
  el.preview.addEventListener("click", (e) => {
    const a = e.target.closest("a[href]");
    if (!a || e.metaKey || e.ctrlKey) return;
    const u = new URL(a.href, location.href);
    if (u.origin === location.origin && u.pathname === location.pathname && u.searchParams.get("path")) {
      e.preventDefault();
      open(u.searchParams.get("path"));
    }
  });

  // ---------- saving ----------
  function changed() {
    if (!note || !cfg.editable) return;
    dirty = true;
    setStatus("dirty");
    clearTimeout(saveTimer);
    saveTimer = setTimeout(save, 900);
    renderPreview(false);
    updatePublish();
  }
  el.src.addEventListener("input", changed);

  async function save() {
    clearTimeout(saveTimer);
    if (!note || !dirty || note.binary || !cfg.editable) return;
    if (saving) {
      again = true;
      return;
    }
    saving = true;
    again = false;
    const p = note.path, sent = el.src.value;
    dirty = false;
    setStatus("saving");
    try {
      const r = await call("POST", `${base}/api/save`, { path: p, base: note.base, text: sent });
      offline = false;
      if (note.path !== p) return;
      if (r.conflict) {
        // Overlapping edits: the server version stays, ours was kept as a copy.
        replaceText(r.text ?? "");
        note.base = r.hash;
        dirty = false;
        notice(t("editor.conflictCopy", stem(r.copy)));
        loadTree(true);
      } else if (r.merged) {
        if (el.src.value === sent) {
          replaceText(r.text);
          note.base = r.hash;
          notice(t("editor.merged"), "info");
        } else {
          // Typed during the save: merge again against what we sent.
          note.base = r.sent || r.hash;
          dirty = true;
        }
      } else {
        note.base = r.hash;
      }
      if (!files.some((f) => f.path === p)) loadTree(true);
    } catch (e) {
      dirty = true;
      offline = !!e.network;
      setStatus("error");
      if (e.session) notice(t("editor.sessionExpired"));
      else if (!e.network) notice(e.message);
      saveTimer = setTimeout(save, 5000);
      return;
    } finally {
      saving = false;
    }
    if (dirty || again) return save();
    setStatus("saved");
  }

  // Replace the whole text but keep the caret roughly where it was.
  function replaceText(text) {
    if (el.src.value === text) return;
    const { selectionStart: s, selectionEnd: e, scrollTop } = el.src;
    el.src.value = text;
    el.src.setSelectionRange(Math.min(s, text.length), Math.min(e, text.length));
    el.src.scrollTop = scrollTop;
    renderPreview(true);
    updatePublish();
  }

  addEventListener("beforeunload", (e) => {
    if (!dirty || !note) return;
    const body = JSON.stringify({ path: note.path, base: note.base, text: el.src.value });
    if (body.length < 60000) {
      fetch(`${base}/api/save`, { method: "POST", keepalive: true, credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": cfg.csrf }, body });
    }
    e.preventDefault();
    e.returnValue = "";
  });

  // ---------- live updates from other devices ----------
  async function poll() {
    if (document.hidden) return;
    try {
      const changedTree = await loadTree(false);
      if (offline) {
        offline = false;
        if (dirty) save();
      }
      if (!changedTree || !note || saving) return;
      const f = files.find((x) => x.path === note.path);
      if (!f) {
        notice(t("editor.deletedElsewhere"));
        if (cfg.editable) dirty = true;
        return;
      }
      if (f.hash !== note.base && !dirty && !note.binary) {
        const data = await call("GET", `${base}/api/note?path=${encodeURIComponent(note.path)}`);
        if (dirty || saving || note.path !== data.path) return;
        note.base = data.hash;
        replaceText(data.text ?? "");
        notice(t("editor.changedElsewhere"), "info");
        setTimeout(() => notice(""), 2500);
      }
    } catch (e) {
      if (e.network) {
        offline = true;
        if (dirty) setStatus("error");
      }
    }
  }
  setInterval(poll, 4000);
  document.addEventListener("visibilitychange", () => !document.hidden && poll());

  // ---------- rename, create, delete ----------
  el.title.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      el.src.focus();
    }
    if (e.key === "Escape") {
      el.title.value = isNote(note.path) ? stem(note.path) : baseName(note.path);
      el.src.focus();
    }
  });
  el.title.addEventListener("change", async () => {
    if (!note || !cfg.editable) return;
    const name = el.title.value.trim().replace(/[\\/]/g, "-");
    const current = isNote(note.path) ? stem(note.path) : baseName(note.path);
    if (!name || name === current) {
      el.title.value = current;
      return;
    }
    const to = join(dirOf(note.path), isNote(note.path) ? name + ".md" : name);
    await moveTo(note.path, to, false);
  });

  async function moveTo(from, to, folder) {
    if (note && dirty) await save();
    try {
      const r = await call("POST", `${base}/api/rename`, { from, to, folder });
      if (folder) {
        for (const d of [...openDirs]) if (d === from || d.startsWith(from + "/")) {
          openDirs.delete(d);
          openDirs.add(to + d.slice(from.length));
        }
      }
      await loadTree(true);
      if (note && (note.path === from || (folder && note.path.startsWith(from + "/")))) {
        await open(folder ? to + note.path.slice(from.length) : r.path);
      }
    } catch (e) {
      notice(e.message);
      if (note) el.title.value = isNote(note.path) ? stem(note.path) : baseName(note.path);
    }
  }

  async function rename(p, folder) {
    const current = folder ? baseName(p) : isNote(p) ? stem(p) : baseName(p);
    const name = await ask(t("editor.namePrompt"), current);
    if (!name || name === current) return;
    const clean = name.replace(/[\\/]/g, "-");
    await moveTo(p, join(dirOf(p), folder || !isNote(p) ? clean : clean + ".md"), folder);
  }

  async function remove(p, folder) {
    const ok = await ask(folder ? t("editor.deleteFolderConfirm", baseName(p)) : t("editor.deleteConfirm", baseName(p)), "", { input: false, danger: true });
    if (!ok) return;
    try {
      await call("POST", `${base}/api/delete`, { path: p, folder });
      if (note && (note.path === p || (folder && note.path.startsWith(p + "/")))) {
        dirty = false;
        history.replaceState(null, "", location.pathname);
        showEmpty();
      }
      await loadTree(true);
    } catch (e) {
      notice(e.message);
    }
  }

  async function newNote(dir) {
    if (dir === undefined) dir = note ? dirOf(note.path) : selectedDir;
    let name = await ask(t("editor.namePrompt"), t("editor.untitled"));
    if (!name) return;
    name = name.replace(/[\\/]/g, "-");
    if (!/\.[a-z0-9]{1,5}$/i.test(name)) name += ".md";
    await createNote(join(dir, name), "");
  }

  async function createNote(p, text) {
    try {
      const r = await call("POST", `${base}/api/save`, { path: p, base: "", text, create: true });
      if (dirOf(p)) openDirs.add(dirOf(p));
      await loadTree(true);
      await open(r.path);
      if (el.panes.dataset.mode === "read") setMode("edit");
      el.src.focus();
    } catch (e) {
      notice(e.message);
    }
  }

  async function newFolder() {
    const parent = note ? dirOf(note.path) : selectedDir;
    const name = await ask(t("editor.folderPrompt"), "");
    if (!name) return;
    const folder = join(parent, name.replace(/[\\/]/g, "-"));
    openDirs.add(folder);
    await createNote(join(folder, t("editor.untitled") + ".md"), "");
  }

  root.addEventListener("click", (e) => {
    const b = e.target.closest("[data-act]");
    if (!b) return;
    const act = b.dataset.act;
    if (act === "new-note") newNote();
    if (act === "new-folder") newFolder();
    if (act === "toggle-side") {
      if (innerWidth <= 800) root.classList.toggle("show-side");
      else {
        root.classList.toggle("no-side");
        store.set("noSide", root.classList.contains("no-side"));
      }
    }
  });

  // ---------- formatting ----------
  function insert(text, selStart, selEnd) {
    el.src.focus();
    const ok = document.execCommand && document.execCommand("insertText", false, text);
    if (!ok) el.src.setRangeText(text, el.src.selectionStart, el.src.selectionEnd, "end");
    if (selStart !== undefined) el.src.setSelectionRange(selStart, selEnd ?? selStart);
    changed();
  }

  function wrap(before, after = before, placeholder = "") {
    const { selectionStart: s, selectionEnd: e, value } = el.src;
    const sel = value.slice(s, e) || placeholder;
    insert(before + sel + after, s + before.length, s + before.length + sel.length);
  }

  // Applies fn to every line touched by the selection.
  function lines(fn) {
    const { selectionStart: s, selectionEnd: e, value } = el.src;
    const start = value.lastIndexOf("\n", s - 1) + 1;
    let end = value.indexOf("\n", e);
    if (end < 0) end = value.length;
    const out = value.slice(start, end).split("\n").map(fn).join("\n");
    el.src.setSelectionRange(start, end);
    insert(out, start, start + out.length);
  }

  function togglePrefix(prefix, re) {
    let allHave = true;
    const { selectionStart: s, selectionEnd: e, value } = el.src;
    const block = value.slice(value.lastIndexOf("\n", s - 1) + 1, e);
    for (const l of block.split("\n")) if (l.trim() && !re.test(l)) allHave = false;
    lines((l) => (allHave ? l.replace(re, "") : l.trim() ? prefix + l.replace(/^(\s*)([-*+] \[.\] |[-*+] |\d+\. |> )/, "$1") : l));
  }

  const actions = {
    heading: () => lines((l) => {
      const m = l.match(/^(#{1,6}) /);
      if (!m) return "# " + l;
      return m[1].length >= 3 ? l.slice(m[0].length) : "#" + l;
    }),
    bold: () => wrap("**"),
    italic: () => wrap("*"),
    strike: () => wrap("~~"),
    highlight: () => wrap("=="),
    link: () => wrap("[[", "]]"),
    list: () => togglePrefix("- ", /^\s*[-*+] (?!\[.\] )/),
    numbered: () => togglePrefix("1. ", /^\s*\d+\. /),
    task: () => togglePrefix("- [ ] ", /^\s*[-*+] \[.\] /),
    quote: () => togglePrefix("> ", /^> ?/),
    callout: () => {
      const { selectionStart: s, selectionEnd: e, value } = el.src;
      const sel = value.slice(s, e);
      const body = sel ? sel.split("\n").map((l) => "> " + l).join("\n") : "> ";
      const lead = s > 0 && value[s - 1] !== "\n" ? "\n" : "";
      insert(`${lead}> [!note]\n${body}`, s + lead.length + 3, s + lead.length + 7);
    },
    code: () => {
      const { selectionStart: s, selectionEnd: e, value } = el.src;
      const sel = value.slice(s, e);
      if (sel.includes("\n") || !sel) {
        const lead = s > 0 && value[s - 1] !== "\n" ? "\n" : "";
        insert(`${lead}\`\`\`\n${sel}\n\`\`\`\n`, s + lead.length + 4, s + lead.length + 4 + sel.length);
      } else wrap("`");
    },
    image: () => el.file.click(),
  };
  el.toolbar.addEventListener("mousedown", (e) => e.target.closest("button") && e.preventDefault());
  el.toolbar.addEventListener("click", (e) => {
    const b = e.target.closest("[data-md]");
    if (b && actions[b.dataset.md]) actions[b.dataset.md]();
  });

  el.src.addEventListener("keydown", (e) => {
    if (suggest.open && suggest.key(e)) return;
    const mod = e.ctrlKey || e.metaKey;
    if (mod && e.key.toLowerCase() === "s") {
      e.preventDefault();
      save();
    } else if (mod && e.key.toLowerCase() === "b") {
      e.preventDefault();
      actions.bold();
    } else if (mod && e.key.toLowerCase() === "i") {
      e.preventDefault();
      actions.italic();
    } else if (mod && e.key.toLowerCase() === "k") {
      e.preventDefault();
      actions.link();
    } else if (e.key === "Enter" && !e.shiftKey && !mod) {
      continueList(e);
    } else if (e.key === "Tab" && !mod) {
      const { selectionStart: s, selectionEnd: en, value } = el.src;
      const line = value.slice(value.lastIndexOf("\n", s - 1) + 1, value.indexOf("\n", s) < 0 ? value.length : value.indexOf("\n", s));
      if (s !== en || /^\s*([-*+] |\d+\. )/.test(line)) {
        e.preventDefault();
        lines((l) => (e.shiftKey ? l.replace(/^(\t| {1,4})/, "") : "\t" + l));
      }
    }
  });

  // Enter inside a list starts the next item; on an empty item it ends the list.
  function continueList(e) {
    const { selectionStart: s, selectionEnd: en, value } = el.src;
    if (s !== en) return;
    const start = value.lastIndexOf("\n", s - 1) + 1;
    const line = value.slice(start, s);
    const m = line.match(/^(\s*)([-*+] \[[ xX]\] |[-*+] |(\d+)([.)]) |> )/);
    if (!m) return;
    e.preventDefault();
    if (line.trim() === m[0].trim()) {
      el.src.setSelectionRange(start, s);
      insert("");
      return;
    }
    let next = m[2];
    if (m[3]) next = `${Number(m[3]) + 1}${m[4]} `;
    if (/\[[xX]\]/.test(next)) next = next.replace(/\[[xX]\]/, "[ ]");
    insert("\n" + m[1] + next);
  }

  // ---------- [[ link suggestions ----------
  const suggest = {
    open: false, items: [], index: 0, start: 0, box: null,
    update() {
      const { selectionStart: s, value } = el.src;
      const before = value.slice(Math.max(0, s - 120), s);
      const m = before.match(/\[\[([^\]\n|#]*)$/);
      if (!m || !cfg.editable) return this.close();
      const q = m[1].toLowerCase();
      this.start = s - m[1].length;
      this.items = files
        .filter((f) => f.path.toLowerCase().includes(q) && f.path !== note.path)
        .sort((a, b) => (isNote(b.path) - isNote(a.path)) || (stem(a.path).toLowerCase().startsWith(q) ? -1 : 0) - (stem(b.path).toLowerCase().startsWith(q) ? -1 : 0) || a.path.length - b.path.length)
        .slice(0, 8);
      if (!this.items.length) return this.close();
      this.index = 0;
      this.render();
    },
    render() {
      if (!this.box) {
        this.box = document.createElement("div");
        this.box.className = "ed-suggest";
        this.box.addEventListener("mousedown", (e) => {
          const d = e.target.closest("[data-i]");
          if (d) {
            e.preventDefault();
            this.pick(Number(d.dataset.i));
          }
        });
        document.body.append(this.box);
      }
      this.open = true;
      this.box.innerHTML = this.items.map((f, i) => `<div data-i="${i}" class="${i === this.index ? "on" : ""}">${escapeHTML(isNote(f.path) ? stem(f.path) : baseName(f.path))}${dirOf(f.path) ? `<small>${escapeHTML(dirOf(f.path))}</small>` : ""}</div>`).join("");
      const c = caret(el.src, el.src.selectionStart);
      this.box.style.left = Math.min(c.left, innerWidth - 316) + "px";
      this.box.style.top = Math.min(c.top + 26, innerHeight - this.box.offsetHeight - 8) + "px";
    },
    key(e) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        this.index = (this.index + (e.key === "ArrowDown" ? 1 : -1) + this.items.length) % this.items.length;
        this.render();
        return true;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        this.pick(this.index);
        return true;
      }
      if (e.key === "Escape") {
        this.close();
        return true;
      }
      return false;
    },
    pick(i) {
      const f = this.items[i];
      const name = isNote(f.path) ? stem(f.path) : baseName(f.path);
      const unique = files.filter((x) => (isNote(x.path) ? stem(x.path) : baseName(x.path)).toLowerCase() === name.toLowerCase()).length === 1;
      const target = unique ? name : f.path.replace(/\.md$/i, "");
      const { selectionStart: s, value } = el.src;
      const close = value.slice(s, s + 2) === "]]" ? "" : "]]";
      el.src.setSelectionRange(this.start, s);
      insert(target + close);
      if (!close) el.src.setSelectionRange(el.src.selectionStart + 2, el.src.selectionStart + 2);
      this.close();
    },
    close() {
      this.open = false;
      if (this.box) this.box.remove();
      this.box = null;
    },
  };
  el.src.addEventListener("input", () => suggest.update());
  el.src.addEventListener("blur", () => setTimeout(() => suggest.close(), 150));
  el.src.addEventListener("scroll", () => suggest.close());

  // Screen position of a caret in a textarea, via an off-screen mirror.
  function caret(ta, pos) {
    const m = document.createElement("div");
    const cs = getComputedStyle(ta);
    for (const p of ["boxSizing", "width", "fontFamily", "fontSize", "fontWeight", "lineHeight", "letterSpacing", "paddingTop", "paddingLeft", "paddingRight", "borderLeftWidth", "tabSize", "wordSpacing"]) m.style[p] = cs[p];
    Object.assign(m.style, { position: "fixed", visibility: "hidden", whiteSpace: "pre-wrap", overflowWrap: "break-word", top: "0", left: "0" });
    m.textContent = ta.value.slice(0, pos);
    const mark = document.createElement("span");
    mark.textContent = "​";
    m.append(mark);
    document.body.append(m);
    const r = ta.getBoundingClientRect();
    const out = { left: r.left + mark.offsetLeft - ta.scrollLeft, top: r.top + mark.offsetTop - ta.scrollTop };
    m.remove();
    return out;
  }

  // ---------- images: paste, drop, pick ----------
  async function upload(list) {
    if (!note || !cfg.editable) return;
    const links = [];
    for (const f of list) {
      const form = new FormData();
      form.append("file", f, f.name || "image.png");
      form.append("dir", dirOf(note.path));
      notice(t("editor.uploading"), "info");
      try {
        const r = await call("POST", `${base}/api/upload`, undefined, form);
        links.push(`![[${baseName(r.path)}]]`);
      } catch (e) {
        notice(t("editor.uploadFailed") + " " + e.message);
        return;
      }
    }
    notice("");
    insert(links.join("\n"));
    loadTree(true);
  }
  el.src.addEventListener("paste", (e) => {
    const fs = [...(e.clipboardData?.files || [])];
    if (fs.length) {
      e.preventDefault();
      upload(fs);
    }
  });
  el.src.addEventListener("dragover", (e) => e.dataTransfer?.types.includes("Files") && e.preventDefault());
  el.src.addEventListener("drop", (e) => {
    const fs = [...(e.dataTransfer?.files || [])];
    if (fs.length) {
      e.preventDefault();
      upload(fs);
    }
  });
  el.file.addEventListener("change", () => {
    upload([...el.file.files]);
    el.file.value = "";
  });

  // ---------- publishing ----------
  const fmRe = /^---\r?\n([\s\S]*?)\r?\n---[ \t]*(\r?\n|$)/;
  function isMarkedPublished(text) {
    const m = text.match(fmRe);
    return !!m && /^publish:\s*(true|yes|on|1)\s*$/im.test(m[1]);
  }
  function setPublishFlag(text, on) {
    const m = text.match(fmRe);
    if (!m) return on ? `---\npublish: true\n---\n${text}` : text;
    let body = m[1].split(/\r?\n/).filter((l) => !/^publish:/i.test(l));
    if (on) body.unshift("publish: true");
    if (!body.some((l) => l.trim())) return text.slice(m[0].length);
    return `---\n${body.join("\n")}\n---\n` + text.slice(m[0].length);
  }
  function publishedNow() {
    if (!note || !isNote(note.path) || !cfg.publish.enabled) return false;
    if (cfg.publish.mode === "folder") return !cfg.publish.folder || note.path.startsWith(cfg.publish.folder + "/");
    return isMarkedPublished(el.src.value);
  }
  function updatePublish() {
    const usable = note && isNote(note.path) && !note.binary && cfg.publish.enabled;
    el.pub.hidden = !usable || cfg.publish.mode === "folder" || !cfg.editable;
    const on = usable && publishedNow();
    el.view.hidden = !on;
    if (on) {
      el.view.href = publicURL(note.path);
      el.view.title = t("editor.viewPublished");
    }
    if (!el.pub.hidden) {
      el.pub.classList.toggle("on", on);
      el.pub.querySelector("span").textContent = on ? t("editor.published") : t("editor.publish");
      el.pub.title = on ? t("editor.unpublish") : t("editor.publish");
    }
  }
  el.pub.addEventListener("click", () => {
    const on = !publishedNow();
    const { selectionStart: s } = el.src;
    const before = el.src.value.length;
    el.src.value = setPublishFlag(el.src.value, on);
    const delta = el.src.value.length - before;
    el.src.setSelectionRange(Math.max(0, s + delta), Math.max(0, s + delta));
    changed();
    save();
  });

  // ---------- start ----------
  if (store.get("noSide", false) && innerWidth > 800) root.classList.add("no-side");
  (async () => {
    try {
      await loadTree(true);
    } catch (e) {
      notice(e.message);
    }
    if (cfg.path) await open(cfg.path);
    else showEmpty();
  })();
})();
