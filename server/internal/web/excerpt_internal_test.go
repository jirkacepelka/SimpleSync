package web

import "testing"

func TestExcerpt(t *testing.T) {
	got := excerpt("# Garden plan\n\nThree **raised** beds. See [[Groceries]] and [[Projects/Home server|the server notes]], or [docs](https://x.y).\n\nSecond paragraph.")
	want := "Three raised beds. See Groceries and the server notes, or docs."
	if got != want {
		t.Fatalf("excerpt: %q", got)
	}
}
