// Published sites: theme switch, note filter, mobile menu, "on this page"
// highlighting, heading links, copy buttons and a reading progress bar.
(function () {
  var root = document.documentElement;
  try {
    var saved = localStorage.getItem("pub-theme");
    if (saved === "light" || saved === "dark") root.dataset.theme = saved;
  } catch (e) {}

  document.addEventListener("DOMContentLoaded", function () {
    var $ = function (s, c) { return (c || document).querySelector(s); };
    var $$ = function (s, c) { return Array.prototype.slice.call((c || document).querySelectorAll(s)); };

    // theme
    var tb = $(".theme-btn");
    if (tb) tb.addEventListener("click", function () {
      var dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
      root.dataset.theme = dark ? "light" : "dark";
      try { localStorage.setItem("pub-theme", root.dataset.theme); } catch (e) {}
    });

    // mobile navigation drawer
    var mb = $(".pub-menu-btn");
    function nav(open) {
      document.body.classList.toggle("nav-open", open);
      if (mb) mb.setAttribute("aria-expanded", open ? "true" : "false");
    }
    if (mb) mb.addEventListener("click", function () { nav(!document.body.classList.contains("nav-open")); });
    var scrim = $(".pub-scrim");
    if (scrim) scrim.addEventListener("click", function () { nav(false); });
    document.addEventListener("keydown", function (e) { if (e.key === "Escape") nav(false); });
    $$(".pub-side a").forEach(function (a) { a.addEventListener("click", function () { nav(false); }); });

    // filter the navigation
    var f = $(".pub-filter input"), none = $(".pub-empty-filter");
    if (f) {
      var details = $$(".pub-nav details");
      var wasOpen = details.map(function (d) { return d.open; });
      f.addEventListener("input", function () {
        var q = f.value.trim().toLowerCase(), hits = 0;
        $$(".pub-nav li").forEach(function (li) { li.hidden = false; });
        if (!q) { details.forEach(function (d, i) { d.open = wasOpen[i]; }); if (none) none.hidden = true; return; }
        $$(".pub-nav a").forEach(function (a) {
          var ok = a.textContent.toLowerCase().indexOf(q) >= 0;
          a.parentNode.hidden = !ok;
          if (ok) hits++;
        });
        details.slice().reverse().forEach(function (d) {
          var any = $$("li:not([hidden])", d).length > 0;
          d.parentNode.hidden = !any;
          d.open = any;
        });
        if (none) none.hidden = hits > 0;
      });
      f.addEventListener("keydown", function (e) {
        if (e.key === "Enter") { var a = $(".pub-nav li:not([hidden]) a"); if (a) location.href = a.href; }
      });
    }
    document.addEventListener("keydown", function (e) {
      if (e.key === "/" && f && !/input|textarea/i.test(document.activeElement.tagName)) { e.preventDefault(); nav(true); f.focus(); }
    });

    // heading links and copy buttons
    var label = document.body.dataset;
    $$(".md h2[id], .md h3[id], .md h4[id]").forEach(function (h) {
      var a = document.createElement("a");
      a.className = "anchor"; a.href = "#" + h.id; a.textContent = "#"; a.setAttribute("aria-label", h.textContent);
      h.insertBefore(a, h.firstChild);
    });
    $$(".md pre").forEach(function (pre) {
      var wrap = document.createElement("div");
      wrap.className = "pre";
      pre.parentNode.insertBefore(wrap, pre);
      wrap.appendChild(pre);
      var b = document.createElement("button");
      b.type = "button"; b.className = "copy"; b.textContent = label.copy || "Copy";
      b.addEventListener("click", function () {
        var done = function () { b.textContent = label.copied || "Copied"; setTimeout(function () { b.textContent = label.copy || "Copy"; }, 1600); };
        if (navigator.clipboard) navigator.clipboard.writeText(pre.innerText).then(done, function () {});
      });
      wrap.appendChild(b);
    });

    // "on this page": follow the reader
    var links = $$(".pub-toc a"), heads = links.map(function (a) { return document.getElementById(decodeURIComponent(a.hash.slice(1))); });
    var bar = $(".pub-progress");
    function onScroll() {
      var y = window.scrollY, h = document.documentElement.scrollHeight - innerHeight;
      if (bar) bar.style.width = (h > 0 ? Math.min(100, y / h * 100) : 0) + "%";
      var cur = -1;
      heads.forEach(function (el, i) { if (el && el.getBoundingClientRect().top < 110) cur = i; });
      links.forEach(function (a, i) { a.classList.toggle("on", i === cur); });
    }
    if (bar || links.length) { addEventListener("scroll", onScroll, { passive: true }); onScroll(); }
  });
})();

// Link graph of the published notes: a small force layout on a canvas.
(function () {
  document.addEventListener("DOMContentLoaded", function () {
    var btn = document.querySelector(".graph-btn"), dlg = document.querySelector(".pub-graph");
    if (!btn || !dlg || !dlg.showModal) { if (btn) btn.hidden = true; return; }
    var cv = dlg.querySelector("canvas"), ctx = cv.getContext("2d");
    var nodes = [], edges = [], cur = -1, hover = -1, drag = null, pan = null, alpha = 1, raf = 0;
    var view = { x: 0, y: 0, k: 1 }, W = 0, H = 0, dpr = 1, loaded = false, col = {};

    function colors() {
      var s = getComputedStyle(document.documentElement);
      ["--accent", "--line-strong", "--muted", "--text-strong", "--faint", "--bg"].forEach(function (n) { col[n] = s.getPropertyValue(n).trim(); });
      col.font = getComputedStyle(document.body).fontFamily;
    }
    function resize() {
      dpr = window.devicePixelRatio || 1;
      var r = cv.getBoundingClientRect();
      W = r.width; H = r.height;
      cv.width = W * dpr; cv.height = H * dpr;
      draw();
    }
    function load(data) {
      var here = decodeURIComponent(location.pathname).replace(/\/$/, "");
      nodes = data.nodes.map(function (n, i) {
        var a = (i / data.nodes.length) * Math.PI * 2, r = 60 + Math.sqrt(data.nodes.length) * 18;
        var url = decodeURIComponent(n.url);
        if (url === here || (url + "/") === here + "/") cur = i;
        return { title: n.title, url: n.url, x: Math.cos(a) * r, y: Math.sin(a) * r, vx: 0, vy: 0, deg: 0, nb: {} };
      });
      edges = data.edges;
      edges.forEach(function (e) {
        nodes[e[0]].deg++; nodes[e[1]].deg++;
        nodes[e[0]].nb[e[1]] = 1; nodes[e[1]].nb[e[0]] = 1;
      });
      view = { x: 0, y: 0, k: 1 };
      alpha = 1; loaded = true;
    }
    function fit() {
      if (!nodes.length || !W) return;
      var x0 = 1e9, x1 = -1e9, y0 = 1e9, y1 = -1e9;
      nodes.forEach(function (n) { x0 = Math.min(x0, n.x); x1 = Math.max(x1, n.x); y0 = Math.min(y0, n.y); y1 = Math.max(y1, n.y); });
      var bw = Math.max(60, x1 - x0 + 120), bh = Math.max(60, y1 - y0 + 100);
      view.k = Math.min(2, Math.max(.3, Math.min(W / bw, H / bh)));
      view.x = -(x0 + x1) / 2; view.y = -(y0 + y1) / 2;
    }
    function rad(n) { return 4 + Math.min(9, Math.sqrt(n.deg) * 2.4); }

    function tick() {
      var n = nodes.length, rep = 7000, i, j, a, b, dx, dy, d2, f;
      for (i = 0; i < n; i++) for (j = i + 1; j < n; j++) {
        a = nodes[i]; b = nodes[j]; dx = a.x - b.x; dy = a.y - b.y; d2 = dx * dx + dy * dy + 0.01;
        if (d2 > 250000) continue;
        f = rep / d2 * alpha; d = Math.sqrt(d2);
        a.vx += dx / d * f; a.vy += dy / d * f; b.vx -= dx / d * f; b.vy -= dy / d * f;
      }
      edges.forEach(function (e) {
        a = nodes[e[0]]; b = nodes[e[1]]; dx = b.x - a.x; dy = b.y - a.y;
        var d = Math.sqrt(dx * dx + dy * dy) + 0.01; f = (d - 110) * 0.02 * alpha;
        a.vx += dx / d * f; a.vy += dy / d * f; b.vx -= dx / d * f; b.vy -= dy / d * f;
      });
      nodes.forEach(function (p) {
        p.vx -= p.x * 0.004 * alpha; p.vy -= p.y * 0.004 * alpha;
        if (p !== drag) { p.x += p.vx; p.y += p.vy; }
        p.vx *= 0.6; p.vy *= 0.6;
      });
      alpha *= 0.985;
    }
    var d;
    function loop() {
      raf = 0;
      if (!dlg.open) return;
      if (alpha > 0.02 || drag) { tick(); if (alpha > 0.02 || drag) raf = requestAnimationFrame(loop); }
      draw();
    }
    function kick(a) { alpha = Math.max(alpha, a); if (!raf) raf = requestAnimationFrame(loop); }

    function sx(x) { return W / 2 + (x + view.x) * view.k; }
    function sy(y) { return H / 2 + (y + view.y) * view.k; }
    function draw() {
      if (!W) return;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, W, H);
      var focus = hover >= 0 ? hover : cur;
      edges.forEach(function (e) {
        var a = nodes[e[0]], b = nodes[e[1]], hot = focus >= 0 && (e[0] === focus || e[1] === focus);
        ctx.strokeStyle = hot ? col["--accent"] : col["--line-strong"];
        ctx.globalAlpha = focus >= 0 && !hot ? 0.45 : 1;
        ctx.lineWidth = hot ? 1.6 : 1;
        ctx.beginPath(); ctx.moveTo(sx(a.x), sy(a.y)); ctx.lineTo(sx(b.x), sy(b.y)); ctx.stroke();
      });
      ctx.globalAlpha = 1;
      var many = nodes.length > 60;
      ctx.font = "12px " + col.font; ctx.textAlign = "center";
      nodes.forEach(function (n, i) {
        var near = focus >= 0 && (i === focus || n.nb[focus]);
        var r = rad(n) * Math.min(1.4, Math.max(.8, view.k));
        ctx.globalAlpha = focus >= 0 && !near ? 0.4 : 1;
        ctx.fillStyle = i === cur || i === hover || near ? col["--accent"] : col["--muted"];
        ctx.beginPath(); ctx.arc(sx(n.x), sy(n.y), r, 0, 7); ctx.fill();
        if (i === cur) { ctx.strokeStyle = col["--accent"]; ctx.lineWidth = 2; ctx.beginPath(); ctx.arc(sx(n.x), sy(n.y), r + 4, 0, 7); ctx.stroke(); }
        if (!many || near || i === cur) {
          ctx.fillStyle = i === hover || i === cur ? col["--text-strong"] : col["--muted"];
          ctx.fillText(n.title, sx(n.x), sy(n.y) + r + 14);
        }
      });
      ctx.globalAlpha = 1;
    }
    function pick(px, py) {
      for (var i = nodes.length - 1; i >= 0; i--) {
        var n = nodes[i], r = rad(n) + 5;
        if (Math.pow(px - sx(n.x), 2) + Math.pow(py - sy(n.y), 2) <= r * r) return i;
      }
      return -1;
    }
    function pos(e) { var r = cv.getBoundingClientRect(); return [e.clientX - r.left, e.clientY - r.top]; }
    var moved = 0;
    cv.addEventListener("pointerdown", function (e) {
      var p = pos(e), i = pick(p[0], p[1]); moved = 0;
      cv.setPointerCapture(e.pointerId);
      if (i >= 0) drag = nodes[i]; else pan = { x: p[0], y: p[1], vx: view.x, vy: view.y };
      cv.dataset.down = i;
    });
    cv.addEventListener("pointermove", function (e) {
      var p = pos(e);
      if (drag) {
        moved++; drag.x = (p[0] - W / 2) / view.k - view.x; drag.y = (p[1] - H / 2) / view.k - view.y; kick(0.3);
      } else if (pan) {
        moved++; view.x = pan.vx + (p[0] - pan.x) / view.k; view.y = pan.vy + (p[1] - pan.y) / view.k; draw();
      } else {
        var i = pick(p[0], p[1]);
        if (i !== hover) { hover = i; cv.classList.toggle("hover", i >= 0); draw(); }
      }
    });
    cv.addEventListener("pointerup", function () {
      var i = +cv.dataset.down; drag = null; pan = null;
      if (i >= 0 && moved < 4) location.href = nodes[i].url;
    });
    cv.addEventListener("wheel", function (e) {
      e.preventDefault();
      view.k = Math.min(3, Math.max(.3, view.k * (e.deltaY < 0 ? 1.12 : 1 / 1.12))); draw();
    }, { passive: false });

    function open() {
      colors();
      dlg.showModal(); resize();
      if (!loaded) {
        fetch(btn.dataset.src, { credentials: "omit" }).then(function (r) { return r.json(); }).then(function (data) {
          load(data);
          for (var i = 0; i < 200; i++) tick();
          fit();
          kick(0.3);
        });
      } else kick(0.3);
    }
    btn.addEventListener("click", open);
    dlg.querySelector(".graph-close").addEventListener("click", function () { dlg.close(); });
    dlg.addEventListener("click", function (e) { if (e.target === dlg) dlg.close(); });
    addEventListener("resize", function () { if (dlg.open) resize(); });
  });
})();
