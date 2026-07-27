package index

import (
	"errors"
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ErrInvalidPath is returned for any path that could escape the data root or
// that we refuse to store.
var ErrInvalidPath = errors.New("invalid path")

// reservedWindowsNames are illegal as a filename component on Windows, with or
// without an extension (NUL.txt is just as illegal as NUL).
var reservedWindowsNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// windowsUnsafeChars cannot appear in a Windows filename.
const windowsUnsafeChars = `<>:"|?*`

// CleanPath validates and canonicalises a client-supplied path.
//
// Everything in the index is stored in NFC. macOS hands us NFD (an "ç" arrives
// as "c" plus a combining cedilla) while Linux and Windows use NFC, so without
// normalising here the same file uploaded from two platforms would occupy two
// different rows. Clients convert back to whatever their filesystem wants when
// they write to disk.
func CleanPath(raw string) (string, error) {
	if raw == "" {
		return "", ErrInvalidPath
	}

	p := norm.NFC.String(raw)
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.TrimPrefix(p, "/")

	// path.Clean resolves any "." and ".." it can; anything left pointing above
	// the root, or any absolute path, is a traversal attempt.
	p = path.Clean(p)
	if p == "." || p == "/" || strings.HasPrefix(p, "../") || p == ".." {
		return "", ErrInvalidPath
	}

	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", ErrInvalidPath
		}
		// NUL and newlines would let a path smuggle itself past naive parsing
		// further down the line.
		if strings.ContainsAny(part, "\x00\n\r") {
			return "", ErrInvalidPath
		}
	}

	return p, nil
}

// IsWindowsUnsafe reports whether a path would be rejected by a Windows
// filesystem. We still accept and store these — a Mac-only setup has every
// right to use them — but the entry is flagged so that a future Windows client
// can escape the name instead of failing with no explanation.
func IsWindowsUnsafe(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if strings.ContainsAny(part, windowsUnsafeChars) {
			return true
		}
		// A trailing space or dot is silently stripped by Windows, which turns
		// two distinct names into one.
		if strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") {
			return true
		}
		stem, _, _ := strings.Cut(part, ".")
		if reservedWindowsNames[strings.ToUpper(stem)] {
			return true
		}
	}
	return false
}

// FoldPath returns the case-insensitive key for a path. Two paths sharing a
// fold key would collide on a case-insensitive volume (the default on macOS
// and Windows), so the index rejects the second one rather than let the two
// silently become one file.
func FoldPath(p string) string {
	return strings.ToLower(p)
}
