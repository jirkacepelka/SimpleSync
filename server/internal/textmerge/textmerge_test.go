package textmerge

import "testing"

func TestMerge(t *testing.T) {
	base := "# Groceries\n- Oat milk\n- Coffee beans\n"
	cases := []struct {
		name, ours, theirs, want string
		ok                       bool
	}{
		{"different lines", "# Groceries\n- Oat milk\n- Lemons\n- Coffee beans\n", "# Groceries\n- Oat milk\n- Coffee beans\n- Basil\n",
			"# Groceries\n- Oat milk\n- Lemons\n- Coffee beans\n- Basil\n", true},
		{"only ours", "# Groceries\n- Oat milk\n", base, "# Groceries\n- Oat milk\n", true},
		{"only theirs", base, "# Shopping\n- Oat milk\n- Coffee beans\n", "# Shopping\n- Oat milk\n- Coffee beans\n", true},
		{"same change", "# Groceries\n- Soy milk\n- Coffee beans\n", "# Groceries\n- Soy milk\n- Coffee beans\n", "# Groceries\n- Soy milk\n- Coffee beans\n", true},
		{"same line", "# Groceries\n- Soy milk\n- Coffee beans\n", "# Groceries\n- Rice milk\n- Coffee beans\n", "", false},
		{"adjacent edit and delete", "# Groceries\n- Oat milk!\n- Coffee beans\n", "# Groceries\n- Oat milk\n", "", false},
		{"edit and delete apart", "# Shopping\n- Oat milk\n- Coffee beans\n", "# Groceries\n- Oat milk\n", "# Shopping\n- Oat milk\n", true},
		{"no trailing newline", "a\nb\nc", "a\nB\nc", "", false},
	}
	for _, c := range cases {
		b := base
		if c.name == "no trailing newline" {
			b = "a\nb\nc"
			c.ours = "a\nX\nc"
		}
		got, ok := Merge(b, c.ours, c.theirs)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestMergeAppendBoth(t *testing.T) {
	got, ok := Merge("a\n", "a\nb\n", "a\nc\n")
	if ok {
		t.Fatalf("appending different lines at the same place must conflict, got %q", got)
	}
}
