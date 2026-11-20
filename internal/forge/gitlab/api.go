package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	apiPrefix        = "/api/v4"
	userAgent        = "ansibled"
	requestTimeout   = 30 * time.Second
	maxResponseBytes = 16 << 20
	perPage          = "100"
	// defaultMaxPages bounds list endpoints that have no natural bound of their own
	// (10 000 items at per_page=100), so a misbehaving server cannot loop us forever.
	defaultMaxPages = 100
)

// APIError is a non-2xx answer from the GitLab API. Its message never contains
// the token: it is built from the method, the escaped API path and GitLab's own
// error message.
type APIError struct {
	Method     string
	Path       string // escaped path below /api/v4, e.g. "/projects/myorg%2Finfra"
	StatusCode int
	Message    string // GitLab's "message"/"error" field, if any
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("gitlab: %s %s: %d %s", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// IsNotFound reports whether err is a GitLab 404.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

// request is one API call. path is already escaped and relative to /api/v4.
type request struct {
	method string
	path   string
	query  url.Values
	body   any
}

// apiURL builds the request URL so that escaped segments ("%2F") survive.
func (c *Client) apiURL(escPath string, q url.Values) (*url.URL, error) {
	raw := strings.TrimRight(c.base.EscapedPath(), "/") + apiPrefix + escPath
	p, err := url.PathUnescape(raw)
	if err != nil {
		return nil, fmt.Errorf("gitlab: bad path %q: %w", escPath, err)
	}
	u := *c.base
	u.Path = p
	u.RawPath = raw
	u.RawQuery = ""
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return &u, nil
}

// do performs req and decodes a JSON answer into out (if non-nil). GETs are
// retried once on network errors and 5xx answers.
func (c *Client) do(ctx context.Context, req request, out any) (http.Header, error) {
	u, err := c.apiURL(req.path, req.query)
	if err != nil {
		return nil, err
	}
	var body []byte
	if req.body != nil {
		if body, err = json.Marshal(req.body); err != nil {
			return nil, fmt.Errorf("gitlab: %s %s: encode body: %w", req.method, req.path, err)
		}
	}
	attempts := 1
	if req.method == http.MethodGet {
		attempts = 2
	}
	for i := 0; ; i++ {
		h, err := c.attempt(ctx, req, u, body, out)
		if err == nil || i+1 >= attempts || !retryable(err) || ctx.Err() != nil {
			return h, err
		}
		c.log.Debug("gitlab: retrying request", "method", req.method, "path", req.path, "err", err)
		t := time.NewTimer(c.retryDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C:
		}
	}
}

// errTransport marks network-level failures (as opposed to API answers).
type errTransport struct{ err error }

func (e *errTransport) Error() string { return e.err.Error() }
func (e *errTransport) Unwrap() error { return e.err }

func retryable(err error) bool {
	var te *errTransport
	if errors.As(err, &te) {
		return true
	}
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode >= 500
}

func (c *Client) attempt(ctx context.Context, req request, u *url.URL, body []byte, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("gitlab: %s %s: %w", req.method, req.path, err)
	}
	hr.Header.Set("User-Agent", userAgent)
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("PRIVATE-TOKEN", c.opts.Token)
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, &errTransport{fmt.Errorf("gitlab: %s %s: %w", req.method, req.path, stripURL(err))}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &errTransport{fmt.Errorf("gitlab: %s %s: read body: %w", req.method, req.path, err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, &APIError{Method: req.method, Path: req.path, StatusCode: resp.StatusCode, Message: errorMessage(data)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, fmt.Errorf("gitlab: %s %s: decode answer: %w", req.method, req.path, err)
		}
	}
	return resp.Header, nil
}

// stripURL drops the *url.Error wrapper (its message repeats the full URL,
// which we already describe by method and path).
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// errorMessage extracts GitLab's error text from a JSON error body. GitLab
// answers {"message": "..."}, {"message": {"field": ["..."]}} or
// {"error": "...", "error_description": "..."}.
func errorMessage(data []byte) string {
	var e struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
		Desc    string          `json:"error_description"`
	}
	if json.Unmarshal(data, &e) != nil {
		return ""
	}
	var msg string
	switch {
	case len(e.Message) > 0:
		if json.Unmarshal(e.Message, &msg) != nil {
			var b bytes.Buffer
			if json.Compact(&b, e.Message) == nil {
				msg = b.String()
			}
		}
	case e.Error != "":
		msg = e.Error
		if e.Desc != "" {
			msg += ": " + e.Desc
		}
	}
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	return msg
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	_, err := c.do(ctx, request{method: http.MethodGet, path: path, query: q}, out)
	return err
}

// getPages walks a paginated list endpoint (per_page=100, following
// X-Next-Page) for at most maxPages pages, calling fn for each page until it
// returns false.
func getPages[T any](ctx context.Context, c *Client, path string, q url.Values, maxPages int, fn func([]T) bool) error {
	q2 := url.Values{}
	for k, v := range q {
		q2[k] = v
	}
	q2.Set("per_page", perPage)
	page := "1"
	for n := 0; n < maxPages; n++ {
		q2.Set("page", page)
		var items []T
		h, err := c.do(ctx, request{method: http.MethodGet, path: path, query: q2}, &items)
		if err != nil {
			return err
		}
		if !fn(items) {
			return nil
		}
		next := strings.TrimSpace(h.Get("X-Next-Page"))
		if next == "" || next == page || len(items) == 0 {
			return nil
		}
		page = next
	}
	return nil
}
