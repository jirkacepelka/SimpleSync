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
