// Package crypt encrypts file contents where they rest on the server's volume.
//
// What this protects against, and what it does not, is worth being exact
// about. The server holds the key, so it can read every file it serves: this
// defends a stolen disk, a copied backup, or another container that has the
// volume mounted. It does not defend against someone who has the running
// server, and it is not end-to-end — clients send and receive plaintext, and
// the transport is HTTPS's job.
//
// Filenames and the directory tree stay in the clear. Encrypting them would
// mean the volume no longer shows what is in it, and the shape of the tree is
// already in the index, which is not encrypted either.
//
// # Format
//
//	magic     6 bytes  "HSYNCE"
//	version   1 byte   1
//	reserved  1 byte   0
//	salt     16 bytes  random, unique per file
//	chunks             AES-256-GCM, 64 KiB of plaintext each
//
// The salt derives a per-file key with HKDF, so the nonce can simply be the
// chunk counter: two files never share a key, and within a file no counter
// repeats. Each chunk is sealed with the header, its own index, and whether it
// is the last one as additional data, which is what stops chunks being
// reordered, swapped between files, or dropped off the end.
//
// The plaintext length is not stored. It follows from the file's size, since
// every chunk but the last holds exactly ChunkSize bytes — one less thing that
// can disagree with the bytes it describes.
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// KeySize is the length of the master key, in bytes.
const KeySize = 32

// ChunkSize is how much plaintext each sealed chunk holds. Large enough that
// the per-chunk overhead is noise, small enough that a reader seeking into a
// big file decrypts a little rather than a lot.
const ChunkSize = 64 << 10

const (
	magic      = "HSYNCE"
	version    = 1
	saltSize   = 16
	tagSize    = 16
	headerSize = len(magic) + 1 + 1 + saltSize // 24
	sealedSize = ChunkSize + tagSize
)

// ErrKeyMissing means a file on disk is encrypted and the server was started
// without the key that would read it.
var ErrKeyMissing = errors.New("file is encrypted but no encryption key is configured")

// ErrCorrupt means the bytes are not a well-formed encrypted file. Failing to
// decrypt is reported the same way: with authenticated encryption there is no
// difference between damaged and tampered with, and treating them alike is the
// point.
var ErrCorrupt = errors.New("encrypted file is damaged or was tampered with")

// Key is the master key. Every file gets its own key derived from this one.
type Key [KeySize]byte

// ParseKey reads a key from its text form, either base64 or hex.
//
// It deliberately does not accept a passphrase. A key stretched from something
// memorable is only as strong as the thing it was stretched from, and hiding
// that behind an env var that takes any string invites a weak one. The
// generator prints a real key; storing it is the operator's job.
func ParseKey(text string) (Key, error) {
	var key Key

	for _, decode := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	} {
		raw, err := decode(text)
		if err != nil || len(raw) != KeySize {
			continue
		}
		copy(key[:], raw)
		return key, nil
	}

	return key, fmt.Errorf("an encryption key must be %d bytes in base64 or hex; "+
		"run `homesync key generate` to make one", KeySize)
}

// GenerateKey returns a fresh key in the form ParseKey reads back.
func GenerateKey() (string, error) {
	raw := make([]byte, KeySize)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// aeadFor derives the per-file cipher from the master key and a file's salt.
func aeadFor(master Key, salt []byte) (cipher.AEAD, error) {
	derived, err := hkdf.Key(sha256.New, master[:], salt, "homesync file content v1", KeySize)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// nonce is the chunk counter. Safe as a counter because the key is unique to
// this file, so no two files ever seal different plaintext under the same
// key and nonce.
func nonce(index int64) []byte {
	var n [12]byte
	binary.BigEndian.PutUint64(n[4:], uint64(index))
	return n[:]
}

// additional binds a chunk to its position. Without the index a chunk could be
// moved within the file; without the header it could be moved to another file;
// without the final flag the tail could be cut off and the result would still
// decrypt cleanly.
func additional(header []byte, index int64, final bool) []byte {
	aad := make([]byte, 0, headerSize+9)
	aad = append(aad, header...)
	aad = binary.BigEndian.AppendUint64(aad, uint64(index))
	if final {
		aad = append(aad, 1)
	} else {
		aad = append(aad, 0)
	}
	return aad
}

// Encrypt writes src to dst in the format above and reports how much
// *plaintext* it consumed.
//
// The reader is consumed to the end before the last chunk can be marked as
// such, so the whole stream is read even though only one chunk is held in
// memory at a time.
func Encrypt(dst io.Writer, src io.Reader, key Key) (int64, error) {
	header := make([]byte, headerSize)
	copy(header, magic)
	header[len(magic)] = version
	header[len(magic)+1] = 0
	if _, err := rand.Read(header[len(magic)+2:]); err != nil {
		return 0, err
	}

	aead, err := aeadFor(key, header[len(magic)+2:])
	if err != nil {
		return 0, err
	}
	if _, err := dst.Write(header); err != nil {
		return 0, err
	}

	var (
		written int64
		index   int64
		plain   = make([]byte, ChunkSize)
		sealed  = make([]byte, 0, sealedSize)
		// One chunk is held back at all times, because a chunk cannot be
		// sealed until we know whether anything follows it.
		pending    = make([]byte, ChunkSize)
		pendingLen = -1
	)

	for {
		n, readErr := io.ReadFull(src, plain)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return written, readErr
		}
		atEnd := readErr == io.EOF || readErr == io.ErrUnexpectedEOF

		if pendingLen >= 0 {
			sealed = aead.Seal(sealed[:0], nonce(index),
				pending[:pendingLen], additional(header, index, false))
			if _, err := dst.Write(sealed); err != nil {
				return written, err
			}
			written += int64(pendingLen)
			index++
			pendingLen = -1
		}

		if n > 0 {
			copy(pending, plain[:n])
			pendingLen = n
		}
		if atEnd {
			break
		}
	}

	if pendingLen < 0 {
		pendingLen = 0
	}

	// Always a final chunk, even for an empty file: it is the only thing
	// authenticating the header, and a file truncated to just its header would
	// otherwise read back as valid and empty.
	sealed = aead.Seal(sealed[:0], nonce(index),
		pending[:pendingLen], additional(header, index, true))
	if _, err := dst.Write(sealed); err != nil {
		return written, err
	}
	written += int64(pendingLen)

	return written, nil
}

// IsEncrypted reports whether these leading bytes are one of our files.
func IsEncrypted(head []byte) bool {
	return len(head) >= headerSize &&
		string(head[:len(magic)]) == magic &&
		head[len(magic)] == version
}

// plainSize works the plaintext length back out of the file's size.
//
// Every chunk but the last carries exactly ChunkSize bytes, so the division is
// exact and there is nothing to trust: a file that has been truncated gives an
// answer that its own chunks then fail to authenticate.
func plainSize(fileSize int64) (size int64, chunks int64, err error) {
	body := fileSize - int64(headerSize)
	if body < tagSize {
		return 0, 0, ErrCorrupt
	}

	full := body / sealedSize
	rem := body % sealedSize

	switch {
	case rem == 0:
		return full * ChunkSize, full, nil
	case rem < tagSize:
		return 0, 0, ErrCorrupt
	default:
		return full*ChunkSize + rem - tagSize, full + 1, nil
	}
}

// Reader decrypts a file on the way past, and seeks.
//
// Seeking matters more than it looks: it is what lets the HTTP layer keep
// using http.ServeContent, so range requests and conditional GETs behave
// identically whether or not the volume is encrypted.
type Reader struct {
	file   *os.File
	aead   cipher.AEAD
	header []byte

	size     int64 // plaintext
	chunks   int64
	fileSize int64

	offset  int64 // plaintext position
	loaded  int64 // which chunk buf holds, -1 for none
	buf     []byte
	scratch []byte
}

// Open returns a reader over a file's plaintext, whichever form it is stored
// in. A plaintext file is handed back as-is, so an unencrypted server pays
// nothing for this existing.
func Open(path string, key *Key) (io.ReadSeekCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}

	head := make([]byte, headerSize)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		f.Close()
		return nil, 0, err
	}

	if !IsEncrypted(head[:n]) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return nil, 0, err
		}
		return f, info.Size(), nil
	}

	if key == nil {
		f.Close()
		return nil, 0, ErrKeyMissing
	}

	aead, err := aeadFor(*key, head[len(magic)+2:])
	if err != nil {
		f.Close()
		return nil, 0, err
	}

	size, chunks, err := plainSize(info.Size())
	if err != nil {
		f.Close()
		return nil, 0, err
	}

	r := &Reader{
		file:     f,
		aead:     aead,
		header:   head,
		size:     size,
		chunks:   chunks,
		fileSize: info.Size(),
		loaded:   -1,
		buf:      make([]byte, 0, ChunkSize),
		scratch:  make([]byte, sealedSize),
	}

	// Check the tail before handing the file over.
	//
	// The plaintext length comes from the file's size, so cutting the end off
	// also cuts down what we think the file is — and a reader that stops at
	// the shorter length never reaches the damage. Cut on a chunk boundary and
	// the shortened file reads back clean, which is precisely the failure
	// sealing the last chunk as final is meant to catch. Reading it here is
	// what makes that seal count: whatever is at the end now has to
	// authenticate as the end.
	if err := r.load(chunks - 1); err != nil {
		f.Close()
		return nil, 0, err
	}

	return r, size, nil
}

func (r *Reader) Read(p []byte) (int, error) {
	if r.offset >= r.size {
		return 0, io.EOF
	}

	index := r.offset / ChunkSize
	if index != r.loaded {
		if err := r.load(index); err != nil {
			return 0, err
		}
	}

	within := r.offset - index*ChunkSize
	if within > int64(len(r.buf)) {
		// The size we derived and the chunk we decrypted disagree, which
		// should be impossible. Refusing beats indexing past the buffer.
		return 0, ErrCorrupt
	}

	n := copy(p, r.buf[within:])
	r.offset += int64(n)
	return n, nil
}

func (r *Reader) load(index int64) error {
	if index < 0 || index >= r.chunks {
		return ErrCorrupt
	}

	at := int64(headerSize) + index*sealedSize
	want := sealedSize
	if index == r.chunks-1 {
		want = int(r.fileSize - at)
		if want < tagSize || want > sealedSize {
			return ErrCorrupt
		}
	}

	if _, err := r.file.ReadAt(r.scratch[:want], at); err != nil {
		return err
	}

	plain, err := r.aead.Open(r.buf[:0], nonce(index),
		r.scratch[:want], additional(r.header, index, index == r.chunks-1))
	if err != nil {
		return ErrCorrupt
	}

	r.buf = plain
	r.loaded = index
	return nil
}

func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.offset + offset
	case io.SeekEnd:
		target = r.size + offset
	default:
		return 0, fmt.Errorf("crypt: bad whence %d", whence)
	}
	if target < 0 {
		return 0, fmt.Errorf("crypt: seek to negative position %d", target)
	}
	r.offset = target
	return target, nil
}

func (r *Reader) Close() error { return r.file.Close() }

// SizeOf reports a file's plaintext length without reading its contents.
//
// The scan calls this for every file on every pass, so it costs one stat and
// one small read regardless of how large the file is.
func SizeOf(path string, info os.FileInfo) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	head := make([]byte, headerSize)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, err
	}
	if !IsEncrypted(head[:n]) {
		return info.Size(), nil
	}

	size, _, err := plainSize(info.Size())
	return size, err
}

// HashFile returns the hex SHA-256 of a file's plaintext, streaming so that a
// large file never lands in memory whole.
func HashFile(path string, key *Key) (string, error) {
	r, _, err := Open(path, key)
	if err != nil {
		return "", err
	}
	defer r.Close()

	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
