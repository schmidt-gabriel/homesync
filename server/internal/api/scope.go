package api

import (
	"fmt"
	"strings"

	"github.com/schmidt-gabriel/homesync/server/internal/index"
)

// Scopes give each device a subtree of the data directory.
//
// A device's scope is prepended to every path it sends and stripped from every
// path it receives, so a client never knows it is not at the root. Two devices
// pointed at the same scope therefore sync the same files, which is how the
// multi-machine case is expressed: by configuration, rather than being the only
// thing on offer.

// DefaultScope turns a device name into a directory name. Kept readable rather
// than hashed, because it is a real folder someone will browse on the server.
func DefaultScope(deviceName string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		case r == ' ' || r == '.':
			return '-'
		default:
			return -1
		}
	}, deviceName)

	cleaned = strings.Trim(cleaned, "-")
	for strings.Contains(cleaned, "--") {
		cleaned = strings.ReplaceAll(cleaned, "--", "-")
	}

	if cleaned == "" {
		return "device"
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}

// ValidateScope checks a scope a human typed. An empty scope means the whole
// data directory, which is allowed but has to be chosen deliberately.
func ValidateScope(scope string) (string, error) {
	if scope == "" {
		return "", nil
	}

	cleaned, err := index.CleanPath(scope)
	if err != nil {
		return "", fmt.Errorf("invalid scope %q: %w", scope, err)
	}
	return cleaned, nil
}

// resolve turns a path the device sent into a path in the index.
func (d Device) resolve(path string) (string, error) {
	cleaned, err := index.CleanPath(path)
	if err != nil {
		return "", err
	}
	if d.Scope == "" {
		return cleaned, nil
	}
	return index.CleanPath(d.Scope + "/" + cleaned)
}

// strip turns an index path back into what the device should see. It returns
// false for anything outside the scope, which must never be sent to it.
func (d Device) strip(path string) (string, bool) {
	if d.Scope == "" {
		return path, true
	}
	// The scope directory itself is not part of what the device syncs; it is
	// the device's root, and a client has no entry for its own root.
	if path == d.Scope {
		return "", false
	}
	prefix := d.Scope + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}

// stripEntries rewrites a batch for one device, dropping anything outside its
// scope.
func (d Device) stripEntries(entries []index.Entry) []index.Entry {
	result := make([]index.Entry, 0, len(entries))
	for _, entry := range entries {
		relative, ok := d.strip(entry.Path)
		if !ok {
			continue
		}
		entry.Path = relative
		result = append(result, entry)
	}
	return result
}
