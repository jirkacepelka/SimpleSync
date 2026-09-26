package markdown

import (
	"strings"
	"testing"
)

func opts() Options {
	return Options{
		Link: func(t string) (string, bool) {
			note, anchor := SplitTarget(t)
			if note != "Garden plan" && note != "Garden plan.md" && note != "" {
				return "", false
			}
			u := "/n/Garden%20plan"
			if anchor != "" {
				u += "#" + anchor
			}
			return u, true
		},
		Embed: func(t string) (string, bool) {
			if t == "photo.png" || t == "img/beds.jpg" {
				return "/f/" + t, true
			}
			return "", false
		},
	}
}

func render(t *testing.T, src string, o Options) string {
	t.Helper()
	r, err := Render([]byte(src), o)
	if err != nil {
		t.Fatal(err)
	}
	return r.HTML
}

func TestWikilinks(t *testing.T) {
	h := render(t, "See [[Garden plan]], [[Garden plan#Before spring|the list]] and [[Missing note]].", opts())
	for _, want := range []string{
		`<a class="internal-link" href="/n/Garden%20plan">Garden plan</a>`,
		`<a class="internal-link" href="/n/Garden%20plan#before-spring">the list</a>`,
		`<span class="internal-link unresolved">Missing note</span>`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %s in %s", want, h)
		}
	}
}

func TestEmbedsAndImages(t *testing.T) {
	h := render(t, "![[photo.png|300]]\n\n![beds](img/beds.jpg)\n\n![[secret.png]]", opts())
	for _, want := range []string{`<img src="/f/photo.png" width="300"`, `<img src="/f/img/beds.jpg" alt="beds"`, `<span class="internal-link unresolved">secret.png</span>`} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %s in %s", want, h)
		}
	}
}

func TestCalloutHighlightTag(t *testing.T) {
	h := render(t, "> [!warning] Frost tonight\n> Cover the **tomatoes**.\n\nThis is ==important== #garden/beds but not a#tag.", opts())
	for _, want := range []string{
		`<div class="callout" data-callout="warning"><div class="callout-title">Frost tonight</div>`,
		`Cover the <strong>tomatoes</strong>.`,
		`<mark>important</mark>`,
		`<span class="tag">#garden/beds</span>`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %s in %s", want, h)
		}
	}
	if strings.Contains(h, "[!warning]") || strings.Contains(h, "<blockquote>") {
		t.Errorf("callout marker leaked: %s", h)
	}
	if strings.Contains(h, `#tag</span>`) {
		t.Errorf("tag inside a word: %s", h)
	}
	if h := render(t, "> [!tip]\n> Water in the morning.", opts()); !strings.Contains(h, `callout-title">Tip</div>`) {
		t.Errorf("default callout title: %s", h)
	}
}

func TestHeadingsAndTasks(t *testing.T) {
	h := render(t, "# Before spring\n## Before spring\n- [x] Order seeds\n- [ ] Fix the barrel\n", opts())
	for _, want := range []string{`<h1 id="before-spring">`, `<h2 id="before-spring-1">`, `checked="" disabled="" type="checkbox"`} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %s in %s", want, h)
		}
	}
}

func TestUnsafeContentIsNeutralised(t *testing.T) {
	h := render(t, "<script>alert(1)</script>\n\n[x](javascript:alert(1)) <img src=x onerror=alert(1)>\n\n[[<b>bold</b>]]", opts())
	for _, bad := range []string{"<script", "javascript:", "onerror", "<b>"} {
		if strings.Contains(h, bad) {
			t.Errorf("unsafe %q passed through: %s", bad, h)
		}
	}
}

func TestFrontmatter(t *testing.T) {
	src := "---\ntitle: \"Garden plan\"\npublish: true\ntags:\n  - garden\n  - spring\naliases: [beds, plot]\n---\n# Body\n"
	fm, body := SplitFrontmatter([]byte(src))
	if !fm.Bool("publish") || fm.Get("title") != "Garden plan" || fm.Get("tags") != "garden, spring" || fm.Get("aliases") != "beds, plot" {
		t.Fatalf("front matter: %+v", fm)
	}
	if string(body) != "# Body\n" {
		t.Fatalf("body: %q", body)
	}
	o := opts()
	o.Properties = true
	h := render(t, src, o)
	if !strings.Contains(h, `<th>publish</th><td>true</td>`) || strings.Contains(h, "<hr>") {
		t.Fatalf("properties: %s", h)
	}
	if fm, body := SplitFrontmatter([]byte("---\nno end")); fm != nil || string(body) != "---\nno end" {
		t.Fatal("unterminated front matter must be kept as text")
	}
}
