package sync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Entry is one path at one revision, as returned by /v1/changes.
type Entry struct {
	Path    string `json:"path"`
	Type    string `json:"type"` // "file" or "dir"
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime"` // unix milliseconds
	SHA256  string `json:"sha256"`
	Rev     int64  `json:"rev"`
	Deleted bool   `json:"deleted"`
	Unsafe  bool   `json:"unsafe"`
}

func (e Entry) IsDir() bool { return e.Type == "dir" }

type changesPage struct {
	Changes    []Entry `json:"changes"`
	CurrentRev int64   `json:"current_rev"`
	More       bool    `json:"more"`
}

// FileResponse is the body of a successful PUT or DELETE.
type FileResponse struct {
	Path   string `json:"path"`
	Rev    int64  `json:"rev"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MTime  int64  `json:"mtime"`
	Type   string `json:"type"`
}

// IgnoreDocument is the shared rules and when they last changed.
type IgnoreDocument struct {
	Rules   string `json:"rules"`
	Version int64  `json:"version"`
}

// ServerError is the one error shape the whole API uses, so a client parses
// failures once.
type ServerError struct {
	Status   int
	Code     string `json:"error"`
	Message  string `json:"message"`
	Conflict string `json:"conflict"`
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// Is lets callers branch on meaning rather than on status codes.
func IsCode(err error, code string) bool {
	var se *ServerError
	return errors.As(err, &se) && se.Code == code
}

func IsNotFound(err error) bool { return IsCode(err, "not_found") }

// Client talks to a HomeSync server.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		// No global timeout: the event stream is meant to stay open for hours,
		// and a client-wide deadline would sever it on a schedule. Per-request
		// deadlines come from the context instead.
		HTTP: &http.Client{},
	}
}

// Insecure reports that the token travels in the clear.
func (c *Client) Insecure() bool {
	return !strings.HasPrefix(strings.ToLower(c.BaseURL), "https://")
}

// pathURL builds a URL for a sync path.
//
// Two things here must not be skipped. The path is normalised to NFC, because
// the server's index is composed. And each segment is escaped separately, so a
// filename containing `?`, `#` or a space cannot change the shape of the
// request while `/` still separates segments.
func (c *Client) pathURL(endpoint, rel string) string {
	segments := strings.Split(norm.NFC.String(rel), "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/v1/%s/%s", c.BaseURL, endpoint, strings.Join(segments, "/"))
}

func (c *Client) request(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	return req, nil
}

// check turns a non-2xx response into the most specific error available.
func check(res *http.Response) error {
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	serverErr := &ServerError{Status: res.StatusCode, Code: "unknown", Message: string(body)}
	_ = json.Unmarshal(body, serverErr)
	if serverErr.Code == "" {
		serverErr.Code = "unknown"
	}
	return serverErr
}

// Changes fetches one page of entries after rev.
func (c *Client) Changes(ctx context.Context, since int64, limit int) (changesPage, error) {
	url := fmt.Sprintf("%s/v1/changes?since=%d&limit=%d", c.BaseURL, since, limit)

	req, err := c.request(ctx, http.MethodGet, url, nil)
	if err != nil {
		return changesPage{}, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return changesPage{}, err
	}
	defer res.Body.Close()

	if err := check(res); err != nil {
		return changesPage{}, err
	}

	var page changesPage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		return changesPage{}, fmt.Errorf("decode changes: %w", err)
	}
	return page, nil
}

// AllChanges follows pagination to the end.
func (c *Client) AllChanges(ctx context.Context, since int64) ([]Entry, int64, error) {
	var collected []Entry
	cursor := since

	for {
		page, err := c.Changes(ctx, cursor, 1000)
		if err != nil {
			return nil, 0, err
		}
		collected = append(collected, page.Changes...)

		if !page.More || len(page.Changes) == 0 {
			return collected, page.CurrentRev, nil
		}

		// Guard against a server that sets `more` without advancing, which
		// would otherwise be an infinite loop holding the sync open.
		last := page.Changes[len(page.Changes)-1].Rev
		if last <= cursor {
			return collected, page.CurrentRev, nil
		}
		cursor = last
	}
}

// Download writes a path's content to a temporary file and reports where it
// landed. The caller owns the file and must move or remove it.
//
// Downloading to a file rather than to memory keeps a large file from being
// held whole, and is what makes the eventual install atomic.
func (c *Client) Download(ctx context.Context, rel, tmpDir string) (file string, rev int64, sha string, err error) {
	req, err := c.request(ctx, http.MethodGet, c.pathURL("files", rel), nil)
	if err != nil {
		return "", 0, "", err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", 0, "", err
	}
	defer res.Body.Close()

	if err := check(res); err != nil {
		return "", 0, "", err
	}

	tmp, err := os.CreateTemp(tmpDir, ".download-*.homesync-tmp")
	if err != nil {
		return "", 0, "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, res.Body); err != nil {
		os.Remove(tmp.Name())
		return "", 0, "", err
	}
	if err := tmp.Sync(); err != nil {
		os.Remove(tmp.Name())
		return "", 0, "", err
	}

	rev, _ = strconv.ParseInt(res.Header.Get("X-Base-Rev"), 10, 64)
	sha = strings.Trim(res.Header.Get("ETag"), `"`)
	sha = strings.TrimPrefix(sha, "W/")
	sha = strings.Trim(sha, `"`)

	return tmp.Name(), rev, sha, nil
}

// Upload sends a file's contents, declaring the revision believed to be
// current and the hash of exactly these bytes.
//
// A `conflict` error is not a failure to retry: it means the server kept its
// version, stored ours under another name, and both now exist.
func (c *Client) Upload(ctx context.Context, rel, file string, baseRev int64, sha string) (FileResponse, error) {
	f, err := os.Open(file)
	if err != nil {
		return FileResponse{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return FileResponse{}, err
	}

	req, err := c.request(ctx, http.MethodPut, c.pathURL("files", rel), f)
	if err != nil {
		return FileResponse{}, err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Base-Rev", strconv.FormatInt(baseRev, 10))
	// The server rehashes the body and refuses a mismatch, so a file that
	// changed underneath the upload fails loudly instead of being stored
	// corrupt. We upload a snapshot, so this should never fire — which is
	// exactly why it is worth sending.
	if sha != "" {
		req.Header.Set("X-Content-SHA256", sha)
	}

	res, err := c.HTTP.Do(req)
	if err != nil {
		return FileResponse{}, err
	}
	defer res.Body.Close()

	if err := check(res); err != nil {
		return FileResponse{}, err
	}

	var out FileResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return FileResponse{}, fmt.Errorf("decode upload response: %w", err)
	}
	return out, nil
}

// Delete removes a path at a known revision.
func (c *Client) Delete(ctx context.Context, endpoint, rel string, baseRev int64) error {
	req, err := c.request(ctx, http.MethodDelete, c.pathURL(endpoint, rel), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Base-Rev", strconv.FormatInt(baseRev, 10))

	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return check(res)
}

// Mkdir creates a directory, including any missing parents.
func (c *Client) Mkdir(ctx context.Context, rel string) error {
	req, err := c.request(ctx, http.MethodPut, c.pathURL("dirs", rel), nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return check(res)
}

// IgnoreRules fetches the shared rules.
func (c *Client) IgnoreRules(ctx context.Context) (IgnoreDocument, error) {
	req, err := c.request(ctx, http.MethodGet, c.BaseURL+"/v1/ignore", nil)
	if err != nil {
		return IgnoreDocument{}, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return IgnoreDocument{}, err
	}
	defer res.Body.Close()

	if err := check(res); err != nil {
		return IgnoreDocument{}, err
	}

	var doc IgnoreDocument
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return IgnoreDocument{}, fmt.Errorf("decode ignore rules: %w", err)
	}
	return doc, nil
}

// Events yields the revision numbers the server announces.
//
// The payload is only a number: the caller always follows up with Changes.
// That is what makes a dropped, delayed or coalesced event harmless, and it is
// why the stream ending is not an error.
func (c *Client) Events(ctx context.Context, out chan<- int64) error {
	req, err := c.request(ctx, http.MethodGet, c.BaseURL+"/v1/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if err := check(res); err != nil {
		return err
	}

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 0, 4<<10), 64<<10)

	for scanner.Scan() {
		line := scanner.Text()
		// Comment lines are the server's heartbeat.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var payload struct {
			Rev int64 `json:"rev"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			continue
		}

		select {
		case out <- payload.Rev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return scanner.Err()
}

// Reachable makes one cheap authenticated request, which answers everything
// that can be wrong at startup: unreachable host, wrong port, bad token.
func (c *Client) Reachable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	_, err := c.Changes(ctx, 0, 1)
	return err
}
