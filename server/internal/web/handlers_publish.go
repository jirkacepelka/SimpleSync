package web

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jirkacepelka/obsisync/server/internal/i18n"
	"github.com/jirkacepelka/obsisync/server/internal/markdown"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// ---- settings (vault owners) ----

func (w *Web) publishSave(rw http.ResponseWriter, r *http.Request, p *page) {
	back := fmt.Sprintf("/vaults/%d/settings#publish", p.Vault.ID)
	ps := store.Publish{
		VaultID: p.Vault.ID,
		Enabled: r.FormValue("enabled") == "1",
		Slug:    r.FormValue("slug"),
		Title:   r.FormValue("title"),
		Mode:    r.FormValue("mode"),
		Folder:  r.FormValue("folder"),
		Home:    r.FormValue("home"),
	}
	if err := w.Store.SavePublish(r.Context(), ps); err != nil {
		redirect(rw, r, back, "", w.errText(r, err))
		return
	}
	w.sites.Delete(p.Vault.ID)
	msg := w.tr(r, "publish.savedOff")
	if ps.Enabled {
		msg = w.tr(r, "publish.savedOn", "/p/"+strings.ToLower(strings.TrimSpace(ps.Slug))+"/")
	}
	redirect(rw, r, back, msg, "")
}

// suggestSlug turns a vault name into a site address.
func suggestSlug(name string) string {
	s := markdown.Slug(name)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) < 3 {
		out = "notes-" + out
	}
	if len(out) > 50 {
		out = strings.Trim(out[:50], "-")
	}
	return out
}

// ---- the public site ----

// site is the published part of a vault, rebuilt when the vault or its
// settings change.
type site struct {
	key    string
	pub    store.Publish
	notes  map[string]bool // published notes
	attach map[string]bool // files linked or embedded from published notes
	titles map[string]string
	mtimes map[string]int64
	hashes map[string]string
	ix     *fileIndex // every file of the vault, to resolve links
	order  []string

	excerpts  map[string]string   // first sentences, for the front page cards
	words     map[string]int      // word count, for the reading time
	backlinks map[string][]string // published notes linking to a note
}

func noteURL(slug, fp string) string {
	return "/p/" + slug + "/" + escapePath(strings.TrimSuffix(fp, ".md"))
}

func fileURL(slug, fp string) string { return "/p/" + slug + "/" + escapePath(fp) }

// resolvers returns link and embed resolvers limited to what is public.
func (s *site) resolvers(from string) (func(string) (string, bool), func(string) (string, bool)) {
	link := func(t string) (string, bool) {
		fp, anchor, ok := s.ix.resolve(from, t)
		switch {
		case !ok:
			return "", false
		case fp == from && anchor != "":
			return "#" + anchor, true
		case s.notes[fp]:
			return withAnchor(noteURL(s.pub.Slug, fp), anchor), true
		case s.attach[fp]:
			return fileURL(s.pub.Slug, fp), true
		}
		return "", false
	}
	embed := func(t string) (string, bool) {
		fp, _, ok := s.ix.resolve(from, t)
		if ok && s.attach[fp] {
			return fileURL(s.pub.Slug, fp), true
		}
		return "", false
	}
	return link, embed
}

func (w *Web) buildSite(ctx context.Context, pub store.Publish, v *store.Vault) (*site, error) {
	key := fmt.Sprintf("%d|%+v", v.HeadRev, pub)
	if c, ok := w.sites.Load(v.ID); ok && c.(*site).key == key {
		return c.(*site), nil
	}
	files, err := w.Store.ListFiles(ctx, v.ID, false)
	if err != nil {
		return nil, err
	}
	s := &site{key: key, pub: pub, notes: map[string]bool{}, attach: map[string]bool{},
		titles: map[string]string{}, mtimes: map[string]int64{}, hashes: map[string]string{}, ix: filesIndex(files, nil),
		excerpts: map[string]string{}, words: map[string]int{}, backlinks: map[string][]string{}}
	texts := map[string]string{}
	for _, f := range files {
		if f.Deleted || hiddenPath(f.Path) {
			continue
		}
		s.hashes[f.Path], s.mtimes[f.Path] = f.Hash, f.Mtime
		if !strings.EqualFold(path.Ext(f.Path), ".md") {
			continue
		}
		inFolder := pub.Folder == "" || strings.HasPrefix(f.Path, pub.Folder+"/")
		if pub.Mode == store.PublishFolder && !inFolder && f.Path != pub.Home {
			continue
		}
		text, ok := w.blobText(f.Hash)
		if !ok {
			continue
		}
		fm, _ := markdown.SplitFrontmatter([]byte(text))
		if pub.Mode == store.PublishMarked && !fm.Bool("publish") && f.Path != pub.Home {
			continue
		}
		s.notes[f.Path] = true
		texts[f.Path] = text
		_, body := markdown.SplitFrontmatter([]byte(text))
		s.excerpts[f.Path] = excerpt(string(body))
		s.words[f.Path] = len(strings.Fields(string(body)))
		title := fm.Get("title")
		if title == "" {
			title = strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path))
		}
		s.titles[f.Path] = title
	}
	// Files referenced from published notes become public too, and links
	// between published notes become backlinks.
	for fp, text := range texts {
		seen := map[string]bool{}
		record := func(t string) (string, bool) {
			p, _, ok := s.ix.resolve(fp, t)
			switch {
			case !ok:
			case s.notes[p]:
				if p != fp && !seen[p] {
					seen[p] = true
					s.backlinks[p] = append(s.backlinks[p], fp)
				}
			case !strings.EqualFold(path.Ext(p), ".md"):
				s.attach[p] = true
			}
			return "", false
		}
		markdown.Render([]byte(text), markdown.Options{Link: record, Embed: record})
	}
	for fp := range s.notes {
		s.order = append(s.order, fp)
	}
	sort.Slice(s.order, func(i, j int) bool { return strings.ToLower(s.order[i]) < strings.ToLower(s.order[j]) })
	for _, bl := range s.backlinks {
		sort.Slice(bl, func(i, j int) bool { return strings.ToLower(bl[i]) < strings.ToLower(bl[j]) })
	}
	w.sites.Store(v.ID, s)
	return s, nil
}

type navItem struct {
	Name     string
	URL      string
	Active   bool
	Open     bool
	Children []*navItem
}

func (s *site) nav(current string) []*navItem {
	root := &navItem{}
	dirs := map[string]*navItem{"": root}
	var dirOf func(d string) *navItem
	dirOf = func(d string) *navItem {
		if n, ok := dirs[d]; ok {
			return n
		}
		up := path.Dir(d)
		if up == "." {
			up = ""
		}
		parent := dirOf(up)
		n := &navItem{Name: path.Base(d)}
		parent.Children = append(parent.Children, n)
		dirs[d] = n
		return n
	}
	for _, fp := range s.order {
		d := path.Dir(fp)
		if d == "." {
			d = ""
		}
		parent := dirOf(d)
		parent.Children = append(parent.Children, &navItem{Name: s.titles[fp], URL: noteURL(s.pub.Slug, fp), Active: fp == current})
		if fp == current {
			for x := d; x != ""; x = path.Dir(x) {
				dirs[x].Open = true
				if path.Dir(x) == "." {
					break
				}
			}
		}
	}
	var sortItems func(items []*navItem)
	sortItems = func(items []*navItem) {
		sort.SliceStable(items, func(i, j int) bool {
			if (items[i].URL == "") != (items[j].URL == "") {
				return items[i].URL == ""
			}
			return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
		})
		for _, it := range items {
			sortItems(it.Children)
		}
	}
	sortItems(root.Children)
	return root.Children
}

type publicPage struct {
	Lang      string
	Site      string
	Initial   string // first letter of the site name, for the logo mark
	SiteURL   string
	Title     string        // plain text, for <title>
	TitleHTML template.HTML // the note's own H1 (may contain inline markup)
	Crumbs    []string
	Content   template.HTML
	Updated   string
	Minutes   int
	Excerpt   string
	Nav       []*navItem
	TOC       []tocItem
	Backlinks []*card
	Prev      *card
	Next      *card
	Cards     []*card // front page without a chosen note
	NotFound  bool
	T         func(string, ...any) string
}

type card struct {
	Title, URL, Folder, Excerpt string
}

type tocItem struct {
	Sub      bool
	ID, Text string
}

func (s *site) card(fp string) *card {
	d := path.Dir(fp)
	if d == "." {
		d = ""
	}
	return &card{Title: s.titles[fp], URL: noteURL(s.pub.Slug, fp), Folder: d, Excerpt: s.excerpts[fp]}
}

var (
	leadingH1 = regexp.MustCompile(`^\s*<h1[^>]*>(.*?)</h1>\s*`)
	headingRe = regexp.MustCompile(`<h([23]) id="([^"]+)">(.*?)</h[23]>`)
	tagRe     = regexp.MustCompile(`<[^>]+>`)
	mdNoise   = regexp.MustCompile("!?\\[\\[([^\\]|]*\\|)?([^\\]]*)\\]\\]|!?\\[([^\\]]*)\\]\\([^)]*\\)|[*_=~`#>]+")
)

func plain(h string) string {
	return strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(h, "")))
}

// excerpt returns the first lines of prose of a note, without Markdown.
func excerpt(body string) string {
	var out []string
	n := 0
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "![") || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "---") || strings.HasPrefix(t, "```") || strings.HasPrefix(t, "> [!") {
			if n > 0 && t == "" {
				break
			}
			continue
		}
		t = strings.TrimLeft(t, "-*+0123456789. []xX")
		t = mdNoise.ReplaceAllStringFunc(t, func(m string) string {
			sub := mdNoise.FindStringSubmatch(m)
			switch {
			case sub[2] != "":
				return sub[2]
			case sub[3] != "":
				return sub[3]
			}
			return ""
		})
		out = append(out, strings.TrimSpace(t))
		n += len(t)
		if n > 180 {
			break
		}
	}
	e := strings.Join(out, " ")
	if r := []rune(e); len(r) > 180 {
		e = strings.TrimSpace(string(r[:180])) + "…"
	}
	return e
}

func (w *Web) publicPage(rw http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(r.PathValue("slug"))
	rp := strings.Trim(r.PathValue("path"), "/")
	lang := i18n.FromRequest(r)
	pp := &publicPage{Lang: lang, T: func(k string, a ...any) string { return i18n.T(lang, k, a...) }}
	pub, err := w.Store.PublishBySlug(r.Context(), slug)
	var v *store.Vault
	if err == nil {
		v, err = w.Store.Vault(r.Context(), pub.VaultID)
	}
	if err != nil {
		w.publicNotFound(rw, pp)
		return
	}
	s, err := w.buildSite(r.Context(), pub, v)
	if err != nil {
		http.Error(rw, "internal error", 500)
		return
	}
	pp.Site = pub.Title
	if pp.Site == "" {
		pp.Site = v.Name
	}
	pp.SiteURL = "/p/" + pub.Slug + "/"
	if r.URL.Path == "/p/"+r.PathValue("slug") {
		http.Redirect(rw, r, pp.SiteURL, http.StatusMovedPermanently)
		return
	}
	fp := ""
	switch {
	case rp == "" && s.notes[pub.Home]:
		fp = pub.Home
	case rp == "":
		pp.Title = pp.Site
		pp.Nav = s.nav("")
		for _, fp := range s.order {
			pp.Cards = append(pp.Cards, s.card(fp))
		}
		w.renderPublic(rw, http.StatusOK, pp)
		return
	case s.notes[rp+".md"]:
		fp = rp + ".md"
	case s.attach[rp]:
		w.servePublicFile(rw, r, s.hashes[rp], rp)
		return
	default:
		pp.Nav = s.nav("")
		w.publicNotFound(rw, pp)
		return
	}
	text, ok := w.blobText(s.hashes[fp])
	if !ok {
		w.publicNotFound(rw, pp)
		return
	}
	link, embed := s.resolvers(fp)
	res, err := markdown.Render([]byte(text), markdown.Options{Link: link, Embed: embed})
	if err != nil {
		http.Error(rw, "internal error", 500)
		return
	}
	body := res.HTML // produced by the sanitizing renderer
	pp.Title = s.titles[fp]
	pp.TitleHTML = template.HTML(template.HTMLEscapeString(pp.Title))
	// A note that opens with its own heading uses it as the page title.
	if m := leadingH1.FindStringSubmatch(body); m != nil {
		pp.TitleHTML = template.HTML(m[1])
		pp.Title = plain(m[1])
		body = body[len(m[0]):]
	}
	for _, m := range headingRe.FindAllStringSubmatch(body, -1) {
		pp.TOC = append(pp.TOC, tocItem{Sub: m[1] == "3", ID: m[2], Text: plain(m[3])})
	}
	if len(pp.TOC) < 2 {
		pp.TOC = nil
	}
	pp.Content = template.HTML(body)
	pp.Excerpt = s.excerpts[fp]
	pp.Updated = time.UnixMilli(s.mtimes[fp]).Local().Format(i18n.T(lang, "fmt.date"))
	pp.Minutes = max(1, (s.words[fp]+199)/200)
	if d := path.Dir(fp); d != "." {
		pp.Crumbs = strings.Split(d, "/")
	}
	for _, b := range s.backlinks[fp] {
		pp.Backlinks = append(pp.Backlinks, s.card(b))
	}
	for i, o := range s.order {
		if o != fp {
			continue
		}
		if i > 0 {
			pp.Prev = s.card(s.order[i-1])
		}
		if i+1 < len(s.order) {
			pp.Next = s.card(s.order[i+1])
		}
	}
	pp.Nav = s.nav(fp)
	w.renderPublic(rw, http.StatusOK, pp)
}

func (w *Web) publicNotFound(rw http.ResponseWriter, pp *publicPage) {
	pp.NotFound = true
	pp.Title = pp.T("public.notFound")
	w.renderPublic(rw, http.StatusNotFound, pp)
}

func setPublicHeaders(rw http.ResponseWriter) {
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	rw.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

func (w *Web) renderPublic(rw http.ResponseWriter, status int, pp *publicPage) {
	if pp.Site == "" {
		pp.Site = "SimpleSync"
	}
	pp.Initial = "S"
	for _, r := range pp.Site {
		pp.Initial = strings.ToUpper(string(r))
		break
	}
	setPublicHeaders(rw)
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(status)
	if err := w.public.Execute(rw, pp); err != nil {
		w.Log.Error("render public page", "err", err)
	}
}

func (w *Web) servePublicFile(rw http.ResponseWriter, r *http.Request, hash, name string) {
	q := r.URL.Query()
	q.Set("inline", "1")
	r.URL.RawQuery = q.Encode()
	rw.Header().Set("Cache-Control", "public, max-age=300")
	w.serveBlob(rw, r, hash, name)
}

// siteCache is embedded in Web.
type siteCache = sync.Map
