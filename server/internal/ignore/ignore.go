// Package ignore is the server's half of the shared ignore rules: the same
// gitignore-shaped matcher the clients run, applied to the index and to the
// volume so that a rule someone saves actually takes something away.
//
// The matcher is deliberately a port of the client's, not an approximation of
// it. Two implementations that disagree about one pattern produce a file the
// server drops and a client keeps re-uploading, forever.
package ignore

import (
	"path"
	"strings"
)

type pattern struct {
	glob string
	// A `!pattern` re-includes something an earlier rule excluded.
	negated bool
	// A trailing `/` restricts the rule to directories.
	dirsOnly bool
	// A pattern containing `/`, or anchored with a leading one, matches the
	// whole relative path. One without matches any single component at any
	// depth, which is what makes `.DS_Store` cover every directory.
	fullPath bool
}

// Rules is a parsed rule document.
type Rules struct {
	patterns []pattern
}

// Parse reads a rules document. Blank lines and `#` comments are skipped.
func Parse(rules string) *Rules {
	var parsed []pattern

	for _, line := range strings.Split(rules, "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		var p pattern
		if strings.HasPrefix(text, "!") {
			p.negated = true
			text = text[1:]
		}
		if strings.HasSuffix(text, "/") {
			p.dirsOnly = true
			text = strings.TrimSuffix(text, "/")
		}
		anchored := strings.HasPrefix(text, "/")
		text = strings.TrimPrefix(text, "/")
		if text == "" {
			continue
		}

		p.glob = text
		p.fullPath = anchored || strings.Contains(text, "/")
		parsed = append(parsed, p)
	}

	return &Rules{patterns: parsed}
}

// Excludes reports whether a path is covered by the rules.
//
// Later rules win, so a negation can re-include something an earlier pattern
// excluded, exactly as in a .gitignore.
func (r *Rules) Excludes(rel string, isDir bool) bool {
	// Anything inside an ignored directory is ignored too. Without this a rule
	// like `build/` would keep the directory itself out while its contents were
	// indexed one by one.
	if !isDir {
		parts := strings.Split(rel, "/")
		for n := 1; n < len(parts); n++ {
			if r.Excludes(strings.Join(parts[:n], "/"), true) {
				return true
			}
		}
	}

	excluded := false
	for _, p := range r.patterns {
		if p.dirsOnly && !isDir {
			continue
		}
		if p.matches(rel) {
			excluded = !p.negated
		}
	}
	return excluded
}

func (p pattern) matches(rel string) bool {
	if p.fullPath {
		return globMatch(p.glob, rel)
	}
	for _, component := range strings.Split(rel, "/") {
		if globMatch(p.glob, component) {
			return true
		}
	}
	return false
}

// globMatch is path.Match, plus the one thing it does not do.
//
// `*` in path.Match never crosses a `/`, which is what gitignore wants — but
// `**` is meant to cross precisely that. For a pattern using it, each `**` is
// matched against as many leading path segments as it takes.
func globMatch(glob, candidate string) bool {
	if !strings.Contains(glob, "**") {
		ok, err := path.Match(glob, candidate)
		return err == nil && ok
	}

	// Split on the first `**`; anything after it is handled by recursing.
	head, tail, _ := strings.Cut(glob, "**")
	head = strings.TrimSuffix(head, "/")
	tail = strings.TrimPrefix(tail, "/")

	segments := strings.Split(candidate, "/")

	// Two boundaries, not one: `**` absorbs whole segments *between* the part
	// before it and the part after. A single split point leaves the middle
	// stuck in one side or the other, which matches `build/**/*.o` against
	// `build/thing.o` but not against `build/x/y/thing.o`.
	for i := 0; i <= len(segments); i++ {
		if head == "" {
			// Nothing precedes `**`, so it starts at the beginning.
			if i != 0 {
				break
			}
		} else if !globMatch(head, strings.Join(segments[:i], "/")) {
			continue
		}

		for j := i; j <= len(segments); j++ {
			// A trailing `**` matches everything below it.
			if tail == "" {
				return true
			}
			if globMatch(tail, strings.Join(segments[j:], "/")) {
				return true
			}
		}
	}
	return false
}
