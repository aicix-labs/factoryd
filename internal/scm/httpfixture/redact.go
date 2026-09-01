package httpfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Redactor rewrites live provider values into the fixed placeholders the
// conformance suite asserts on.
//
// Two jobs, and the second is the one that matters. The obvious one is keeping
// hostnames, usernames and tokens for a real instance out of a public
// repository. The subtler one is that a recorded fixture full of real ids
// churns on every re-record, which makes the diff unreadable and hides the
// change that actually matters.
//
// Anything not explicitly mapped is still rewritten -- deterministically, from
// a hash of the value -- rather than passed through. A redactor that only
// rewrites what it was told about leaks whatever it was not told about, and
// nobody notices, because the fixture still looks plausible.
type Redactor struct {
	exact    map[string]string
	patterns []patternRule
}

type patternRule struct {
	re   *regexp.Regexp
	repl string
}

// NewRedactor returns an empty redactor.
func NewRedactor() *Redactor { return &Redactor{exact: map[string]string{}} }

// MapPattern registers a context-aware replacement, applied before the literal
// map.
//
// Numeric identifiers need this. A merge request iid of 3 cannot be replaced by
// blind string substitution -- it would rewrite every other 3 in the document,
// including parts of timestamps and unrelated ids. Anchoring to the surrounding
// syntax ("iid":3, /merge_requests/3) replaces the identifier and nothing else.
func (r *Redactor) MapPattern(re *regexp.Regexp, repl string) {
	r.patterns = append(r.patterns, patternRule{re: re, repl: repl})
}

// Map registers a literal replacement. Later calls for the same live value win.
func (r *Redactor) Map(live, placeholder string) {
	if live == "" || live == placeholder {
		return
	}
	r.exact[live] = placeholder
}

// Mappings returns the registered replacements, for diagnostics.
func (r *Redactor) Mappings() map[string]string {
	out := make(map[string]string, len(r.exact))
	for k, v := range r.exact {
		out[k] = v
	}
	return out
}

var (
	// Git object ids, full and abbreviated down to 7.
	reSHA = regexp.MustCompile(`\b[0-9a-f]{40}\b|\b[0-9a-f]{7,39}\b`)
	// RFC 3339 timestamps, which every provider stamps on everything.
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[Zz]|[+-]\d{2}:?\d{2})`)
	// Gravatar and avatar digests, which are hashes of real email addresses.
	reAvatarDigest = regexp.MustCompile(`\b[0-9a-f]{32,64}\b`)
)

// FixedTimestamp is what every recorded time becomes.
const FixedTimestamp = "2026-09-01T10:00:00Z"

// syntheticSHA derives a stable placeholder for an unmapped object id, so the
// same live value always becomes the same placeholder and re-recording produces
// no spurious diff.
func syntheticSHA(live string) string {
	sum := sha256.Sum256([]byte("factoryd-fixture-sha:" + live))
	return hex.EncodeToString(sum[:])[:len(live)]
}

func syntheticDigest(live string) string {
	sum := sha256.Sum256([]byte("factoryd-fixture-digest:" + live))
	return hex.EncodeToString(sum[:])[:len(live)]
}

// Apply rewrites every known value, then everything that looks like an
// identifier and was not claimed by a mapping.
func (r *Redactor) Apply(s string) string {
	for _, p := range r.patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}

	// Longest first, so a project path is replaced before its namespace and a
	// full sha before its abbreviation.
	keys := make([]string, 0, len(r.exact))
	for k := range r.exact {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, r.exact[k])
	}

	s = reTimestamp.ReplaceAllStringFunc(s, func(m string) string {
		// The Go zero time is not a live value and says nothing about the
		// instance. Normalising it would put a timestamp into a request-body
		// assertion that the driver will never send.
		if strings.HasPrefix(m, "0001-01-01") {
			return m
		}
		return FixedTimestamp
	})
	s = reSHA.ReplaceAllStringFunc(s, func(m string) string {
		// A value already rewritten to a placeholder must not be rewritten
		// again into noise.
		if isPlaceholderSHA(m) {
			return m
		}
		return syntheticSHA(m)
	})
	s = reAvatarDigest.ReplaceAllStringFunc(s, func(m string) string {
		if isPlaceholderSHA(m) {
			return m
		}
		return syntheticDigest(m)
	})
	return s
}

// isPlaceholderSHA reports whether a hex run is one of the suite's fixed
// placeholders (all one repeated character, e.g. the 40 a's used for a head).
func isPlaceholderSHA(s string) bool {
	if s == "" {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

// Leaks returns any of the given secret or instance-identifying values that
// still appear in s.
//
// This is the redactor's own check. Without it, a mapping the recorder forgot
// to register is invisible: the fixture is still valid JSON, the suite still
// passes, and the leak ships.
func Leaks(s string, values []string) []string {
	var found []string
	seen := map[string]bool{}
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		if strings.Contains(s, v) {
			found = append(found, v)
		}
	}
	sort.Strings(found)
	return found
}

// MustNotLeak returns an error naming every value that survived redaction.
func MustNotLeak(s string, values []string) error {
	if found := Leaks(s, values); len(found) > 0 {
		return fmt.Errorf("redaction left %d value(s) in the fixture: %s",
			len(found), strings.Join(found, ", "))
	}
	return nil
}
