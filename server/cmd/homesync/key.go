package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/schmidt-gabriel/homesync/server/internal/crypt"
)

func keyCommand(cfg config, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("key: missing subcommand")
	}

	switch args[0] {
	case "generate":
		text, err := crypt.GenerateKey()
		if err != nil {
			return err
		}
		// Printed on its own line so it can be piped straight into a secret
		// store, with the explanation on stderr where it will not be captured.
		fmt.Fprint(os.Stderr, "A new encryption key. Store it somewhere you will still have\n"+
			"it after the server's disk is gone — without it the files on that\n"+
			"volume cannot be read back.\n\n  ENCRYPTION_KEY=")
		fmt.Println(text)
		return nil

	case "encrypt":
		key, err := cfg.key()
		if err != nil {
			return err
		}
		if key == nil {
			return errors.New("key encrypt: set ENCRYPTION_KEY first")
		}
		return convertTree(cfg.dataDir, key, true)

	case "decrypt":
		key, err := cfg.key()
		if err != nil {
			return err
		}
		if key == nil {
			return errors.New("key decrypt: set ENCRYPTION_KEY to the key the files were written with")
		}
		return convertTree(cfg.dataDir, key, false)

	default:
		return fmt.Errorf("key: unknown subcommand %q", args[0])
	}
}

// convertTree rewrites every file under root into the requested form.
//
// Run it with the server stopped. It is safe to interrupt and safe to run
// twice: each file is converted on its own, through a temporary file that is
// renamed into place, and one already in the target form is left alone.
//
// The modification time is carried across deliberately. The index keys its
// cheap change check on size and mtime, and the plaintext size does not change
// here, so a converted tree does not look modified — no revisions are burned
// and no client re-downloads a file whose contents nobody touched.
func convertTree(root string, key *crypt.Key, encrypting bool) error {
	var converted, skipped int

	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		// Our own half-written uploads; the server will clean them up.
		if strings.HasSuffix(d.Name(), ".homesync-tmp") {
			return nil
		}

		changed, err := convertFile(abs, key, encrypting)
		if err != nil {
			return fmt.Errorf("%s: %w", abs, err)
		}
		if changed {
			converted++
		} else {
			skipped++
		}
		return nil
	})
	if err != nil {
		return err
	}

	verb := "encrypted"
	if !encrypting {
		verb = "decrypted"
	}
	fmt.Printf("%s %d file(s); %d already in that form.\n", verb, converted, skipped)
	return nil
}

func convertFile(abs string, key *crypt.Key, encrypting bool) (bool, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return false, err
	}

	head := make([]byte, 32)
	f, err := os.Open(abs)
	if err != nil {
		return false, err
	}
	n, err := io.ReadFull(f, head)
	f.Close()
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}

	if crypt.IsEncrypted(head[:n]) == encrypting {
		return false, nil
	}

	source, _, err := crypt.Open(abs, key)
	if err != nil {
		return false, err
	}
	defer source.Close()

	tmp, err := os.CreateTemp(filepath.Dir(abs), ".convert-*.homesync-tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if encrypting {
		_, err = crypt.Encrypt(tmp, source, *key)
	} else {
		_, err = io.Copy(tmp, source)
	}
	if err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmpName, info.Mode().Perm()); err != nil {
		return false, err
	}
	if err := os.Chtimes(tmpName, info.ModTime(), info.ModTime()); err != nil {
		return false, err
	}

	return true, os.Rename(tmpName, abs)
}
