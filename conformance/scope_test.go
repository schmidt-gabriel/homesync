package conformance

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestScopes covers the rule that makes multi-machine sync a choice rather
// than the only option: a device syncs one subtree, and two devices pointed at
// the same subtree share those files.
//
// It needs its own server, since it registers extra devices.
func TestScopes(t *testing.T) {
	if os.Getenv("HOMESYNC_URL") != "" {
		t.Skip("needs a server it can register devices on")
	}

	srv := StartServer(t)
	binary := buildServer(t)
	env := append(os.Environ(),
		"DATA_DIR="+srv.DataDir,
		"CONFIG_DIR="+filepath.Join(filepath.Dir(srv.DataDir), "config"),
		"LOG_LEVEL=warn",
	)

	// Two devices in their own subtrees, and a third sharing the first's.
	alice := deviceWithScope(t, srv, binary, env, "alice", "alice")
	bob := deviceWithScope(t, srv, binary, env, "bob", "bob")
	alicesOtherMac := deviceWithScope(t, srv, binary, env, "alice-laptop", "alice")

	alice.PutNew(t, "private.txt", "alice's file")

	t.Run("a device does not see another's files", func(t *testing.T) {
		requireErrorCode(t, bob.Get(t, "private.txt"),
			http.StatusNotFound, "not_found", "reading across scopes")

		for _, entry := range bob.ChangesSince(t, 0).Changes {
			if strings.Contains(entry.Path, "private") {
				t.Errorf("bob's changes leaked %q", entry.Path)
			}
		}
	})

	t.Run("two devices in the same scope share files", func(t *testing.T) {
		requireBody(t, alicesOtherMac.Get(t, "private.txt"), "alice's file",
			"reading a file put by another device in the same scope")
	})

	t.Run("paths are relative to the scope", func(t *testing.T) {
		// The device asked for "private.txt" and must be told "private.txt",
		// never "alice/private.txt": a client has no idea it is in a subtree.
		entry, found := alice.ChangesSince(t, 0).Find("private.txt")
		if !found {
			t.Fatal("private.txt missing from alice's changes")
		}
		if strings.Contains(entry.Path, "alice/") {
			t.Errorf("the scope leaked into the path: %q", entry.Path)
		}
	})

	t.Run("on disk the scope is a real folder", func(t *testing.T) {
		if _, err := os.Lstat(filepath.Join(srv.DataDir, "alice", "private.txt")); err != nil {
			t.Errorf("expected the file under its scope directory: %v", err)
		}
	})

	t.Run("a device cannot escape its scope", func(t *testing.T) {
		// The dot segments are rejected before any scope arithmetic happens,
		// which is what keeps "../bob/private.txt" from being a way out.
		res := bob.Do(t, http.MethodGet, "/v1/files/../alice/private.txt", nil)
		if res.Status != http.StatusBadRequest && res.Status != http.StatusNotFound {
			t.Errorf("expected the traversal to be refused, got %d: %s", res.Status, res.Body)
		}
	})

	t.Run("the scope directory itself is not an entry", func(t *testing.T) {
		// It is the device's root, and a client has no entry for its own root.
		// It also must not occupy a slot in a page, or a paginated response
		// could come back empty with more=true and stall the client.
		for _, entry := range alice.ChangesSince(t, 0).Changes {
			if entry.Path == "" || entry.Path == "alice" {
				t.Errorf("the scope directory appeared as an entry: %+v", entry)
			}
		}
	})
}

// deviceWithScope registers a device and returns a Server that speaks as it.
func deviceWithScope(t *testing.T, srv *Server, binary string, env []string, name, scope string) *Server {
	t.Helper()

	cmd := exec.Command(binary, "device", "add", name, scope)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("device add %s: %v: %s", name, err, out)
	}

	token := tokenPattern.FindString(string(out))
	if token == "" {
		t.Fatalf("no token in output: %s", out)
	}

	return &Server{
		BaseURL: srv.BaseURL,
		Token:   token,
		DataDir: srv.DataDir,
		Scope:   scope,
		client:  srv.client,
		stop:    func() {},
	}
}
