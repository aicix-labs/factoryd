package scm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AuditMarker opens the machine-readable block inside an audit comment. Audits
// are stored as ordinary PR/MR comments so that a human reading the change sees
// exactly what the merge gate sees.
const AuditMarker = "<!-- factoryd:audit:v1"

const auditMarkerEnd = "-->"

type auditWire struct {
	Lens     string   `json:"lens"`
	Verdict  string   `json:"verdict"`
	SHA      string   `json:"sha"`
	Attempts []string `json:"attempts"`
	Notes    string   `json:"notes,omitempty"`
	// A pointer, because omitempty does not omit a zero time.Time. An audit
	// posted without a timestamp was writing "0001-01-01T00:00:00Z" into the
	// comment, which reads like data and is not.
	PostedAt *time.Time `json:"posted_at,omitempty"`
}

func parseVerdict(s string) (AuditVerdict, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CLEARED":
		return AuditCleared, nil
	case "BROKEN":
		return AuditBroken, nil
	default:
		// Deliberately not defaulting to CLEARED or to UNKNOWN-as-fine: an
		// unreadable verdict is an error the caller must see.
		return AuditUnknown, fmt.Errorf("unknown audit verdict %q", s)
	}
}

// EncodeAudit renders an audit as a comment body: a human-readable heading and
// the machine-readable block the merge gate parses.
func EncodeAudit(a Audit) (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	w := auditWire{
		Lens: a.Lens, Verdict: a.Verdict.String(), SHA: a.SHA,
		Attempts: a.Attempts, Notes: a.Notes,
	}
	if !a.PostedAt.IsZero() {
		at := a.PostedAt.UTC().Truncate(time.Second)
		w.PostedAt = &at
	}
	blob, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "### adversarial pass — %s — **%s**\n\n", a.Lens, a.Verdict)
	fmt.Fprintf(&sb, "Pinned to `%s`.\n\n", a.SHA)
	sb.WriteString("Attempted:\n\n")
	for _, at := range a.Attempts {
		fmt.Fprintf(&sb, "- %s\n", at)
	}
	if a.Notes != "" {
		fmt.Fprintf(&sb, "\n%s\n", a.Notes)
	}
	fmt.Fprintf(&sb, "\n%s\n%s\n%s\n", AuditMarker, blob, auditMarkerEnd)
	return sb.String(), nil
}

// ParseAudit extracts an audit from a comment body. ok is false when the body
// carries no audit block at all; a body that carries a malformed block returns
// an error rather than being skipped, because a silently dropped audit reads to
// the merge gate as "no audit was ever posted".
func ParseAudit(body string) (a Audit, ok bool, err error) {
	i := strings.Index(body, AuditMarker)
	if i < 0 {
		return Audit{}, false, nil
	}
	rest := body[i+len(AuditMarker):]
	j := strings.Index(rest, auditMarkerEnd)
	if j < 0 {
		return Audit{}, true, fmt.Errorf("audit block is not terminated by %q", auditMarkerEnd)
	}

	var w auditWire
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:j])), &w); err != nil {
		return Audit{}, true, fmt.Errorf("audit block is not valid JSON: %w", err)
	}
	v, err := parseVerdict(w.Verdict)
	if err != nil {
		return Audit{}, true, err
	}
	a = Audit{
		Lens: w.Lens, Verdict: v, SHA: w.SHA, Attempts: w.Attempts,
		Notes: w.Notes,
	}
	if w.PostedAt != nil {
		a.PostedAt = *w.PostedAt
	}
	if err := a.Validate(); err != nil {
		return Audit{}, true, err
	}
	return a, true, nil
}

// SelectAudits keeps the audits pinned to sha. An audit of an earlier commit
// says nothing about the current head, so filtering happens here rather than
// being left to each driver.
func SelectAudits(all []Audit, sha string) []Audit {
	if sha == "" {
		return nil
	}
	var out []Audit
	for _, a := range all {
		if a.SHA == sha {
			out = append(out, a)
		}
	}
	return out
}
