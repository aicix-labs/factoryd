package state

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func tmpPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.json")
}

func TestMissingFileIsAFreshDocument(t *testing.T) {
	s, err := Load(tmpPath(t), "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if s.Factory != "widgets" || s.SchemaVersion != SchemaVersion {
		t.Fatalf("fresh document = %+v", s)
	}
	for _, r := range Roles {
		if s.Roles[r] == nil {
			t.Fatalf("fresh document has no state for role %s", r)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	p := tmpPath(t)
	s := New("widgets")
	s.Role(RoleProducer).SpinCount = 2
	s.Role(RoleProducer).LastProgressAt = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	s.LastVerdict = &Verdict{ChangeID: "42", Kind: VerdictMerged, Summary: "landed", MergeCommit: "abc"}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}

	got, err := Load(p, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role(RoleProducer).SpinCount != 2 {
		t.Fatalf("spin count = %d, want 2", got.Role(RoleProducer).SpinCount)
	}
	if got.LastVerdict == nil || got.LastVerdict.Kind != VerdictMerged {
		t.Fatalf("verdict = %+v", got.LastVerdict)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("Save did not stamp updated_at")
	}
}

// A state document from a schema this build does not understand must be
// refused. Guessing would mean operating on fields that may have changed
// meaning.
func TestUnknownSchemaVersionIsRefused(t *testing.T) {
	cases := map[string]string{
		"future version":  `{"schema_version": 99, "factory": "widgets"}`,
		"missing version": `{"factory": "widgets"}`,
		"zero version":    `{"schema_version": 0, "factory": "widgets"}`,
		"not json":        `{`,
	}
	for name, body := range cases {
		p := tmpPath(t)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p, "widgets"); err == nil {
			t.Errorf("%s: Load accepted it", name)
		}
	}
}

// The distinction that matters: an absent file is a fresh start, an unreadable
// file is an error. If both produced a fresh document, a corrupted state file
// would erase the record of whatever corrupted it.
func TestAbsentAndUnreadableDifferInOutcome(t *testing.T) {
	if _, err := Load(tmpPath(t), "widgets"); err != nil {
		t.Fatalf("absent file: %v", err)
	}
	p := tmpPath(t)
	if err := os.WriteFile(p, []byte(`{"schema_version": 99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, "widgets"); err == nil {
		t.Fatal("unreadable file was treated as a fresh start")
	}
}

func TestValidateRejectsInconsistentDocuments(t *testing.T) {
	cases := map[string]func(*State){
		"halted with no reason": func(s *State) {
			s.Role(RoleProducer).Halted = true
		},
		"negative spin count": func(s *State) {
			s.Role(RoleReviewer).SpinCount = -1
		},
		"a fourth verdict kind": func(s *State) {
			s.LastVerdict = &Verdict{ChangeID: "1", Kind: "looks-fine", Summary: "x"}
		},
		"no factory name": func(s *State) { s.Factory = "" },
	}
	for name, mutate := range cases {
		s := New("widgets")
		mutate(s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
		if err := s.Save(tmpPath(t)); err == nil {
			t.Errorf("%s: Save wrote it", name)
		}
	}
}

func TestHaltedWithAReasonIsAccepted(t *testing.T) {
	s := New("widgets")
	rs := s.Role(RoleProducer)
	rs.Halted = true
	rs.HaltReason = "spin abort: 6 turns with no progress"
	if err := s.Validate(); err != nil {
		t.Fatalf("a halt with a stated reason was rejected: %v", err)
	}
}

// Save must never leave a partially written document behind for a reader.
func TestSaveIsAtomicUnderConcurrentWriters(t *testing.T) {
	p := tmpPath(t)
	if err := New("widgets").Save(p); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: every read must see a complete, valid document.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := Load(p, "widgets"); err != nil {
					t.Errorf("reader saw a document it could not load: %v", err)
					return
				}
			}
		}()
	}

	for i := 0; i < 40; i++ {
		s := New("widgets")
		s.Role(RoleProducer).SpinCount = i
		if err := s.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()

	// No temporary files left behind.
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			t.Errorf("Save left a temporary file behind: %s", e.Name())
		}
	}
}

func TestUpdateAppliesAndPersists(t *testing.T) {
	p := tmpPath(t)
	if _, err := Update(p, "widgets", func(s *State) error {
		s.Role(RoleReviewer).SpinCount = 3
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role(RoleReviewer).SpinCount != 3 {
		t.Fatalf("spin count = %d, want 3", got.Role(RoleReviewer).SpinCount)
	}
}

// A failing update must not write. Otherwise a half-applied change persists
// under the same schema version and reads as intentional.
func TestUpdateDoesNotWriteOnError(t *testing.T) {
	p := tmpPath(t)
	if err := New("widgets").Save(p); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update(p, "widgets", func(s *State) error {
		s.Role(RoleProducer).SpinCount = 9
		return os.ErrInvalid
	}); err == nil {
		t.Fatal("Update reported success after its function failed")
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("Update wrote despite its function failing")
	}
}

func TestTurnAge(t *testing.T) {
	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)
	running := &Turn{ID: "t1", StartedAt: start}
	if !running.Running() {
		t.Fatal("a turn with no end time is not running")
	}
	if got := running.Age(start.Add(time.Minute)); got != time.Minute {
		t.Fatalf("running turn age = %v, want 1m", got)
	}
	done := &Turn{ID: "t1", StartedAt: start, EndedAt: &end}
	if done.Running() {
		t.Fatal("a finished turn reports running")
	}
	if got := done.Age(start.Add(time.Hour)); got != 90*time.Second {
		t.Fatalf("finished turn age = %v, want 90s", got)
	}
	var nilTurn *Turn
	if nilTurn.Running() {
		t.Fatal("a nil turn reports running")
	}
}
