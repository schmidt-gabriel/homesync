package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Device is a registered client. One per machine, so a lost laptop can be
// revoked without disturbing the others.
type Device struct {
	ID   string
	Name string
	// Scope is the subtree of the data directory this device syncs. Empty
	// means the whole tree.
	Scope string
}

type contextKey struct{ name string }

var deviceContextKey = contextKey{"device"}

// DeviceFrom returns the authenticated device for a request.
func DeviceFrom(ctx context.Context) (Device, bool) {
	d, ok := ctx.Value(deviceContextKey).(Device)
	return d, ok
}

// hashToken returns the value we actually persist. The plaintext token is
// shown once at creation and never stored, so a leaked database does not hand
// over working credentials.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken mints a device token: 32 bytes of entropy, URL-safe.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AddDevice registers a device and returns the one-time plaintext token.
// The scope defaults to a directory named after the device.
func AddDevice(ctx context.Context, db *sql.DB, name, scope string) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	id, err := NewToken()
	if err != nil {
		return "", err
	}
	id = id[:16]

	_, err = db.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, created_at, scope) VALUES (?, ?, ?, ?, ?)`,
		id, name, hashToken(token), time.Now().UnixMilli(), scope)
	if err != nil {
		return "", err
	}
	return token, nil
}

// ListDevices returns every registered device, newest first.
func ListDevices(ctx context.Context, db *sql.DB) ([]Device, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, scope FROM devices ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.Scope); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// SetScope repoints a device at a different subtree. Pointing two devices at
// the same one is what makes them share files.
func SetScope(ctx context.Context, db *sql.DB, name, scope string) (int64, error) {
	res, err := db.ExecContext(ctx, `UPDATE devices SET scope = ? WHERE name = ?`, scope, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RemoveDevice revokes a device by name, returning how many were removed.
// Its files are left alone: revoking access is not deleting data.
func RemoveDevice(ctx context.Context, db *sql.DB, name string) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM devices WHERE name = ?`, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// authenticate looks up the bearer token and returns the owning device.
func (s *Server) authenticate(ctx context.Context, header string) (Device, error) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return Device{}, errors.New("missing bearer token")
	}

	var d Device
	var storedHash string
	err := s.index.DB().QueryRowContext(ctx,
		`SELECT id, name, scope, token_hash FROM devices WHERE token_hash = ?`,
		hashToken(token)).Scan(&d.ID, &d.Name, &d.Scope, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, errors.New("unknown token")
	}
	if err != nil {
		return Device{}, err
	}

	// The lookup above already matched on the hash; this constant-time compare
	// costs nothing and keeps the comparison honest if the query ever changes.
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(hashToken(token))) != 1 {
		return Device{}, errors.New("unknown token")
	}

	return d, nil
}

// requireAuth wraps a handler so it only runs for a known device.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device, err := s.authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="homesync"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}

		// Best effort: a failed heartbeat must never fail the request.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.index.DB().ExecContext(ctx,
				`UPDATE devices SET last_seen = ? WHERE id = ?`, time.Now().UnixMilli(), device.ID)
		}()

		next(w, r.WithContext(context.WithValue(r.Context(), deviceContextKey, device)))
	}
}
