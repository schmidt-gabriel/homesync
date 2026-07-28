// Package conformance is an executable version of docs/PROTOCOL.md.
//
// It deliberately speaks only HTTP and imports nothing from the Go server, so
// it validates *a* HomeSync server rather than *the* one in this repository.
// A reimplementation in another language proves itself by passing this suite.
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Server is the system under test.
type Server struct {
	BaseURL string
	Token   string
	// DataDir is the server's data directory when the suite started it itself.
	// Empty when testing a server we did not launch, which is why the tests
	// that write straight to the volume skip in that case.
	DataDir string
	// Scope is the subtree the test device syncs. Paths in the protocol are
	// relative to it, so tests never mention it — except the ones that write
	// straight to the volume, which have to land inside it to be visible.
	Scope string

	client *http.Client
	stop   func()
}

// StartServer returns the server to test.
//
// With HOMESYNC_URL and HOMESYNC_TOKEN set it uses that one, which is how CI
// runs the suite against the built container image. Otherwise it builds and
// launches the Go server from ../server, so a plain `go test ./...` works with
// no setup.
func StartServer(t *testing.T) *Server {
	t.Helper()

	client := &http.Client{Timeout: 30 * time.Second}

	if url, token := os.Getenv("HOMESYNC_URL"), os.Getenv("HOMESYNC_TOKEN"); url != "" && token != "" {
		srv := &Server{
			BaseURL: strings.TrimSuffix(url, "/"),
			Token:   token,
			DataDir: os.Getenv("HOMESYNC_DATA_DIR"),
			Scope:   os.Getenv("HOMESYNC_SCOPE"),
			client:  client,
			stop:    func() {},
		}
		srv.waitHealthy(t)
		return srv
	}

	binary := buildServer(t)

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configDir := filepath.Join(root, "config")
	for _, dir := range []string{dataDir, configDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	env := append(os.Environ(),
		"DATA_DIR="+dataDir,
		"CONFIG_DIR="+configDir,
		"LOG_LEVEL=warn",
		// Long enough that the periodic pass never fires mid-test: the tests
		// that care about reconciliation trigger it explicitly by restarting.
		"RESCAN_INTERVAL=1h",
	)

	// Mint the token before starting the server: the CLI and the server both
	// open the same SQLite file, and doing it first avoids any contention.
	const scope = "conformance"
	token := addDevice(t, binary, env, "conformance")

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cmd := exec.Command(binary, "serve")
	cmd.Env = append(env, "LISTEN_ADDR="+addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	srv := &Server{
		BaseURL: "http://" + addr,
		Token:   token,
		DataDir: dataDir,
		Scope:   scope,
		client:  client,
		stop: func() {
			if cmd.Process != nil {
				cmd.Process.Kill()
				cmd.Wait()
			}
		},
	}
	t.Cleanup(srv.stop)

	srv.waitHealthy(t)
	return srv
}

// serverBinary is built once and reused by every test in the package.
var serverBinary struct {
	path string
	err  error
	done bool
}

func buildServer(t *testing.T) string {
	t.Helper()

	if serverBinary.done {
		if serverBinary.err != nil {
			t.Fatalf("build server: %v", serverBinary.err)
		}
		return serverBinary.path
	}
	serverBinary.done = true

	// Not t.TempDir: this outlives the test that happened to build it.
	dir, err := os.MkdirTemp("", "homesync-conformance-*")
	if err != nil {
		serverBinary.err = err
		t.Fatalf("temp dir: %v", err)
	}

	path := filepath.Join(dir, "homesync")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/homesync")
	cmd.Dir = "../server"
	if out, err := cmd.CombinedOutput(); err != nil {
		serverBinary.err = fmt.Errorf("%w: %s", err, out)
		t.Fatalf("build server: %v", serverBinary.err)
	}

	serverBinary.path = path
	return path
}

// tokenPattern matches the base64url token the CLI prints.
var tokenPattern = regexp.MustCompile(`[A-Za-z0-9_-]{43}`)

func addDevice(t *testing.T, binary string, env []string, name string) string {
	t.Helper()

	cmd := exec.Command(binary, "device", "add", name)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("device add: %v: %s", err, out)
	}

	token := tokenPattern.FindString(string(out))
	if token == "" {
		t.Fatalf("no token in device add output: %s", out)
	}
	return token
}

func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// ScopedDir is where this device's files actually live on the server's disk.
// Everything the protocol says is relative to it.
func (s *Server) ScopedDir() string {
	if s.DataDir == "" {
		return ""
	}
	if s.Scope == "" {
		return s.DataDir
	}
	return filepath.Join(s.DataDir, s.Scope)
}

func (s *Server) waitHealthy(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, err := s.client.Get(s.BaseURL + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s never became healthy", s.BaseURL)
}

// ── Request helpers ──────────────────────────────────────────────────────────

// Response is the outcome of one request, kept deliberately simple so the
// tests read as protocol statements rather than as HTTP plumbing.
type Response struct {
	Status  int
	Body    []byte
	Headers http.Header
}

// JSON decodes the body into v, failing the test if it is not valid JSON.
func (r Response) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode response %q: %v", r.Body, err)
	}
}

// Text returns the body as a string.
func (r Response) Text() string { return string(r.Body) }

// Field pulls one top-level JSON field out of the body.
func (r Response) Field(t *testing.T, name string) any {
	t.Helper()
	var body map[string]any
	r.JSON(t, &body)
	return body[name]
}

// Do issues a request. headers alternate name and value.
func (s *Server) Do(t *testing.T, method, path string, body io.Reader, headers ...string) Response {
	t.Helper()

	if len(headers)%2 != 0 {
		t.Fatalf("headers must be name/value pairs, got %d values", len(headers))
	}

	req, err := http.NewRequest(method, s.BaseURL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	res, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return Response{Status: res.StatusCode, Body: payload, Headers: res.Header}
}

// Put uploads content, declaring the revision the caller believes is current.
func (s *Server) Put(t *testing.T, path, content string, baseRev int64) Response {
	t.Helper()
	return s.Do(t, http.MethodPut, "/v1/files/"+path, strings.NewReader(content),
		"X-Base-Rev", strconv.FormatInt(baseRev, 10))
}

// Get downloads content.
func (s *Server) Get(t *testing.T, path string) Response {
	t.Helper()
	return s.Do(t, http.MethodGet, "/v1/files/"+path, nil)
}

// Delete removes a path at a known revision.
func (s *Server) Delete(t *testing.T, path string, baseRev int64) Response {
	t.Helper()
	return s.Do(t, http.MethodDelete, "/v1/files/"+path, nil,
		"X-Base-Rev", strconv.FormatInt(baseRev, 10))
}

// Entry mirrors the Entry shape in docs/PROTOCOL.md §5.
type Entry struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime"`
	SHA256  string `json:"sha256"`
	Rev     int64  `json:"rev"`
	Deleted bool   `json:"deleted"`
	Unsafe  bool   `json:"unsafe"`
}

// Changes is the /v1/changes response.
type Changes struct {
	Changes    []Entry `json:"changes"`
	CurrentRev int64   `json:"current_rev"`
	More       bool    `json:"more"`
}

// Find returns the entry for a path, if present in this page.
func (c Changes) Find(path string) (Entry, bool) {
	for _, e := range c.Changes {
		if e.Path == path {
			return e, true
		}
	}
	return Entry{}, false
}

// ChangesSince fetches one page of changes.
func (s *Server) ChangesSince(t *testing.T, since int64) Changes {
	t.Helper()

	res := s.Do(t, http.MethodGet, "/v1/changes?since="+strconv.FormatInt(since, 10), nil)
	if res.Status != http.StatusOK {
		t.Fatalf("GET /v1/changes: status %d, body %s", res.Status, res.Body)
	}

	var changes Changes
	res.JSON(t, &changes)
	return changes
}

// RevOf returns the current revision of a path, or 0 if it is absent or
// tombstoned — the same value a client must send as X-Base-Rev in that case.
func (s *Server) RevOf(t *testing.T, path string) int64 {
	t.Helper()

	res := s.Get(t, path)
	if res.Status != http.StatusOK {
		return 0
	}

	rev, err := strconv.ParseInt(res.Headers.Get("X-Base-Rev"), 10, 64)
	if err != nil {
		t.Fatalf("GET %s returned 200 without a usable X-Base-Rev: %v", path, err)
	}
	return rev
}

// PutNew uploads a path that must not already exist, and returns its revision.
func (s *Server) PutNew(t *testing.T, path, content string) int64 {
	t.Helper()

	res := s.Put(t, path, content, 0)
	if res.Status != http.StatusCreated {
		t.Fatalf("PUT %s: expected 201, got %d (%s)", path, res.Status, res.Body)
	}

	var created struct {
		Rev int64 `json:"rev"`
	}
	res.JSON(t, &created)
	return created.Rev
}

// ── Assertions ───────────────────────────────────────────────────────────────

func requireStatus(t *testing.T, res Response, want int, what string) {
	t.Helper()
	if res.Status != want {
		t.Fatalf("%s: expected status %d, got %d (body: %s)", what, want, res.Status, res.Body)
	}
}

func requireErrorCode(t *testing.T, res Response, want int, code, what string) {
	t.Helper()
	requireStatus(t, res, want, what)

	var body struct {
		Error string `json:"error"`
	}
	res.JSON(t, &body)
	if body.Error != code {
		t.Errorf("%s: expected error code %q, got %q", what, code, body.Error)
	}
}

func requireBody(t *testing.T, res Response, want, what string) {
	t.Helper()
	requireStatus(t, res, http.StatusOK, what)
	if got := res.Text(); got != want {
		t.Errorf("%s: expected body %q, got %q", what, want, got)
	}
}

// eventually retries until the condition holds or the deadline passes. Used for
// anything the server does asynchronously, such as noticing a change made
// directly on its filesystem.
func eventually(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", within, what)
}

// unique keeps paths from colliding between tests sharing one server.
func unique(t *testing.T, name string) string {
	t.Helper()
	safe := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return safe + "-" + name
}

func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return bytes.NewReader(encoded)
}
