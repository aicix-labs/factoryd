// Package httpjson is the small JSON-over-HTTP client shared by the provider
// drivers. It exists so both drivers fail identically on transport and status
// errors -- divergence between the two drivers is the specific v1 defect the
// conformance suite guards against.
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Error is a non-2xx response. The body is retained because provider error
// messages ("Branch cannot be merged") are the difference between a useful
// refusal reason and a shrug.
type Error struct {
	Status int
	Method string
	URL    string
	Body   string
}

func (e *Error) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 400 {
		body = body[:400] + "..."
	}
	if body == "" {
		body = "<empty body>"
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, body)
}

// Message extracts a provider error message from a JSON body, falling back to
// the raw body.
func (e *Error) Message() string {
	var m struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal([]byte(e.Body), &m) == nil {
		if m.Message != "" {
			return m.Message
		}
		if m.Error != "" {
			return m.Error
		}
	}
	// GitLab sometimes returns a bare JSON string.
	var s string
	if json.Unmarshal([]byte(e.Body), &s) == nil && s != "" {
		return s
	}
	return strings.TrimSpace(e.Body)
}

// Client is a minimal JSON client bound to one API root.
type Client struct {
	Base   string
	HTTP   *http.Client
	Header http.Header
}

// Do issues a request. path may be an absolute URL (used when following
// pagination links) or a path relative to Base. A non-2xx status is returned as
// *Error and out is left untouched.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) (*http.Response, error) {
	url := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		url = strings.TrimSuffix(c.Base, "/") + path
	}

	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("%s %s: encoding request: %w", method, url, err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, fmt.Errorf("%s %s: reading body: %w", method, url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, &Error{Status: resp.StatusCode, Method: method, URL: url, Body: string(raw)}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp, fmt.Errorf("%s %s: decoding response: %w", method, url, err)
		}
	}
	return resp, nil
}

// StatusOf returns the HTTP status carried by err, or 0 if err is not an
// *Error.
func StatusOf(err error) int {
	var e *Error
	if AsError(err, &e) {
		return e.Status
	}
	return 0
}

// AsError is errors.As specialised to *Error, kept here so drivers do not each
// import errors for one call.
func AsError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// NextLink parses an RFC 5988 Link header and returns the rel="next" URL.
func NextLink(h http.Header) string {
	for _, link := range h.Values("Link") {
		for _, part := range strings.Split(link, ",") {
			seg := strings.Split(strings.TrimSpace(part), ";")
			if len(seg) < 2 {
				continue
			}
			url := strings.Trim(strings.TrimSpace(seg[0]), "<>")
			for _, attr := range seg[1:] {
				a := strings.ReplaceAll(strings.TrimSpace(attr), `"`, "")
				if a == "rel=next" {
					return url
				}
			}
		}
	}
	return ""
}
