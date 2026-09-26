package web

import (
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/jirkacepelka/obsisync/server/internal/markdown"
	"github.com/jirkacepelka/obsisync/server/internal/pathutil"
	"github.com/jirkacepelka/obsisync/server/internal/store"
)

// hiddenPath reports whether p is inside a hidden folder (.obsidian, .trash,
// .git …) or is a hidden file. Those are not notes and stay out of the
// editor and of published sites.
func hiddenPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// fileIndex resolves note links the way Obsidian does: by path, relative to
// the linking note, or by file name anywhere in the vault.
type fileIndex struct {
	byPath map[string]string   // folded path → path
	byName map[string][]string // folded base name, with and without ".md" → paths
}

func newFileIndex(paths []string) *fileIndex {
	ix := &fileIndex{byPath: map[string]string{}, byName: map[string][]string{}}
	for _, p := range paths {
		ix.byPath[pathutil.Fold(p)] = p
		name := pathutil.Fold(path.Base(p))
		ix.byName[name] = append(ix.byName[name], p)
		if stem, ok := strings.CutSuffix(name, ".md"); ok {
			ix.byName[stem] = append(ix.byName[stem], p)
		}
	}
	for _, ps := range ix.byName {
		sort.Slice(ps, func(i, j int) bool {
			if len(ps[i]) != len(ps[j]) {
				return len(ps[i]) < len(ps[j])
			}
			return ps[i] < ps[j]
		})
	}
	return ix
}

func filesIndex(files []store.FileEntry, keep func(string) bool) *fileIndex {
	var paths []string
	for _, f := range files {
		if !f.Deleted && !hiddenPath(f.Path) && (keep == nil || keep(f.Path)) {
			paths = append(paths, f.Path)
		}
	}
	return newFileIndex(paths)
}

// resolve finds the file a link in note "from" points to. target may carry
// a "#heading" suffix, which is returned separately as an anchor slug.
func (ix *fileIndex) resolve(from, target string) (p, anchor string, ok bool) {
	note, anchor := markdown.SplitTarget(target)
	if note == "" {
		return from, anchor, from != ""
	}
	note = strings.TrimPrefix(note, "/")
	dir := path.Dir(from)
	try := []string{note, note + ".md"}
	if dir != "." && dir != "" {
		try = append([]string{path.Join(dir, note), path.Join(dir, note) + ".md"}, try...)
	}
	for _, t := range try {
		if p, ok := ix.byPath[pathutil.Fold(path.Clean(t))]; ok {
			return p, anchor, true
		}
	}
	name := pathutil.Fold(path.Base(note))
	cands := ix.byName[name]
	if strings.Contains(note, "/") {
		var filtered []string
		suffix := "/" + pathutil.Fold(note)
		for _, c := range cands {
			fc := pathutil.Fold(c)
			if strings.HasSuffix(fc, suffix) || strings.HasSuffix(fc, suffix+".md") {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
	}
	if len(cands) == 0 {
		return "", "", false
	}
	for _, c := range cands {
		if path.Dir(c) == dir {
			return c, anchor, true
		}
	}
	return cands[0], anchor, true
}

// escapePath escapes every segment of a vault path for use in a URL path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func withAnchor(u, anchor string) string {
	if anchor == "" {
		return u
	}
	return u + "#" + anchor
}
