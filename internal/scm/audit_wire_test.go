package scm

import (
	"strings"
	"testing"
	"time"
)

func goodAudit() Audit {
	return Audit{
		Lens:     "scope-escape",
		Verdict:  AuditCleared,
		SHA:      "abc123",
		Attempts: []string{"reverted the guard", "fed it a commented-out match"},
		Notes:    "no escape found",
		PostedBy: "factory-reviewer",
		PostedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	}
}

func TestAuditValidateRejectsRubberStamps(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Audit)
		wantErr bool
	}{
		{"complete audit", func(*Audit) {}, false},
		{"cleared with no attempts", func(a *Audit) { a.Attempts = nil }, true},
		{"broken with no attempts", func(a *Audit) { a.Verdict = AuditBroken; a.Attempts = nil }, true},
		{"unknown verdict", func(a *Audit) { a.Verdict = AuditUnknown }, true},
		{"no lens", func(a *Audit) { a.Lens = "" }, true},
		{"not pinned to a commit", func(a *Audit) { a.SHA = "" }, true},
		{"an empty attempt", func(a *Audit) { a.Attempts = []string{"tried", ""} }, true},
	}
	for _, c := range cases {
		a := goodAudit()
		c.mutate(&a)
		if err := a.Validate(); (err != nil) != c.wantErr {
			t.Errorf("%s: Validate() = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func TestAuditRoundTrip(t *testing.T) {
	in := goodAudit()
	body, err := EncodeAudit(in)
	if err != nil {
		t.Fatal(err)
	}
	// The human-readable half must carry the substance: a reviewer reading the
	// change should see what was tried without decoding anything.
	for _, want := range []string{"scope-escape", "CLEARED", "reverted the guard", in.SHA} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered audit does not mention %q", want)
		}
	}

	got, ok, err := ParseAudit(body)
	if err != nil || !ok {
		t.Fatalf("ParseAudit(ok=%v) = %v", ok, err)
	}
	if got.Lens != in.Lens || got.Verdict != in.Verdict || got.SHA != in.SHA {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if len(got.Attempts) != len(in.Attempts) {
		t.Fatalf("round trip kept %d attempts, want %d", len(got.Attempts), len(in.Attempts))
	}
}

func TestEncodeAuditRefusesInvalid(t *testing.T) {
	a := goodAudit()
	a.Attempts = nil
	if _, err := EncodeAudit(a); err == nil {
		t.Fatal("encoded an audit that records nothing tried")
	}
}

func TestParseAudit(t *testing.T) {
	t.Run("a plain comment is not an audit", func(t *testing.T) {
		_, ok, err := ParseAudit("looks good to me")
		if ok || err != nil {
			t.Fatalf("ok=%v err=%v, want false/nil", ok, err)
		}
	})

	// A malformed audit block must be an error, not a skip. Skipping it would
	// present to the merge gate as "no audit was posted", which is a different
	// and much quieter failure.
	for _, c := range []struct{ name, body string }{
		{"unterminated block", AuditMarker + "\n{\"lens\":\"x\"}\n"},
		{"not json", AuditMarker + "\nnot json at all\n-->"},
		{"unknown verdict", AuditMarker + "\n{\"lens\":\"x\",\"verdict\":\"PROBABLY\",\"sha\":\"a\",\"attempts\":[\"t\"]}\n-->"},
		{"no attempts", AuditMarker + "\n{\"lens\":\"x\",\"verdict\":\"CLEARED\",\"sha\":\"a\",\"attempts\":[]}\n-->"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ok, err := ParseAudit(c.body)
			if !ok {
				t.Fatal("a body carrying the audit marker was reported as carrying no audit")
			}
			if err == nil {
				t.Fatal("malformed audit parsed cleanly")
			}
		})
	}
}

func TestSelectAudits(t *testing.T) {
	head := "aaa"
	all := []Audit{
		{Lens: "a", SHA: "old"},
		{Lens: "b", SHA: head},
		{Lens: "c", SHA: head},
	}
	if got := SelectAudits(all, head); len(got) != 2 {
		t.Fatalf("selected %d audits, want 2", len(got))
	}
	// An empty sha must select nothing rather than everything: "no head sha"
	// must never widen into "every audit ever posted counts".
	if got := SelectAudits(all, ""); len(got) != 0 {
		t.Fatalf("an empty sha selected %d audits, want 0", len(got))
	}
}
