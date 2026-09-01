// Package httpfixture replays recorded HTTP exchanges so driver behaviour can
// be tested against real provider response shapes without network access.
//
// The transport is deliberately strict in both directions:
//
//   - a request the bundle does not contain fails the test, so a driver that
//     calls an endpoint nobody recorded cannot quietly succeed;
//   - an exchange the driver never made fails the test, so a fixture cannot
//     describe a check the driver does not actually perform.
//
// Both directions matter. A permissive fixture player is a check that cannot
// fail (SPEC.md §9).
package httpfixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Exchange is one recorded request/response pair.
type Exchange struct {
	Method string `json:"method"`
	// Path is the request path, including a sorted query string when the
	// request carries one ("/api/v4/projects/x/merge_base?refs[]=a&refs[]=b").
	Path string `json:"path"`
	// Status is the HTTP status to reply with.
	Status int `json:"status"`
	// Body is the raw response body. Recorded as JSON for readability; a
	// string body is emitted verbatim.
	Body json.RawMessage `json:"body,omitempty"`
	// Header are extra response headers (e.g. Link, for pagination).
	Header map[string]string `json:"header,omitempty"`
	// RequestBodyContains, when set, asserts the driver sent these substrings.
	// This is how "did the driver actually pin the merge to the expected head
	// SHA?" is checked, rather than assumed.
	RequestBodyContains []string `json:"request_body_contains,omitempty"`
}

// Bundle is an ordered set of exchanges for one scenario.
type Bundle struct {
	Name      string     `json:"name"`
	Exchanges []Exchange `json:"exchanges"`
}

// Load reads a bundle from dir/<name>.json.
func Load(dir, name string) (*Bundle, error) {
	p := filepath.Join(dir, name+".json")
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var b Bundle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if len(b.Exchanges) == 0 {
		return nil, fmt.Errorf("%s: bundle has no exchanges; an empty fixture asserts nothing", p)
	}
	for i, e := range b.Exchanges {
		if e.Method == "" || e.Path == "" || e.Status == 0 {
			return nil, fmt.Errorf("%s: exchange %d is missing method, path, or status", p, i)
		}
	}
	b.Name = name
	return &b, nil
}

// Transport replays a Bundle. It is safe for concurrent use.
type Transport struct {
	mu     sync.Mutex
	bundle *Bundle
	used   []bool
	errs   []string
}

// NewTransport returns a Transport replaying b.
func NewTransport(b *Bundle) *Transport {
	return &Transport{bundle: b, used: make([]bool, len(b.Exchanges))}
}

// Client returns an *http.Client wired to this transport.
func (t *Transport) Client() *http.Client { return &http.Client{Transport: t} }

// canonPath renders a request as "<path>" or "<path>?<sorted query>".
func canonPath(r *http.Request) string {
	q := r.URL.Query()
	// EscapedPath, not Path: a GitLab project id is URL-encoded ("acme%2Fwidgets")
	// and decoding it would record a path the driver never sent.
	path := r.URL.EscapedPath()
	if len(q) == 0 {
		return path
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(path)
	sb.WriteByte('?')
	first := true
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			if !first {
				sb.WriteByte('&')
			}
			first = false
			sb.WriteString(k)
			sb.WriteByte('=')
			sb.WriteString(v)
		}
	}
	return sb.String()
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	var reqBody []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		reqBody = b
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	want := canonPath(r)
	for i, e := range t.bundle.Exchanges {
		if t.used[i] || e.Method != r.Method || e.Path != want {
			continue
		}
		t.used[i] = true
		for _, sub := range e.RequestBodyContains {
			if !bytes.Contains(reqBody, []byte(sub)) {
				t.errs = append(t.errs, fmt.Sprintf(
					"%s %s: request body does not contain %q (body: %s)",
					e.Method, e.Path, sub, string(reqBody)))
			}
		}
		body := []byte(e.Body)
		if len(body) > 0 && body[0] == '"' {
			var s string
			if err := json.Unmarshal(body, &s); err == nil {
				body = []byte(s)
			}
		}
		resp := &http.Response{
			StatusCode: e.Status,
			Status:     fmt.Sprintf("%d %s", e.Status, http.StatusText(e.Status)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}
		for k, v := range e.Header {
			resp.Header.Set(k, v)
		}
		return resp, nil
	}

	t.errs = append(t.errs, fmt.Sprintf("unrecorded request: %s %s", r.Method, want))
	return nil, fmt.Errorf("httpfixture %q: unrecorded request %s %s", t.bundle.Name, r.Method, want)
}

// Done reports every discrepancy: unrecorded requests, failed request-body
// assertions, and recorded exchanges the driver never made. A nil return means
// the driver made exactly the calls the fixture describes.
func (t *Transport) Done() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	problems := append([]string(nil), t.errs...)
	for i, e := range t.bundle.Exchanges {
		if !t.used[i] {
			problems = append(problems, fmt.Sprintf(
				"recorded exchange never requested: %s %s", e.Method, e.Path))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("httpfixture %q:\n  %s", t.bundle.Name, strings.Join(problems, "\n  "))
}

// Deny returns a Transport that refuses every request. It is how a scenario
// asserts that a driver made no network call at all -- for example that an
// invalid audit is rejected locally rather than posted and rejected by the
// provider.
func Deny() *Transport {
	return &Transport{bundle: &Bundle{Name: "deny-all"}}
}
