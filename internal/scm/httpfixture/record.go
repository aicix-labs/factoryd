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
	"time"
)

// headersWorthRecording are the response headers the drivers actually read.
// Everything else is dropped: recording every header would bury the meaningful
// ones and drag in rate-limit counters and request ids that change on every
// run.
var headersWorthRecording = []string{"Link", "X-Next-Page", "X-Total-Pages"}

// Recorder is an http.RoundTripper that captures live exchanges into a Bundle.
//
// It records what the driver actually sent and received, which is the point:
// the hand-written fixtures it replaces described what the API was believed to
// do. A driver that perfectly matches a wrong fixture passes every test above
// it, and nothing looks broken.
type Recorder struct {
	// Inner is the real transport. Nil means http.DefaultTransport.
	Inner http.RoundTripper
	// Redactor rewrites live values before anything is written.
	Redactor *Redactor
	// Secrets must never appear in a written fixture. Writing fails if one
	// does.
	Secrets []string

	mu  sync.Mutex
	raw []rawExchange
}

// rawExchange is what actually crossed the wire, kept unredacted until the
// fixture is written.
//
// Redaction happens at write time, not at capture time, because some of the
// values that need mapping do not exist yet when the request is made. A merge
// creates its merge commit during the exchange being recorded; a redactor built
// beforehand cannot know that sha.
type rawExchange struct {
	method   string
	path     string
	status   int
	reqBody  []byte
	respBody []byte
	header   map[string]string
}

// NewRecorder returns a Recorder using the default transport.
func NewRecorder(red *Redactor, secrets []string) *Recorder {
	return &Recorder{Redactor: red, Secrets: secrets}
}

// Client returns an *http.Client wired to this recorder.
func (r *Recorder) Client() *http.Client { return &http.Client{Transport: r} }

// Reset drops everything captured so far, so one recorder can drive several
// scenarios in sequence.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.raw = nil
}

func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		reqBody = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}

	inner := r.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	resp, err := inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	respBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	if readErr != nil {
		return resp, readErr
	}

	r.capture(req, reqBody, resp, respBody)
	return resp, nil
}

func (r *Recorder) capture(req *http.Request, reqBody []byte, resp *http.Response, respBody []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e := rawExchange{
		method:   req.Method,
		path:     canonPath(req),
		status:   resp.StatusCode,
		reqBody:  append([]byte(nil), reqBody...),
		respBody: append([]byte(nil), respBody...),
	}
	for _, h := range headersWorthRecording {
		if v := resp.Header.Get(h); v != "" {
			if e.header == nil {
				e.header = map[string]string{}
			}
			e.header[h] = v
		}
	}
	r.raw = append(r.raw, e)
}

// render applies the redactor to everything captured. It is called at write
// time, so mappings registered after the exchanges happened still take effect.
func (r *Recorder) render() []Exchange {
	out := make([]Exchange, 0, len(r.raw))
	for _, raw := range r.raw {
		e := Exchange{
			Method: raw.method,
			Path:   r.Redactor.Apply(raw.path),
			Status: raw.status,
		}
		if len(bytes.TrimSpace(raw.respBody)) > 0 {
			red := r.Redactor.Apply(string(raw.respBody))
			// Re-marshal through JSON so the committed fixture is formatted,
			// and so a body that is not JSON is caught here, not at replay.
			var v any
			if err := json.Unmarshal([]byte(red), &v); err != nil {
				e.Body = json.RawMessage(mustMarshal(red))
			} else {
				e.Body = json.RawMessage(mustMarshal(v))
			}
		}
		for k, v := range raw.header {
			if e.Header == nil {
				e.Header = map[string]string{}
			}
			e.Header[k] = r.Redactor.Apply(v)
		}
		// A request body becomes an assertion, not a record: the fixture
		// asserts the driver sent these substrings. Recording the whole body
		// would make replay brittle against field ordering the driver does not
		// control.
		if len(bytes.TrimSpace(raw.reqBody)) > 0 {
			e.RequestBodyContains = requestAssertions(r.Redactor.Apply(string(raw.reqBody)))
		}
		out = append(out, e)
	}
	return out
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`"<unmarshalable>"`)
	}
	return b
}

// requestAssertions picks the parts of a request body worth asserting: the
// top-level scalar fields, rendered as the JSON fragments a driver must emit.
func requestAssertions(body string) []string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		// Not an object: assert the whole thing.
		return []string{strings.TrimSpace(body)}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []string
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v == "" {
				continue
			}
			// Marshalled, not %q. Go's encoder escapes <, > and & as \u003c
			// and friends, so a fragment built by quoting the parsed value does
			// not appear in the bytes the driver actually sent -- and the
			// assertion fails for a reason that has nothing to do with the
			// driver. An audit comment contains "<!--", so this is not
			// hypothetical.
			out = append(out, string(mustMarshal(k))+":"+string(mustMarshal(v)))
		case bool, float64:
			out = append(out, string(mustMarshal(k))+":"+string(mustMarshal(v)))
		}
	}
	return out
}

// Exchanges returns what has been captured, redacted as it would be written.
func (r *Recorder) Exchanges() []Exchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.render()
}

// Write saves the captured exchanges as dir/<name>.json.
//
// It refuses to write a bundle with no exchanges, and refuses to write one that
// still contains a secret. Both refusals matter: an empty bundle asserts
// nothing, and a leaked token in a committed fixture is not something anyone
// will spot in review.
func (r *Recorder) Write(dir, name string, src Source) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.raw) == 0 {
		return fmt.Errorf("recorder: scenario %q captured no exchanges; an empty fixture asserts nothing", name)
	}
	src.RecordedAt = FixedTimestamp
	b := Bundle{Name: name, Source: &src, Exchanges: r.render()}

	raw, err := json.MarshalIndent(struct {
		Source    *Source    `json:"source"`
		Exchanges []Exchange `json:"exchanges"`
	}{b.Source, b.Exchanges}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if err := MustNotLeak(string(raw), r.Secrets); err != nil {
		return fmt.Errorf("recorder: scenario %q: %w", name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o644)
}

// RecordedNow is a Source for a live capture.
func RecordedNow(provider, version string) Source {
	return Source{
		Recorded: true, Provider: provider, ProviderVersion: version,
		RecordedAt: time.Time{}.Format(time.RFC3339),
	}
}
