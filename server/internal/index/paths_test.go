package index

import (
	"errors"
	"testing"
)

func TestCleanPath(t *testing.T) {
	t.Run("accepts and canonicalises", func(t *testing.T) {
		cases := map[string]struct{ in, want string }{
			"plain file":        {"notes.md", "notes.md"},
			"nested":            {"projects/alpha/notes.md", "projects/alpha/notes.md"},
			"leading slash":     {"/notes.md", "notes.md"},
			"backslashes":       {`projects\alpha\notes.md`, "projects/alpha/notes.md"},
			"redundant slashes": {"projects//alpha///notes.md", "projects/alpha/notes.md"},
			"inner dot":         {"projects/./notes.md", "projects/notes.md"},
			// ".." that stays inside the root is resolved, not rejected.
			"contained parent": {"projects/alpha/../notes.md", "projects/notes.md"},
			"dotfile":          {".gitignore", ".gitignore"},
			"spaces":           {"my notes/draft two.md", "my notes/draft two.md"},
		}

		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				got, err := CleanPath(tc.in)
				if err != nil {
					t.Fatalf("CleanPath(%q): unexpected error %v", tc.in, err)
				}
				if got != tc.want {
					t.Errorf("CleanPath(%q) = %q, want %q", tc.in, got, tc.want)
				}
			})
		}
	})

	t.Run("normalises to NFC", func(t *testing.T) {
		// The whole point: macOS reports filenames decomposed, everything else
		// composed. Both spellings must produce one canonical form.
		const (
			decomposed = "ação.txt"
			composed   = "ação.txt"
		)

		if decomposed == composed {
			t.Fatal("the two spellings are identical; this test would prove nothing")
		}

		for _, in := range []string{decomposed, composed} {
			got, err := CleanPath(in)
			if err != nil {
				t.Fatalf("CleanPath(%q): %v", in, err)
			}
			if got != composed {
				t.Errorf("CleanPath(%q) = %q, want the composed form %q", in, got, composed)
			}
		}
	})

	t.Run("rejects", func(t *testing.T) {
		cases := map[string]string{
			"empty":              "",
			"root":               "/",
			"bare dot":           ".",
			"bare parent":        "..",
			"escaping parent":    "../secrets",
			"deep escape":        "a/../../secrets",
			"escape after slash": "/../secrets",
			"NUL byte":           "notes\x00.md",
			"newline":            "notes\n.md",
		}

		for name, in := range cases {
			t.Run(name, func(t *testing.T) {
				got, err := CleanPath(in)
				if !errors.Is(err, ErrInvalidPath) {
					t.Errorf("CleanPath(%q) = %q, %v; want ErrInvalidPath", in, got, err)
				}
			})
		}
	})
}

func TestIsWindowsUnsafe(t *testing.T) {
	unsafe := []string{
		`report:final.txt`,
		`what?.txt`,
		`a<b.txt`,
		`a>b.txt`,
		`a|b.txt`,
		`a"b.txt`,
		`star*.txt`,
		// Windows silently strips a trailing space or dot from a name, which
		// turns two distinct paths into one.
		"trailing space ",
		"trailing dot.",
		"folder /notes.md",
		"CON",
		"con.txt",
		"NUL.log",
		"aux",
		"COM1.txt",
		"LPT9",
		// Anywhere in the path, not just the last component.
		"projects/CON/notes.md",
		"projects/a:b/notes.md",
	}

	for _, path := range unsafe {
		t.Run(path, func(t *testing.T) {
			if !IsWindowsUnsafe(path) {
				t.Errorf("IsWindowsUnsafe(%q) = false, want true", path)
			}
		})
	}

	safe := []string{
		"notes.md",
		"projects/alpha/notes.md",
		".gitignore",
		"my notes.md",
		"console.log",         // starts with "con" but is not "CON"
		"contact.txt",         // likewise
		"COM10.txt",           // only COM1-COM9 are reserved
		"a-b_c(1).txt",        // punctuation Windows is fine with
		"trailing space .txt", // the space is not at the end of the component
	}

	for _, path := range safe {
		t.Run(path, func(t *testing.T) {
			if IsWindowsUnsafe(path) {
				t.Errorf("IsWindowsUnsafe(%q) = true, want false", path)
			}
		})
	}
}

func TestFoldPath(t *testing.T) {
	// Paths that must collide, because a case-insensitive volume would store
	// them as one file.
	collide := [][2]string{
		{"notes.md", "NOTES.md"},
		{"notes.md", "Notes.MD"},
		{"projects/alpha/x.txt", "Projects/Alpha/X.txt"},
	}

	for _, pair := range collide {
		if FoldPath(pair[0]) != FoldPath(pair[1]) {
			t.Errorf("FoldPath(%q) and FoldPath(%q) differ; they must collide", pair[0], pair[1])
		}
	}

	distinct := [][2]string{
		{"notes.md", "notes2.md"},
		{"a/notes.md", "b/notes.md"},
	}

	for _, pair := range distinct {
		if FoldPath(pair[0]) == FoldPath(pair[1]) {
			t.Errorf("FoldPath(%q) and FoldPath(%q) collide; they must not", pair[0], pair[1])
		}
	}
}
