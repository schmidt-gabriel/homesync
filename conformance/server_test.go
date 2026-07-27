package conformance

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestServerConformance is docs/PROTOCOL.md as executable statements. The
// subtests share one server, so they must not depend on each other's state:
// every one works on paths derived from its own name.
func TestServerConformance(t *testing.T) {
	srv := StartServer(t)

	t.Run("Authentication", func(t *testing.T) { testAuthentication(t, srv) })
	t.Run("PathRules", func(t *testing.T) { testPathRules(t, srv) })
	t.Run("FilesRoundTrip", func(t *testing.T) { testFilesRoundTrip(t, srv) })
	t.Run("Changes", func(t *testing.T) { testChanges(t, srv) })
	t.Run("Directories", func(t *testing.T) { testDirectories(t, srv) })
	t.Run("Conflicts", func(t *testing.T) { testConflicts(t, srv) })
	t.Run("Deletion", func(t *testing.T) { testDeletion(t, srv) })
	t.Run("Trash", func(t *testing.T) { testTrash(t, srv) })
	t.Run("IgnoreRules", func(t *testing.T) { testIgnoreRules(t, srv) })
	t.Run("Events", func(t *testing.T) { testEvents(t, srv) })
	t.Run("OutOfBandChanges", func(t *testing.T) { testOutOfBandChanges(t, srv) })
}

// ── §2 Authentication ────────────────────────────────────────────────────────

func testAuthentication(t *testing.T, srv *Server) {
	client := &http.Client{Timeout: 10 * time.Second}

	t.Run("healthz needs no token", func(t *testing.T) {
		res, err := client.Get(srv.BaseURL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", res.StatusCode)
		}
	})

	unauthenticated := func(t *testing.T, header string) {
		t.Helper()

		req, err := http.NewRequest(http.MethodGet, srv.BaseURL+"/v1/changes", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if header != "" {
			req.Header.Set("Authorization", header)
		}

		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", res.StatusCode)
		}
		if got := res.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Errorf("expected a Bearer challenge, got %q", got)
		}
	}

	t.Run("no token", func(t *testing.T) { unauthenticated(t, "") })
	t.Run("unknown token", func(t *testing.T) { unauthenticated(t, "Bearer not-a-real-token") })
	t.Run("wrong scheme", func(t *testing.T) { unauthenticated(t, "Basic "+srv.Token) })
}

// ── §3 Path rules ────────────────────────────────────────────────────────────

func testPathRules(t *testing.T, srv *Server) {
	t.Run("dot segments rejected", func(t *testing.T) {
		// Raw and percent-encoded must both fail, and must fail outright: a
		// redirect to a cleaned path is not an acceptable answer to a PUT.
		for name, path := range map[string]string{
			"raw":     "../../etc/passwd",
			"encoded": "%2e%2e%2f%2e%2e%2fetc%2fpasswd",
			"single":  "./secrets",
		} {
			t.Run(name, func(t *testing.T) {
				res := srv.Do(t, http.MethodPut, "/v1/files/"+path, strings.NewReader("x"))
				requireErrorCode(t, res, http.StatusBadRequest, "invalid_path", "traversal attempt")
			})
		}
	})

	t.Run("unicode normalised to NFC", func(t *testing.T) {
		// macOS hands out NFD ("c" + combining cedilla); Linux and Windows use
		// NFC. Both spellings must reach the same single entry.
		// Spelled with escapes on purpose. Writing the two forms as literal
		// text would leave the distinction at the mercy of any editor or tool
		// that normalises the source file, and the test would silently start
		// comparing a string to itself.
		nfd := unique(t, "a\u0063\u0327a\u0303o.txt") // c + combining cedilla
		nfc := unique(t, "a\u00e7\u00e3o.txt")        // precomposed ç, ã

		if nfd == nfc {
			t.Fatal("the two spellings are identical; this test would prove nothing")
		}

		srv.PutNew(t, nfd, "unicode")

		requireBody(t, srv.Get(t, nfc), "unicode", "GET via the NFC spelling")
		requireBody(t, srv.Get(t, nfd), "unicode", "GET via the NFD spelling")

		// Both spellings must have produced exactly one index entry, and it
		// must be stored in the composed form.
		prefix := unique(t, "")
		var matched []string
		for _, e := range srv.ChangesSince(t, 0).Changes {
			if strings.HasPrefix(e.Path, prefix) {
				matched = append(matched, e.Path)
			}
		}

		if len(matched) != 1 {
			t.Fatalf("expected the two spellings to share one entry, found %d: %q", len(matched), matched)
		}
		if matched[0] != nfc {
			t.Errorf("expected the entry to be stored as NFC %q, got %q", nfc, matched[0])
		}
	})

	t.Run("case collision refused without writing", func(t *testing.T) {
		lower := unique(t, "notes.md")
		upper := unique(t, "NOTES.md")

		srv.PutNew(t, lower, "original")

		res := srv.Put(t, upper, "clobber", 0)
		requireErrorCode(t, res, http.StatusConflict, "case_collision", "PUT differing only by case")

		// On a case-insensitive volume the write itself would have destroyed
		// the original, so this catches a server that checks too late.
		requireBody(t, srv.Get(t, lower), "original", "original after a refused collision")

		// On a case-sensitive volume the damage looks different: the original
		// survives but the rejected write is left behind as a file the index
		// does not know about, which the next rescan would then resurrect.
		// Without this assertion the whole subtest silently proves nothing on
		// Linux, which is exactly where CI runs it.
		//
		// The name has to be compared byte for byte against the directory
		// listing: a plain Lstat of the upper-case name succeeds on a
		// case-insensitive volume by resolving to the legitimate lower-case
		// file, which would report a failure that is not there.
		if srv.DataDir != "" {
			entries, err := os.ReadDir(srv.DataDir)
			if err != nil {
				t.Fatalf("read the data directory: %v", err)
			}
			for _, e := range entries {
				if e.Name() == upper {
					t.Errorf("a refused collision left %q on disk, unindexed", upper)
				}
			}
		}
	})

	t.Run("windows-hostile names stored and flagged", func(t *testing.T) {
		path := unique(t, "report:final.txt")
		srv.PutNew(t, path, "x")

		entry, found := srv.ChangesSince(t, 0).Find(path)
		if !found {
			t.Fatalf("%q missing from changes", path)
		}
		if !entry.Unsafe {
			t.Errorf("expected unsafe=true for a name Windows cannot represent")
		}
	})
}

// ── §5 Files ─────────────────────────────────────────────────────────────────

func testFilesRoundTrip(t *testing.T, srv *Server) {
	path := unique(t, "doc.txt")
	const content = "abcdefghij"

	rev := srv.PutNew(t, path, content)

	t.Run("content round-trips", func(t *testing.T) {
		requireBody(t, srv.Get(t, path), content, "GET after PUT")
	})

	t.Run("ETag is the sha256", func(t *testing.T) {
		sum := sha256.Sum256([]byte(content))
		want := `"` + hex.EncodeToString(sum[:]) + `"`

		if got := srv.Get(t, path).Headers.Get("ETag"); got != want {
			t.Errorf("expected ETag %s, got %s", want, got)
		}
	})

	t.Run("X-Base-Rev reports the current revision", func(t *testing.T) {
		if got := srv.RevOf(t, path); got != rev {
			t.Errorf("expected rev %d, got %d", rev, got)
		}
	})

	t.Run("HEAD works", func(t *testing.T) {
		requireStatus(t, srv.Do(t, http.MethodHead, "/v1/files/"+path, nil), http.StatusOK, "HEAD")
	})

	t.Run("Range requests are honoured", func(t *testing.T) {
		res := srv.Do(t, http.MethodGet, "/v1/files/"+path, nil, "Range", "bytes=2-5")
		requireStatus(t, res, http.StatusPartialContent, "ranged GET")
		if got := res.Text(); got != "cdef" {
			t.Errorf("expected %q, got %q", "cdef", got)
		}
	})

	t.Run("conditional GET returns 304", func(t *testing.T) {
		etag := srv.Get(t, path).Headers.Get("ETag")
		res := srv.Do(t, http.MethodGet, "/v1/files/"+path, nil, "If-None-Match", etag)
		requireStatus(t, res, http.StatusNotModified, "GET with If-None-Match")
	})

	t.Run("update with the current revision succeeds", func(t *testing.T) {
		res := srv.Put(t, path, "updated", srv.RevOf(t, path))
		requireStatus(t, res, http.StatusOK, "PUT over an existing path")
		requireBody(t, srv.Get(t, path), "updated", "GET after update")
	})

	t.Run("missing path is 404", func(t *testing.T) {
		res := srv.Get(t, unique(t, "nothing-here.txt"))
		requireErrorCode(t, res, http.StatusNotFound, "not_found", "GET of an absent path")
	})

	t.Run("invalid base rev is rejected", func(t *testing.T) {
		res := srv.Do(t, http.MethodPut, "/v1/files/"+unique(t, "bad.txt"),
			strings.NewReader("x"), "X-Base-Rev", "not-a-number")
		requireErrorCode(t, res, http.StatusBadRequest, "invalid_base_rev", "PUT with a bad X-Base-Rev")
	})
}

// ── §5 Changes ───────────────────────────────────────────────────────────────

func testChanges(t *testing.T, srv *Server) {
	t.Run("revisions increase and are reported", func(t *testing.T) {
		before := srv.ChangesSince(t, 0).CurrentRev

		path := unique(t, "tracked.txt")
		rev := srv.PutNew(t, path, "content")

		if rev <= before {
			t.Errorf("expected the new revision (%d) to exceed the previous current_rev (%d)", rev, before)
		}

		changes := srv.ChangesSince(t, before)
		entry, found := changes.Find(path)
		if !found {
			t.Fatalf("%q missing from changes since %d", path, before)
		}
		if entry.Rev != rev {
			t.Errorf("expected entry rev %d, got %d", rev, entry.Rev)
		}
		if entry.Type != "file" {
			t.Errorf("expected type %q, got %q", "file", entry.Type)
		}
		if entry.Size != int64(len("content")) {
			t.Errorf("expected size %d, got %d", len("content"), entry.Size)
		}
	})

	t.Run("entries arrive in revision order", func(t *testing.T) {
		changes := srv.ChangesSince(t, 0)

		var previous int64
		for _, e := range changes.Changes {
			if e.Rev <= previous {
				t.Fatalf("changes are not ascending: %d followed %d", e.Rev, previous)
			}
			previous = e.Rev
		}
	})

	t.Run("pagination sets more", func(t *testing.T) {
		srv.PutNew(t, unique(t, "page-a.txt"), "a")
		srv.PutNew(t, unique(t, "page-b.txt"), "b")

		res := srv.Do(t, http.MethodGet, "/v1/changes?since=0&limit=1", nil)
		requireStatus(t, res, http.StatusOK, "GET /v1/changes with a limit")

		var page Changes
		res.JSON(t, &page)
		if len(page.Changes) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(page.Changes))
		}
		if !page.More {
			t.Error("expected more=true on a truncated page")
		}

		// Following the documented paging rule must make progress.
		next := srv.Do(t, http.MethodGet,
			"/v1/changes?since="+itoa(page.Changes[0].Rev)+"&limit=1", nil)
		var second Changes
		next.JSON(t, &second)
		if len(second.Changes) == 0 || second.Changes[0].Rev <= page.Changes[0].Rev {
			t.Error("paging with since=<last rev> did not advance")
		}
	})

	t.Run("bad parameters are rejected", func(t *testing.T) {
		requireErrorCode(t, srv.Do(t, http.MethodGet, "/v1/changes?since=-1", nil),
			http.StatusBadRequest, "invalid_since", "negative since")
		requireErrorCode(t, srv.Do(t, http.MethodGet, "/v1/changes?limit=0", nil),
			http.StatusBadRequest, "invalid_limit", "zero limit")
	})
}

// ── §5 Directories ───────────────────────────────────────────────────────────

func testDirectories(t *testing.T, srv *Server) {
	dir := unique(t, "folder")

	t.Run("create", func(t *testing.T) {
		requireStatus(t, srv.Do(t, http.MethodPut, "/v1/dirs/"+dir, nil),
			http.StatusCreated, "PUT a new directory")
	})

	t.Run("creating twice is not an error", func(t *testing.T) {
		requireStatus(t, srv.Do(t, http.MethodPut, "/v1/dirs/"+dir, nil),
			http.StatusOK, "PUT an existing directory")
	})

	t.Run("non-empty directory cannot be deleted", func(t *testing.T) {
		srv.PutNew(t, dir+"/child.txt", "x")

		entry, found := srv.ChangesSince(t, 0).Find(dir)
		if !found {
			t.Fatalf("directory %q missing from the index", dir)
		}

		res := srv.Do(t, http.MethodDelete, "/v1/dirs/"+dir, nil,
			"X-Base-Rev", itoa(entry.Rev))
		requireErrorCode(t, res, http.StatusConflict, "not_empty", "DELETE of a populated directory")
	})

	t.Run("empty directory is deleted through either endpoint", func(t *testing.T) {
		empty := unique(t, "empty")
		requireStatus(t, srv.Do(t, http.MethodPut, "/v1/dirs/"+empty, nil),
			http.StatusCreated, "PUT a new directory")

		entry, found := srv.ChangesSince(t, 0).Find(empty)
		if !found {
			t.Fatalf("directory %q missing from the index", empty)
		}

		res := srv.Do(t, http.MethodDelete, "/v1/dirs/"+empty, nil, "X-Base-Rev", itoa(entry.Rev))
		requireStatus(t, res, http.StatusOK, "DELETE of an empty directory")
	})

	t.Run("GET of a directory is not a file read", func(t *testing.T) {
		requireErrorCode(t, srv.Get(t, dir), http.StatusBadRequest, "is_directory",
			"GET of a directory path")
	})
}

// ── §6 Conflicts ─────────────────────────────────────────────────────────────

// conflictName is the documented shape: <stem>.conflict-<device>-<stamp><ext>
var conflictName = regexp.MustCompile(`\.conflict-[A-Za-z0-9-]+-\d{8}-\d{6}\.md$`)

func testConflicts(t *testing.T, srv *Server) {
	path := unique(t, "shared.md")
	srv.PutNew(t, path, "the winning content")

	// A second machine that still believes the path does not exist.
	res := srv.Put(t, path, "the losing content", 0)
	requireStatus(t, res, http.StatusConflict, "PUT from a stale base revision")

	var body struct {
		Error    string `json:"error"`
		Conflict string `json:"conflict"`
		Path     string `json:"path"`
		Rev      int64  `json:"rev"`
	}
	res.JSON(t, &body)

	t.Run("reports the conflict code and the generated name", func(t *testing.T) {
		if body.Error != "conflict" {
			t.Errorf("expected error %q, got %q", "conflict", body.Error)
		}
		if body.Conflict == "" {
			t.Fatal("409 did not name the conflict copy")
		}
		if body.Conflict != body.Path {
			t.Errorf("conflict %q and path %q disagree", body.Conflict, body.Path)
		}
		if !conflictName.MatchString(body.Conflict) {
			t.Errorf("conflict name %q does not match the documented shape", body.Conflict)
		}
	})

	t.Run("keeps the original untouched", func(t *testing.T) {
		requireBody(t, srv.Get(t, path), "the winning content", "the canonical path after a conflict")
	})

	t.Run("stores the losing content rather than discarding it", func(t *testing.T) {
		requireBody(t, srv.Get(t, body.Conflict), "the losing content", "the conflict copy")
	})

	t.Run("the copy is a normal file that syncs", func(t *testing.T) {
		entry, found := srv.ChangesSince(t, 0).Find(body.Conflict)
		if !found {
			t.Fatal("the conflict copy is missing from changes")
		}
		if entry.Deleted || entry.Type != "file" {
			t.Errorf("expected a live file, got %+v", entry)
		}
	})
}

// ── §5 Deletion ──────────────────────────────────────────────────────────────

func testDeletion(t *testing.T, srv *Server) {
	path := unique(t, "doomed.txt")
	rev := srv.PutNew(t, path, "content")

	t.Run("deleting from a stale revision is refused", func(t *testing.T) {
		// Deleting a version you have not seen would discard someone's edit.
		res := srv.Delete(t, path, rev-1)
		requireErrorCode(t, res, http.StatusConflict, "stale", "DELETE from a stale revision")
		requireBody(t, srv.Get(t, path), "content", "the file after a refused delete")
	})

	t.Run("deleting at the current revision succeeds", func(t *testing.T) {
		requireStatus(t, srv.Delete(t, path, rev), http.StatusOK, "DELETE at the current revision")
		requireErrorCode(t, srv.Get(t, path), http.StatusNotFound, "not_found", "GET after delete")
	})

	t.Run("leaves a tombstone so offline clients find out", func(t *testing.T) {
		entry, found := srv.ChangesSince(t, 0).Find(path)
		if !found {
			t.Fatal("deleted path vanished from changes instead of becoming a tombstone")
		}
		if !entry.Deleted {
			t.Error("expected deleted=true")
		}
		if entry.Rev <= rev {
			t.Errorf("expected the tombstone to carry a newer revision than %d, got %d", rev, entry.Rev)
		}
	})

	t.Run("deleting an absent path is 404", func(t *testing.T) {
		requireErrorCode(t, srv.Delete(t, unique(t, "never-existed.txt"), 0),
			http.StatusNotFound, "not_found", "DELETE of an absent path")
	})

	t.Run("recreating after deletion uses base rev 0", func(t *testing.T) {
		res := srv.Put(t, path, "back again", 0)
		requireStatus(t, res, http.StatusCreated, "PUT over a tombstone")
		requireBody(t, srv.Get(t, path), "back again", "the recreated file")
	})
}

// ── §7 Trash ─────────────────────────────────────────────────────────────────

type trashListing struct {
	Items []struct {
		ID        string `json:"id"`
		Path      string `json:"path"`
		DeletedAt string `json:"deleted_at"`
		Size      int64  `json:"size"`
	} `json:"items"`
}

func (l trashListing) findByPath(path string) (string, bool) {
	for _, item := range l.Items {
		if item.Path == path {
			return item.ID, true
		}
	}
	return "", false
}

func testTrash(t *testing.T, srv *Server) {
	path := unique(t, "recoverable.txt")
	rev := srv.PutNew(t, path, "precious")
	requireStatus(t, srv.Delete(t, path, rev), http.StatusOK, "DELETE")

	res := srv.Do(t, http.MethodGet, "/v1/trash", nil)
	requireStatus(t, res, http.StatusOK, "GET /v1/trash")

	var listing trashListing
	res.JSON(t, &listing)

	id, found := listing.findByPath(path)
	if !found {
		t.Fatalf("deleted content for %q is not in the trash", path)
	}

	t.Run("timestamps are RFC 3339", func(t *testing.T) {
		for _, item := range listing.Items {
			if _, err := time.Parse(time.RFC3339, item.DeletedAt); err != nil {
				t.Errorf("deleted_at %q is not RFC 3339: %v", item.DeletedAt, err)
			}
		}
	})

	t.Run("restore brings the content back", func(t *testing.T) {
		res := srv.Do(t, http.MethodPost, "/v1/trash/restore",
			jsonBody(t, map[string]string{"id": id}), "Content-Type", "application/json")
		requireStatus(t, res, http.StatusOK, "POST /v1/trash/restore")
		requireBody(t, srv.Get(t, path), "precious", "the restored file")
	})

	t.Run("restore refuses to clobber", func(t *testing.T) {
		// Something now occupies the original path, so a second restore of the
		// same content must not overwrite it: restoring should never be the
		// operation that loses data.
		second := srv.PutNew(t, unique(t, "occupier.txt"), "first")
		occupied := unique(t, "occupier.txt")
		requireStatus(t, srv.Delete(t, occupied, second), http.StatusOK, "DELETE")

		res := srv.Do(t, http.MethodGet, "/v1/trash", nil)
		var current trashListing
		res.JSON(t, &current)

		occupiedID, ok := current.findByPath(occupied)
		if !ok {
			t.Fatalf("%q missing from the trash", occupied)
		}

		srv.PutNew(t, occupied, "something else now lives here")

		restore := srv.Do(t, http.MethodPost, "/v1/trash/restore",
			jsonBody(t, map[string]string{"id": occupiedID}), "Content-Type", "application/json")
		requireErrorCode(t, restore, http.StatusConflict, "occupied", "restore onto an occupied path")
		requireBody(t, srv.Get(t, occupied), "something else now lives here", "the occupying file")
	})

	t.Run("unknown id is 404", func(t *testing.T) {
		res := srv.Do(t, http.MethodPost, "/v1/trash/restore",
			jsonBody(t, map[string]string{"id": "20200101T000000.000_nope.txt"}),
			"Content-Type", "application/json")
		requireErrorCode(t, res, http.StatusNotFound, "not_found", "restore of an unknown id")
	})
}

// ── §5 Ignore rules ──────────────────────────────────────────────────────────

func testIgnoreRules(t *testing.T, srv *Server) {
	t.Run("round-trips and versions", func(t *testing.T) {
		rules := "# " + t.Name() + "\n*.tmp\nbuild/\n"

		res := srv.Do(t, http.MethodPut, "/v1/ignore",
			jsonBody(t, map[string]string{"rules": rules}), "Content-Type", "application/json")
		requireStatus(t, res, http.StatusOK, "PUT /v1/ignore")

		read := srv.Do(t, http.MethodGet, "/v1/ignore", nil)
		requireStatus(t, read, http.StatusOK, "GET /v1/ignore")

		var body struct {
			Rules   string `json:"rules"`
			Version int64  `json:"version"`
		}
		read.JSON(t, &body)

		if body.Rules != rules {
			t.Errorf("rules did not round-trip:\nwant %q\ngot  %q", rules, body.Rules)
		}
		if body.Version == 0 {
			t.Error("expected a non-zero version after a save")
		}
		if got := read.Headers.Get("ETag"); got != `"`+itoa(body.Version)+`"` {
			t.Errorf("expected the ETag to carry the version, got %q", got)
		}
	})
}

// ── §5 Events ────────────────────────────────────────────────────────────────

func testEvents(t *testing.T, srv *Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.BaseURL+"/v1/events", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)

	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("open the event stream: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", got)
	}

	events := make(chan string, 16)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "data: ") {
				events <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	receive := func(what string) string {
		t.Helper()
		select {
		case event, open := <-events:
			if !open {
				t.Fatalf("the stream closed while waiting for %s", what)
			}
			return event
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return ""
		}
	}

	t.Run("announces the current revision on connect", func(t *testing.T) {
		if event := receive("the initial revision"); !strings.Contains(event, `"rev"`) {
			t.Errorf("expected a rev payload, got %q", event)
		}
	})

	t.Run("announces a change made through the API", func(t *testing.T) {
		rev := srv.PutNew(t, unique(t, "watched.txt"), "content")

		// The stream is a hint, not a transcript: it may coalesce, so accept
		// any announcement that has caught up to our revision.
		deadline := time.After(10 * time.Second)
		for {
			select {
			case event, open := <-events:
				if !open {
					t.Fatal("the stream closed before announcing the change")
				}
				if strings.Contains(event, itoa(rev)) {
					return
				}
			case <-deadline:
				t.Fatalf("no event announced revision %d", rev)
			}
		}
	})
}

// ── Changes made directly on the server's filesystem ─────────────────────────

func testOutOfBandChanges(t *testing.T, srv *Server) {
	if srv.DataDir == "" {
		t.Skip("no access to the server's data directory (testing a remote server)")
	}

	name := unique(t, "written-by-hand.txt")
	abs := filepath.Join(srv.DataDir, name)

	t.Run("a file written straight to the volume is indexed", func(t *testing.T) {
		if err := os.WriteFile(abs, []byte("not via the API"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		eventually(t, 15*time.Second, "the file to appear in the index", func() bool {
			entry, found := srv.ChangesSince(t, 0).Find(name)
			return found && !entry.Deleted
		})

		requireBody(t, srv.Get(t, name), "not via the API", "GET of an out-of-band file")
	})

	t.Run("a file removed straight from the volume is tombstoned", func(t *testing.T) {
		if err := os.Remove(abs); err != nil {
			t.Fatalf("remove file: %v", err)
		}

		eventually(t, 15*time.Second, "the tombstone to appear", func() bool {
			entry, found := srv.ChangesSince(t, 0).Find(name)
			return found && entry.Deleted
		})
	})
}

// itoa keeps the assertions readable at the call sites.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
