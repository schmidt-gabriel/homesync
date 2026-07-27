package conformance

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClientConformance drives a client implementation and checks that changes
// propagate correctly in both directions.
//
// It is skipped unless HOMESYNC_CLIENT_CMD names a command to run. That command
// is launched once, with these variables in its environment, and is expected to
// keep syncing until it is killed:
//
//	HOMESYNC_URL     server to talk to
//	HOMESYNC_TOKEN   device token
//	HOMESYNC_ROOT    local directory to keep in sync
//
// Any client in any language can satisfy that contract, which is the point:
// a second implementation proves itself here rather than by being compared
// against the first one.
//
// Example:
//
//	HOMESYNC_CLIENT_CMD="swift run homesync-cli" go test -run TestClientConformance ./...
func TestClientConformance(t *testing.T) {
	command := os.Getenv("HOMESYNC_CLIENT_CMD")
	if command == "" {
		t.Skip("set HOMESYNC_CLIENT_CMD to the client under test to run this suite")
	}

	srv := StartServer(t)
	root := t.TempDir()
	startClient(t, command, srv, root)

	// How long a client may take to notice and propagate a change. Generous:
	// a client that batches or debounces is behaving correctly, and a flaky
	// conformance suite is worse than a slow one.
	const settle = 30 * time.Second

	t.Run("local file reaches the server", func(t *testing.T) {
		writeLocal(t, root, "from-client.txt", "written locally")

		eventually(t, settle, "the local file to reach the server", func() bool {
			return srv.Get(t, "from-client.txt").Status == http.StatusOK
		})
		requireBody(t, srv.Get(t, "from-client.txt"), "written locally", "the uploaded file")
	})

	t.Run("server file reaches the client", func(t *testing.T) {
		srv.PutNew(t, "from-server.txt", "written remotely")

		eventually(t, settle, "the remote file to reach the client", func() bool {
			return readLocal(root, "from-server.txt") == "written remotely"
		})
	})

	t.Run("local edit reaches the server", func(t *testing.T) {
		writeLocal(t, root, "from-server.txt", "edited locally")

		eventually(t, settle, "the local edit to reach the server", func() bool {
			return srv.Get(t, "from-server.txt").Text() == "edited locally"
		})
	})

	t.Run("local deletion reaches the server", func(t *testing.T) {
		removeLocal(t, root, "from-client.txt")

		eventually(t, settle, "the deletion to reach the server", func() bool {
			return srv.Get(t, "from-client.txt").Status == http.StatusNotFound
		})
	})

	t.Run("server deletion reaches the client", func(t *testing.T) {
		path := "doomed-remotely.txt"
		rev := srv.PutNew(t, path, "temporary")

		eventually(t, settle, "the file to arrive locally", func() bool {
			return existsLocal(root, path)
		})

		requireStatus(t, srv.Delete(t, path, rev), http.StatusOK, "DELETE")

		eventually(t, settle, "the deletion to reach the client", func() bool {
			return !existsLocal(root, path)
		})
	})

	t.Run("nested directories propagate", func(t *testing.T) {
		writeLocal(t, root, "deep/nested/path/file.txt", "deep content")

		eventually(t, settle, "the nested file to reach the server", func() bool {
			return srv.Get(t, "deep/nested/path/file.txt").Status == http.StatusOK
		})
	})

	t.Run("ignored files do not travel", func(t *testing.T) {
		// .DS_Store is in the server's default ignore list, so a conforming
		// client must never upload it.
		writeLocal(t, root, ".DS_Store", "junk")
		writeLocal(t, root, "canary.txt", "sentinel")

		// Wait for the canary rather than sleeping: once a file written at the
		// same moment has arrived, the client has had its chance.
		eventually(t, settle, "the canary to reach the server", func() bool {
			return srv.Get(t, "canary.txt").Status == http.StatusOK
		})

		if res := srv.Get(t, ".DS_Store"); res.Status != http.StatusNotFound {
			t.Errorf("client uploaded an ignored file: status %d", res.Status)
		}
	})

	t.Run("a conflict leaves both versions locally", func(t *testing.T) {
		path := "contested.md"
		srv.PutNew(t, path, "server version")

		eventually(t, settle, "the file to arrive locally", func() bool {
			return existsLocal(root, path)
		})

		// Change both sides. Whichever ordering the client happens to see, the
		// rule is the same: nothing may be silently discarded.
		writeLocal(t, root, path, "client version")
		srv.Put(t, path, "another server version", srv.RevOf(t, path))

		eventually(t, settle, "a conflict copy to appear locally", func() bool {
			return len(localConflicts(t, root, "contested")) > 0
		})

		// And both bodies must still be readable somewhere.
		bodies := map[string]bool{readLocal(root, path): true}
		for _, name := range localConflicts(t, root, "contested") {
			bodies[readLocal(root, name)] = true
		}
		if !bodies["client version"] {
			t.Error("the local edit was lost rather than preserved as a conflict copy")
		}
	})
}

// startClient launches the client under test and stops it when the test ends.
func startClient(t *testing.T, command string, srv *Server, root string) {
	t.Helper()

	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(),
		"HOMESYNC_URL="+srv.BaseURL,
		"HOMESYNC_TOKEN="+srv.Token,
		"HOMESYNC_ROOT="+root,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start client %q: %v", command, err)
	}

	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
}

func writeLocal(t *testing.T, root, path, content string) {
	t.Helper()

	abs := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("create parent of %s: %v", path, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readLocal(root, path string) string {
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return ""
	}
	return string(content)
}

func existsLocal(root, path string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
	return err == nil
}

func removeLocal(t *testing.T, root, path string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// localConflicts lists conflict copies of a given stem in the root.
func localConflicts(t *testing.T, root, stem string) []string {
	t.Helper()

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}

	var found []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, stem+".conflict-") {
			found = append(found, name)
		}
	}
	return found
}
