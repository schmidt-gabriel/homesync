package crypt

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func testKey(t *testing.T) Key {
	t.Helper()
	text, err := GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	key, err := ParseKey(text)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	return key
}

// encryptToFile writes plaintext through Encrypt and returns where it landed.
func encryptToFile(t *testing.T, key Key, plain []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "file.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()

	written, err := Encrypt(f, bytes.NewReader(plain), key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if written != int64(len(plain)) {
		t.Fatalf("Encrypt reported %d bytes of plaintext, gave it %d", written, len(plain))
	}
	return path
}

func readAll(t *testing.T, path string, key *Key) ([]byte, int64) {
	t.Helper()

	r, size, err := Open(path, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got, size
}

// The sizes worth checking are the ones on a chunk boundary, because that is
// where the arithmetic that recovers the plaintext length changes shape.
func TestRoundTrip(t *testing.T) {
	key := testKey(t)

	sizes := []int{
		0, 1, 15, 16, 17,
		ChunkSize - 1, ChunkSize, ChunkSize + 1,
		2*ChunkSize - 1, 2 * ChunkSize, 2*ChunkSize + 1,
		5*ChunkSize + 1234,
	}

	for _, size := range sizes {
		plain := make([]byte, size)
		if _, err := rand.Read(plain); err != nil {
			t.Fatalf("random: %v", err)
		}

		path := encryptToFile(t, key, plain)
		got, reported := readAll(t, path, &key)

		if reported != int64(size) {
			t.Errorf("size %d: reported %d", size, reported)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("size %d: contents differ", size)
		}

		// The size has to be knowable without decrypting, because the scan
		// asks it for every file on every pass.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		cheap, err := SizeOf(path, info)
		if err != nil {
			t.Fatalf("size %d: SizeOf: %v", size, err)
		}
		if cheap != int64(size) {
			t.Errorf("size %d: SizeOf reported %d", size, cheap)
		}
	}
}

// The whole point of the exercise: the bytes on the volume are not the bytes
// the client sent.
func TestCiphertextDoesNotContainPlaintext(t *testing.T) {
	key := testKey(t)
	secret := []byte("the quick brown fox jumps over the lazy dog")

	raw, err := os.ReadFile(encryptToFile(t, key, secret))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Contains(raw, secret) {
		t.Error("the plaintext is sitting in the encrypted file")
	}
	if !IsEncrypted(raw) {
		t.Error("the file is not recognised as encrypted")
	}
}

func TestPlaintextFilesAreReadUntouched(t *testing.T) {
	key := testKey(t)
	path := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(path, []byte("written before encryption was on"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Both with and without a key: turning encryption on must not make the
	// files that were already there unreadable.
	for _, k := range []*Key{nil, &key} {
		got, size, err := func() ([]byte, int64, error) {
			r, size, err := Open(path, k)
			if err != nil {
				return nil, 0, err
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			return got, size, err
		}()
		if err != nil {
			t.Fatalf("open plaintext: %v", err)
		}
		if string(got) != "written before encryption was on" {
			t.Errorf("got %q", got)
		}
		if size != 32 {
			t.Errorf("size %d", size)
		}
	}
}

func TestEncryptedFileWithoutKeyIsRefused(t *testing.T) {
	path := encryptToFile(t, testKey(t), []byte("secret"))

	if _, _, err := Open(path, nil); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("expected ErrKeyMissing, got %v", err)
	}
}

func TestWrongKeyIsRefused(t *testing.T) {
	path := encryptToFile(t, testKey(t), []byte("secret"))
	other := testKey(t)

	// Refused at open, since opening now authenticates the final chunk.
	r, _, err := Open(path, &other)
	if err != nil {
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("expected ErrCorrupt, got %v", err)
		}
		return
	}
	defer r.Close()

	if _, err := io.ReadAll(r); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a wrong key should not decrypt anything, got %v", err)
	}
}

// Authentication is not decoration: a flipped byte anywhere in the body has to
// be an error rather than plausible-looking rubbish.
func TestTamperingIsDetected(t *testing.T) {
	key := testKey(t)
	plain := make([]byte, 3*ChunkSize)
	if _, err := rand.Read(plain); err != nil {
		t.Fatalf("random: %v", err)
	}

	for _, at := range []int64{int64(headerSize), int64(headerSize + 100), int64(headerSize + sealedSize + 7)} {
		path := encryptToFile(t, key, plain)

		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open for tampering: %v", err)
		}
		var b [1]byte
		if _, err := f.ReadAt(b[:], at); err != nil {
			t.Fatalf("read: %v", err)
		}
		b[0] ^= 0xff
		if _, err := f.WriteAt(b[:], at); err != nil {
			t.Fatalf("write: %v", err)
		}
		f.Close()

		r, _, err := Open(path, &key)
		if err != nil {
			// Corrupting the header can fail at open, which is also a refusal.
			continue
		}
		_, err = io.ReadAll(r)
		r.Close()
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("byte at %d flipped and the file still read: %v", at, err)
		}
	}
}

// Cutting chunks off the end is the failure that authenticating each chunk on
// its own would miss, which is why the last one is sealed as final.
func TestTruncationIsDetected(t *testing.T) {
	key := testKey(t)
	plain := make([]byte, 3*ChunkSize)
	if _, err := rand.Read(plain); err != nil {
		t.Fatalf("random: %v", err)
	}
	path := encryptToFile(t, key, plain)

	// Drop the last chunk, leaving a file that is otherwise well formed.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Truncate(path, info.Size()-sealedSize); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	r, size, err := Open(path, &key)
	if err != nil {
		return // refused outright, which is fine
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err == nil {
		t.Fatalf("a truncated file read back %d of %d bytes without complaint", len(got), size)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("expected ErrCorrupt, got %v", err)
	}
}

// http.ServeContent seeks to work out the length and to serve ranges, so this
// is what keeps range requests working on an encrypted volume.
func TestSeek(t *testing.T) {
	key := testKey(t)
	plain := make([]byte, 2*ChunkSize+500)
	if _, err := rand.Read(plain); err != nil {
		t.Fatalf("random: %v", err)
	}
	path := encryptToFile(t, key, plain)

	r, size, err := Open(path, &key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("seek end: %v", err)
	}
	if end != size || end != int64(len(plain)) {
		t.Fatalf("seek to end gave %d, want %d", end, len(plain))
	}

	// Read a window that straddles a chunk boundary, from a cold start and
	// then backwards, so a stale cached chunk would show up.
	for _, at := range []int64{0, 10, ChunkSize - 5, ChunkSize, 2 * ChunkSize, 500} {
		if _, err := r.Seek(at, io.SeekStart); err != nil {
			t.Fatalf("seek %d: %v", at, err)
		}
		want := plain[at:min(at+64, int64(len(plain)))]
		got := make([]byte, len(want))
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatalf("read at %d: %v", at, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("bytes at %d differ", at)
		}
	}
}

func TestParseKey(t *testing.T) {
	valid, err := GenerateKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := ParseKey(valid); err != nil {
		t.Errorf("a generated key did not parse: %v", err)
	}

	// Hex is accepted too, since that is what most people paste.
	if _, err := ParseKey("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"); err != nil {
		t.Errorf("hex key rejected: %v", err)
	}

	for _, bad := range []string{"", "hunter2", "c2hvcnQ=", "zz"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("%q was accepted as a key", bad)
		}
	}
}

func TestTwoFilesNeverShareCiphertext(t *testing.T) {
	key := testKey(t)
	plain := []byte("identical contents on both")

	first, err := os.ReadFile(encryptToFile(t, key, plain))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	second, err := os.ReadFile(encryptToFile(t, key, plain))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// The per-file salt is what makes this true, and it is what lets the
	// nonce be a plain counter.
	if bytes.Equal(first, second) {
		t.Error("the same plaintext encrypted to the same bytes twice")
	}
}
