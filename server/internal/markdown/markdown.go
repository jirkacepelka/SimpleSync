// Package markdown renders Obsidian-flavoured Markdown to safe HTML for the
// web editor's preview and for published pages.
//
// On top of CommonMark and GitHub extensions (tables, task lists,
// strikethrough, autolinks) it understands [[wikilinks]], ![[embeds]],
// callouts (> [!note]), ==highlights==, #tags and YAML front matter.
// Raw HTML in notes is never passed through.
package markdown

import (
	"bytes"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Options tell the renderer where links and embeds point to. Targets are
// what the note wrote: a wikilink target ("Folder/Note", "photo.png") or a
// relative Markdown link/image destination, already URL-decoded.
type Options struct {
	// Link resolves a link target; ok=false renders it as unresolved text.
	Link func(target string) (href string, ok bool)
	// Embed resolves an embedded file (usually an image).
	Embed func(target string) (src string, ok bool)
	// Properties renders the front matter as a small table above the note.
	Properties bool
}

// Result is a rendered note.
type Result struct {
	HTML  string
	Front Frontmatter
}

// Render converts a note to HTML.
func Render(src []byte, o Options) (Result, error) {
	front, body := SplitFrontmatter(src)
	if o.Link == nil {
		o.Link = func(string) (string, bool) { return "", false }
	}
	if o.Embed == nil {
		o.Embed = func(string) (string, bool) { return "", false }
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify, extension.TaskList),
		goldmark.WithParserOptions(
			parser.WithInlineParsers(
				util.Prioritized(wikilinkParser{}, 150),
				util.Prioritized(highlightParser{}, 500),
				util.Prioritized(tagParser{}, 500),
			),
			parser.WithASTTransformers(util.Prioritized(&transformer{o: o}, 100)),
		),
		goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(nodeRenderer{}, 100))),
	)
	var buf bytes.Buffer
	if o.Properties && len(front) > 0 {
		buf.WriteString(`<table class="properties"><tbody>`)
		for _, kv := range front {
			buf.WriteString("<tr><th>")
			buf.Write(util.EscapeHTML([]byte(kv.Key)))
			buf.WriteString("</th><td>")
			buf.Write(util.EscapeHTML([]byte(kv.Value)))
			buf.WriteString("</td></tr>")
		}
		buf.WriteString("</tbody></table>\n")
	}
	if err := md.Convert(body, &buf); err != nil {
		return Result{}, err
	}
	return Result{HTML: buf.String(), Front: front}, nil
}

// ---- front matter ----

// Property is one "key: value" line of front matter; lists are joined
// with ", ".
type Property struct{ Key, Value string }

type Frontmatter []Property

// Get returns the value of key, or "".
func (f Frontmatter) Get(key string) string {
	for _, p := range f {
		if strings.EqualFold(p.Key, key) {
			return p.Value
		}
	}
	return ""
}

// Bool reports whether key is set to a true-ish value.
func (f Frontmatter) Bool(key string) bool {
	switch strings.ToLower(f.Get(key)) {
	case "true", "yes", "1", "on":
		return true
	}
	return false
}

// SplitFrontmatter separates a leading YAML block from the note body. Only
// flat "key: value" pairs and simple lists are understood, which covers
// Obsidian's properties.
func SplitFrontmatter(src []byte) (Frontmatter, []byte) {
	end := FrontmatterEnd(src)
	if end == 0 {
		return nil, src
	}
	var out Frontmatter
	block := string(src[:end])
	for _, line := range strings.Split(block, "\n")[1:] {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "---" || strings.TrimSpace(line) == "" {
			continue
		}
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- ") && len(out) > 0 {
			last := &out[len(out)-1]
			if last.Value != "" {
				last.Value += ", "
			}
			last.Value += unquote(strings.TrimSpace(t[2:]))
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") {
			continue
		}
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
			items := strings.Split(v[1:len(v)-1], ",")
			for i := range items {
				items[i] = unquote(strings.TrimSpace(items[i]))
			}
			v = strings.Join(items, ", ")
		} else {
			v = unquote(v)
		}
		out = append(out, Property{strings.TrimSpace(k), v})
	}
	return out, src[end:]
}

// FrontmatterEnd returns the byte offset right after the closing "---"
// line of a leading front matter block, or 0 when there is none.
func FrontmatterEnd(src []byte) int {
	if !bytes.HasPrefix(src, []byte("---\n")) && !bytes.HasPrefix(src, []byte("---\r\n")) {
		return 0
	}
	i := bytes.IndexByte(src, '\n') + 1
	for i < len(src) {
		j := bytes.IndexByte(src[i:], '\n')
		line := src[i:]
		next := len(src)
		if j >= 0 {
			line = src[i : i+j]
			next = i + j + 1
		}
		if strings.TrimRight(string(line), "\r ") == "---" {
			return next
		}
		i = next
	}
	return 0
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// ---- helpers shared with callers ----

// IsImage reports whether name looks like an image Obsidian can embed.
func IsImage(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".avif", ".svg":
		return true
	}
	return false
}

// Slug turns heading text into the anchor used for #heading links.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case (unicode.IsSpace(r) || r == '-' || r == '_') && b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// SplitTarget splits "Note#Heading" into the note and the anchor slug.
func SplitTarget(t string) (note, anchor string) {
	note, frag, _ := strings.Cut(t, "#")
	if frag != "" && !strings.HasPrefix(frag, "^") {
		anchor = Slug(frag)
	}
	return strings.TrimSpace(note), anchor
}

// ---- AST nodes ----

var (
	KindWikilink  = ast.NewNodeKind("Wikilink")
	KindHighlight = ast.NewNodeKind("Highlight")
	KindTag       = ast.NewNodeKind("Tag")
	KindCallout   = ast.NewNodeKind("Callout")
)

// Wikilink is [[target|label]] or, with Embed, ![[target|label]].
type Wikilink struct {
	ast.BaseInline
	Target, Label string
	Embed         bool
	URL           string
	OK            bool
}

func (n *Wikilink) Kind() ast.NodeKind          { return KindWikilink }
func (n *Wikilink) Dump(src []byte, level int)  { ast.DumpHelper(n, src, level, nil, nil) }
func (n *Highlight) Kind() ast.NodeKind         { return KindHighlight }
func (n *Highlight) Dump(src []byte, level int) { ast.DumpHelper(n, src, level, nil, nil) }
func (n *Tag) Kind() ast.NodeKind               { return KindTag }
func (n *Tag) Dump(src []byte, level int)       { ast.DumpHelper(n, src, level, nil, nil) }
func (n *Callout) Kind() ast.NodeKind           { return KindCallout }
func (n *Callout) Dump(src []byte, level int)   { ast.DumpHelper(n, src, level, nil, nil) }

type Highlight struct{ ast.BaseInline }

type Tag struct {
	ast.BaseInline
	Name string
}

type Callout struct {
	ast.BaseBlock
	CalloutType, Title string
}

// ---- inline parsers ----

type wikilinkParser struct{}

func (wikilinkParser) Trigger() []byte { return []byte{'[', '!'} }

func (wikilinkParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	off, embed := 0, false
	switch {
	case bytes.HasPrefix(line, []byte("![[")):
		off, embed = 3, true
	case bytes.HasPrefix(line, []byte("[[")):
		off = 2
	default:
		return nil
	}
	end := bytes.Index(line[off:], []byte("]]"))
	if end <= 0 {
		return nil
	}
	inner := string(line[off : off+end])
	if strings.ContainsAny(inner, "[]\n") {
		return nil
	}
	block.Advance(off + end + 2)
	target, label, hasLabel := strings.Cut(inner, "|")
	target = strings.TrimSpace(target)
	if !hasLabel {
		label = target
		if note, frag, ok := strings.Cut(target, "#"); ok && !embed {
			label = strings.TrimSpace(note) + " › " + strings.TrimPrefix(frag, "^")
			if note == "" {
				label = strings.TrimPrefix(frag, "^")
			}
		}
	}
	return &Wikilink{Target: target, Label: strings.TrimSpace(label), Embed: embed}
}

type highlightParser struct{}

type highlightDelims struct{}

func (highlightDelims) IsDelimiter(b byte) bool { return b == '=' }
func (highlightDelims) CanOpenCloser(o, c *parser.Delimiter) bool {
	return o.Char == c.Char && o.OriginalLength == 2 && c.OriginalLength == 2
}
func (highlightDelims) OnMatch(int) ast.Node { return &Highlight{} }

func (highlightParser) Trigger() []byte { return []byte{'='} }

func (highlightParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	line, seg := block.PeekLine()
	node := parser.ScanDelimiter(line, before, 2, highlightDelims{})
	if node == nil || node.OriginalLength != 2 || before == '=' {
		return nil
	}
	node.Segment = seg.WithStop(seg.Start + node.OriginalLength)
	block.Advance(node.OriginalLength)
	pc.PushDelimiter(node)
	return node
}

type tagParser struct{}

func (tagParser) Trigger() []byte { return []byte{'#'} }

func (tagParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	if !(before == '\n' || unicode.IsSpace(before) || before == '(') {
		return nil
	}
	line, _ := block.PeekLine()
	s := string(line[1:])
	n, letter := 0, false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '/' {
			n += len(string(r))
			letter = letter || !unicode.IsDigit(r)
			continue
		}
		break
	}
	if n == 0 || !letter {
		return nil
	}
	block.Advance(1 + n)
	return &Tag{Name: s[:n]}
}

// ---- transformer: callouts, heading ids, link resolution ----

type transformer struct{ o Options }

var calloutRe = regexp.MustCompile(`^\[!([A-Za-z][\w-]*)\]([+-]?)[ \t]*(.*?)\s*$`)

func (t *transformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	src := reader.Source()
	var quotes []*ast.Blockquote
	ids := map[string]int{}
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Blockquote:
			quotes = append(quotes, n)
		case *ast.Heading:
			id := Slug(plainText(n, src))
			if id == "" {
				id = "section"
			}
			if c := ids[id]; c > 0 {
				ids[id] = c + 1
				id += "-" + strconv.Itoa(c)
			} else {
				ids[id] = 1
			}
			n.SetAttributeString("id", []byte(id))
		case *Wikilink:
			if n.Embed {
				n.URL, n.OK = t.o.Embed(n.Target)
				if !n.OK && !IsImage(n.Target) {
					n.URL, n.OK = t.o.Link(n.Target)
				}
			} else {
				n.URL, n.OK = t.o.Link(n.Target)
			}
		case *ast.Link:
			t.local(n, n.Destination, false)
		case *ast.Image:
			t.local(n, n.Destination, true)
		}
		return ast.WalkContinue, nil
	})
	for _, q := range quotes {
		t.callout(q, src)
	}
}

// local rewrites relative link and image destinations through the resolver.
func (t *transformer) local(n ast.Node, dest []byte, image bool) {
	d := string(dest)
	if d == "" || strings.HasPrefix(d, "#") || strings.HasPrefix(d, "/") || strings.Contains(strings.SplitN(d, "/", 2)[0], ":") {
		if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
			n.SetAttributeString("rel", []byte("noopener nofollow"))
		}
		return
	}
	if u, err := url.PathUnescape(d); err == nil {
		d = u
	}
	var href string
	var ok bool
	if image {
		href, ok = t.o.Embed(d)
	} else {
		href, ok = t.o.Link(d)
	}
	switch n := n.(type) {
	case *ast.Link:
		n.Destination = []byte(href)
		if !ok {
			n.SetAttributeString("class", []byte("unresolved"))
		}
	case *ast.Image:
		n.Destination = []byte(href)
	}
}

func (t *transformer) callout(q *ast.Blockquote, src []byte) {
	para, ok := q.FirstChild().(*ast.Paragraph)
	if !ok || para.Lines().Len() == 0 {
		return
	}
	first := para.Lines().At(0)
	m := calloutRe.FindSubmatch(first.Value(src))
	if m == nil {
		return
	}
	typ := strings.ToLower(string(m[1]))
	title := strings.TrimSpace(string(m[3]))
	if title == "" {
		title = strings.ToUpper(typ[:1]) + typ[1:]
	}
	// Drop the inline nodes of the title line from the paragraph.
	for c := para.FirstChild(); c != nil; {
		next := c.NextSibling()
		if start := firstSegmentStart(c); start >= 0 && start < first.Stop {
			para.RemoveChild(para, c)
		}
		c = next
	}
	co := &Callout{CalloutType: typ, Title: title}
	if para.ChildCount() == 0 {
		q.RemoveChild(q, para)
	}
	for c := q.FirstChild(); c != nil; {
		next := c.NextSibling()
		q.RemoveChild(q, c)
		co.AppendChild(co, c)
		c = next
	}
	q.Parent().ReplaceChild(q.Parent(), q, co)
}

func firstSegmentStart(n ast.Node) int {
	if t, ok := n.(*ast.Text); ok {
		return t.Segment.Start
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if s := firstSegmentStart(c); s >= 0 {
			return s
		}
	}
	return -1
}

func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			b.Write(c.Segment.Value(src))
		case *ast.String:
			b.Write(c.Value)
		case *Wikilink:
			b.WriteString(c.Label)
		case *Tag:
			b.WriteString("#" + c.Name)
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// ---- renderer ----

type nodeRenderer struct{}

func (nodeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindWikilink, renderWikilink)
	reg.Register(KindHighlight, func(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			w.WriteString("<mark>")
		} else {
			w.WriteString("</mark>")
		}
		return ast.WalkContinue, nil
	})
	reg.Register(KindTag, func(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			w.WriteString(`<span class="tag">#`)
			w.Write(util.EscapeHTML([]byte(n.(*Tag).Name)))
			w.WriteString("</span>")
		}
		return ast.WalkSkipChildren, nil
	})
	reg.Register(KindCallout, func(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		c := n.(*Callout)
		if entering {
			w.WriteString(`<div class="callout" data-callout="`)
			w.Write(util.EscapeHTML([]byte(c.CalloutType)))
			w.WriteString(`"><div class="callout-title">`)
			w.Write(util.EscapeHTML([]byte(c.Title)))
			w.WriteString(`</div><div class="callout-content">` + "\n")
		} else {
			w.WriteString("</div></div>\n")
		}
		return ast.WalkContinue, nil
	})
}

var sizeRe = regexp.MustCompile(`^(\d{1,4})(?:x(\d{1,4}))?$`)

func renderWikilink(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*Wikilink)
	esc := func(s string) []byte { return util.EscapeHTML([]byte(s)) }
	switch {
	case n.Embed && IsImage(n.Target) && n.OK:
		w.WriteString(`<img src="`)
		w.Write(esc(n.URL))
		w.WriteString(`"`)
		if m := sizeRe.FindStringSubmatch(n.Label); m != nil {
			w.WriteString(` width="` + m[1] + `"`)
			if m[2] != "" {
				w.WriteString(` height="` + m[2] + `"`)
			}
			w.WriteString(` alt=""`)
		} else {
			w.WriteString(` alt="`)
			w.Write(esc(path.Base(n.Label)))
			w.WriteString(`"`)
		}
		w.WriteString(` loading="lazy">`)
	case n.OK:
		cls := "internal-link"
		if n.Embed {
			cls = "internal-link embed"
		}
		w.WriteString(`<a class="` + cls + `" href="`)
		w.Write(esc(n.URL))
		w.WriteString(`">`)
		w.Write(esc(n.Label))
		w.WriteString("</a>")
	default:
		w.WriteString(`<span class="internal-link unresolved">`)
		w.Write(esc(n.Label))
		w.WriteString("</span>")
	}
	return ast.WalkSkipChildren, nil
}
