package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"path"
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
		titles: map[string]string{}, mtimes: map[string]int64{}, hashes: map[string]string{}, ix: filesIndex(files, nil)}
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
		title := fm.Get("title")
		if title == "" {
			title = strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path))
		}
		s.titles[f.Path] = title
	}
	// Files referenced from published notes become public too.
	for fp, text := range texts {
		record := func(t string) (string, bool) {
			if p, _, ok := s.ix.resolve(fp, t); ok && !s.notes[p] && !strings.EqualFold(path.Ext(p), ".md") {
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
	Lang     string
	Site     string
	SiteURL  string
	Title    string
	Content  template.HTML
	Updated  string
	Nav      []*navItem
	Index    []*navItem
	NotFound bool
	// OwnHeading: the note starts with an H1, so the page title is not repeated.
	OwnHeading bool
	T          func(string, ...any) string
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
			name := strings.TrimSuffix(fp, ".md")
			if t := s.titles[fp]; t != path.Base(name) {
				name = t
			}
			pp.Index = append(pp.Index, &navItem{Name: name, URL: noteURL(pub.Slug, fp)})
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
	pp.Title = s.titles[fp]
	// A note that opens with its own heading doesn't need a second one.
	pp.OwnHeading = strings.HasPrefix(strings.TrimSpace(res.HTML), "<h1")
	pp.Content = template.HTML(res.HTML) // produced by the sanitizing renderer
	pp.Updated = time.UnixMilli(s.mtimes[fp]).Local().Format(i18n.T(lang, "fmt.date"))
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
