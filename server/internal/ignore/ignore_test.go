package ignore

import "testing"

func TestRulesExcludes(t *testing.T) {
	rules := Parse(`
# a comment, and a blank line above it
*.tmp
build/
.git/
node_modules
docs/drafts/
/top-only
**/generated/*.go
!keep.tmp
`)

	cases := []struct {
		path  string
		isDir bool
		want  bool
		why   string
	}{
		{"notes.md", false, false, "an ordinary file is not touched"},
		{"notes.tmp", false, true, "a glob matches at the root"},
		{"deep/inside/notes.tmp", false, true, "a bare glob matches at any depth"},
		{"keep.tmp", false, false, "a later negation re-includes"},

		{"build", true, true, "a trailing slash matches the directory"},
		{"build", false, false, "a trailing slash does not match a file of that name"},
		{"build/output.o", false, true, "everything inside an ignored directory goes with it"},
		{"src/build/output.o", false, true, "a bare rule applies at any depth"},

		{".git", true, true, "the case that started all this"},
		{".git/HEAD", false, true, "and everything under it"},
		{".git/objects/ab/cdef", false, true, "however deep"},

		{"node_modules", true, true, "a rule with no trailing slash still covers a directory"},
		{"node_modules", false, true, "and a file of the same name"},

		{"docs/drafts", true, true, "a multi-component rule matches the whole path"},
		{"docs/drafts/one.md", false, true, "and what is inside it"},
		{"other/docs/drafts", true, false, "but is not free to float: it is anchored by its slash"},

		{"top-only", false, true, "a leading slash anchors to the root"},
		{"nested/top-only", false, false, "and only the root"},

		{"generated/thing.go", false, true, "** starts at the beginning"},
		{"a/b/generated/thing.go", false, true, "and crosses any number of directories"},
		{"a/b/generated/thing.rs", false, false, "while the tail still has to match"},
	}

	for _, c := range cases {
		if got := rules.Excludes(c.path, c.isDir); got != c.want {
			t.Errorf("Excludes(%q, isDir=%v) = %v, want %v — %s",
				c.path, c.isDir, got, c.want, c.why)
		}
	}
}

// The defaults ship with the server and are what most installations run
// unmodified, so their headline entries are worth asserting directly.
func TestDefaultCoversVersionControlAndBuildTrees(t *testing.T) {
	rules := Parse(Default)

	for _, path := range []string{
		".git/HEAD", "notes/.git/config", "node_modules/react/index.js",
		".DS_Store", "src/.DS_Store", "build/app", "DerivedData/x/y",
	} {
		if !rules.Excludes(path, false) {
			t.Errorf("the default rules should exclude %q", path)
		}
	}

	for _, path := range []string{"notes.md", "photos/holiday.jpg", "gitignore.md"} {
		if rules.Excludes(path, false) {
			t.Errorf("the default rules should not exclude %q", path)
		}
	}
}
