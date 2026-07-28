package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schmidt-gabriel/homesync/server/internal/crypt"
)

func newKey(t *testing.T) *crypt.Key {
	t.Helper()
	text, err := crypt.GenerateKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	key, err := crypt.ParseKey(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &key
}

func read(t *testing.T, s *Store, rel string) []byte {
	t.Helper()
	r, _, err := s.Open(rel)
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return got
}

// Whatever lands on disk, what the store reports has to describe the plaintext:
// it is what the client compares against and what goes in the index.
func TestWriteReportsPlaintextSizeAndHash(t *testing.T) {
	content := bytes.Repeat([]byte("homesync "), 20000) // spans several chunks
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	for _, encrypted := range []bool{false, true} {
		var key *crypt.Key
		if encrypted {
			key = newKey(t)
		}

		s, err := New(t.TempDir(), key)
		if err != nil {
			t.Fatalf("new store: %v", err)
		}

		result, err := s.Write("a/b/notes.txt", bytes.NewReader(content))
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if result.Size != int64(len(content)) {
			t.Errorf("encrypted=%v: size %d, want %d", encrypted, result.Size, len(content))
		}
		if result.SHA256 != want {
			t.Errorf("encrypted=%v: hash %s, want %s", encrypted, result.SHA256, want)
		}
		if got := read(t, s, "a/b/notes.txt"); !bytes.Equal(got, content) {
			t.Errorf("encrypted=%v: contents differ on the way back", encrypted)
		}

		// And the bytes on disk are, or are not, the ones we were given.
		raw, err := os.ReadFile(filepath.Join(s.Root(), "a", "b", "notes.txt"))
		if err != nil {
			t.Fatalf("read raw: %v", err)
		}
		leaked := bytes.Contains(raw, []byte("homesync homesync"))
		if encrypted && leaked {
			t.Error("the plaintext is sitting on the encrypted volume")
		}
		if !encrypted && !leaked {
			t.Error("an unencrypted store did not write the plaintext")
		}
	}
}

// Turning encryption on must not strand what is already there. Both forms have
// to be readable through the same store, because that is the state every
// existing server is in the moment the key is set.
func TestEncryptedStoreReadsPlaintextFiles(t *testing.T) {
	root := t.TempDir()

	plain, err := New(root, nil)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := plain.Write("before.txt", strings.NewReader("written before the key")); err != nil {
		t.Fatalf("write: %v", err)
	}

	encrypted, err := New(root, newKey(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := encrypted.Write("after.txt", strings.NewReader("written after the key")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := string(read(t, encrypted, "before.txt")); got != "written before the key" {
		t.Errorf("old file read back as %q", got)
	}
	if got := string(read(t, encrypted, "after.txt")); got != "written after the key" {
		t.Errorf("new file read back as %q", got)
	}
}

func TestEncryptedFileNeedsItsKey(t *testing.T) {
	root := t.TempDir()

	encrypted, err := New(root, newKey(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := encrypted.Write("secret.txt", strings.NewReader("contents")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A server restarted without ENCRYPTION_KEY must refuse rather than serve
	// ciphertext as if it were the file.
	plain, err := New(root, nil)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, _, err := plain.Open("secret.txt"); err == nil {
		t.Fatal("an encrypted file was opened without a key")
	}
}
