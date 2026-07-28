package sync

import "testing"

func TestIgnoreMatching(t *testing.T) {
	rules := ParseIgnore(`
# a comment, and a blank line above
node_modules/
*.log
/only-at-root.txt
build/**/*.o
!keep.log
`)

	cases := []struct {
		path    string
		isDir   bool
		exclude bool
		why     string
	}{
		{"notes.md", false, false, "an ordinary file"},
		{"debug.log", false, true, "matches *.log"},
		{"deep/inside/debug.log", false, true, "a bare pattern applies at any depth"},
		{"keep.log", false, false, "a later negation re-includes it"},
		{"node_modules", true, true, "the directory itself"},
		{"node_modules/left-pad/index.js", false, true, "anything inside an ignored directory"},
		{"src/node_modules/x.js", false, true, "at any depth, not only the root"},
		{"only-at-root.txt", false, true, "anchored, and this is the root"},
		{"sub/only-at-root.txt", false, false, "anchored, so not here"},
		{"build/x/y/thing.o", false, true, "** crosses directories"},
		{"build/thing.o", false, true, "** also matches no directories at all"},
		{"build/thing.c", false, false, "wrong extension"},

		// Platform noise is always applied, whatever the document says.
		{".DS_Store", false, true, "platform noise"},
		{"a/b/.DS_Store", false, true, "platform noise at depth"},
		{"draft.txt~", false, true, "editor backup"},
		{".#lockfile", false, true, "emacs lock"},
		{"session.swp", false, true, "vim swap"},
	}

	for _, c := range cases {
		if got := rules.Excludes(c.path, c.isDir); got != c.exclude {
			t.Errorf("Excludes(%q, dir=%v) = %v, want %v — %s",
				c.path, c.isDir, got, c.exclude, c.why)
		}
	}
}

// A directory rule must not take the whole tree with it.
func TestIgnoreDirectoryRuleDoesNotMatchFiles(t *testing.T) {
	rules := ParseIgnore("build/")

	if rules.Excludes("build", false) {
		t.Error("a trailing slash restricts the rule to directories")
	}
	if !rules.Excludes("build", true) {
		t.Error("the directory itself should be excluded")
	}
}

func TestConflictName(t *testing.T) {
	when := mustTime(t, "2026-07-27T20:13:47Z")

	cases := []struct{ path, device, want string }{
		{"notes.md", "iMac", "notes.conflict-iMac-20260727-201347.md"},
		{"a/b/notes.md", "iMac", "a/b/notes.conflict-iMac-20260727-201347.md"},
		{"no-extension", "iMac", "no-extension.conflict-iMac-20260727-201347"},
		// The device name goes into a filename, so anything that is not a
		// plain letter, digit or dash is folded to a dash.
		{"x.txt", "gabriel's box", "x.conflict-gabriel-s-box-20260727-201347.txt"},
		{"x.txt", "!!!", "x.conflict-unknown-20260727-201347.txt"},
	}

	for _, c := range cases {
		if got := ConflictName(c.path, c.device, when); got != c.want {
			t.Errorf("ConflictName(%q, %q) = %q, want %q", c.path, c.device, got, c.want)
		}
	}
}
