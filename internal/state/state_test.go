package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aicix-labs/factoryd/internal/proc"
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

func TestClaimAndReleaseServiceUseTheExactProcessHandle(t *testing.T) {
	s := New("widgets")
	holder, err := proc.Self("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimService(ServiceStatusServe, holder); err != nil {
		t.Fatal(err)
	}
	if got := s.Service(ServiceStatusServe); got == nil || got.PID != holder.PID || got.StartToken != holder.StartToken {
		t.Fatalf("status service = %+v, want exact holder %+v", got, holder)
	}

	// A process with the same PID but a different start token is not the
	// holder. It must not clear an active service after PID reuse.
	s.ReleaseService(ServiceStatusServe, proc.Ref{PID: holder.PID, StartToken: holder.StartToken + "-different"})
	if got := s.Service(ServiceStatusServe); got == nil {
		t.Fatal("non-holder released the service")
	}
	s.ReleaseService(ServiceStatusServe, holder)
	if got := s.Service(ServiceStatusServe); got != nil {
		t.Fatalf("holder did not release service: %+v", got)
	}
}

func TestV2StateBlocksUntilServiceRegistryMigration(t *testing.T) {
	p := tmpPath(t)
	body := `{"schema_version":2,"factory":"widgets","roles":{},"verdict_registry":{"status":"ready"},"cycle":{"phase":"new"}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion != SchemaVersion {
		t.Fatalf("migrated schema version = %d, want %d", s.SchemaVersion, SchemaVersion)
	}
	if !errors.Is(s.ServiceRegistry.MigrationError(), ErrServiceRegistryMigrationRequired) {
		t.Fatalf("v2 service registry = %+v, want durable migration block", s.ServiceRegistry)
	}
	holder, err := proc.Self("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimService(ServiceStatusServe, holder); !errors.Is(err, ErrServiceRegistryMigrationRequired) {
		t.Fatalf("v2 registry admitted a new service before attestation: %v", err)
	}
	if _, err := Update(p, "widgets", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	persisted, err := Load(p, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(persisted.ServiceRegistry.MigrationError(), ErrServiceRegistryMigrationRequired) {
		t.Fatalf("persisted v2 upgrade silently trusted empty services: %+v", persisted.ServiceRegistry)
	}
	if err := MigrateServiceRegistry(p, "widgets"); err != nil {
		t.Fatal(err)
	}
	migrated, err := Load(p, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if !migrated.ServicesReady() || migrated.ServiceRegistry.AttestedAt.IsZero() {
		t.Fatalf("explicit service migration did not record an attestation: %+v", migrated.ServiceRegistry)
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

// Both roles' supervisors write this one document. Update is a
// read-modify-write, so without a lock the later writer silently discards
// whatever the other recorded in between.
func TestConcurrentUpdatesDoNotLoseWrites(t *testing.T) {
	p := tmpPath(t)
	if err := New("widgets").Save(p); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	const each = 25
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				if _, err := Update(p, "widgets", func(s *State) error {
					s.Role(RoleProducer).SpinCount++
					return nil
				}); err != nil {
					t.Errorf("update: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, err := Load(p, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if want := writers * each; got.Role(RoleProducer).SpinCount != want {
		t.Fatalf("spin count = %d after %d increments, want %d; %d updates were lost",
			got.Role(RoleProducer).SpinCount, want, want, want-got.Role(RoleProducer).SpinCount)
	}
}

// Two roles writing different fields must not clobber each other. This is the
// shape the supervisor actually produces: the producer touching its own state
// while the reviewer touches its own.
func TestConcurrentRolesKeepBothUpdates(t *testing.T) {
	p := tmpPath(t)
	if err := New("widgets").Save(p); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, role := range Roles {
		wg.Add(1)
		go func(r Role) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := Update(p, "widgets", func(s *State) error {
					s.Role(r).SpinCount++
					return nil
				}); err != nil {
					t.Errorf("%s: %v", r, err)
					return
				}
			}
		}(role)
	}
	wg.Wait()

	got, err := Load(p, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Roles {
		if n := got.Role(r).SpinCount; n != 50 {
			t.Errorf("role %s recorded %d of its 50 updates; the other role clobbered %d", r, n, 50-n)
		}
	}
}

// The lock must not leave the state file itself behind as a lock artefact, and
// the lock file must be distinct from the document.
func TestLockPathIsSeparateFromTheDocument(t *testing.T) {
	p := tmpPath(t)
	if _, err := Update(p, "widgets", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if LockPath(p) == p {
		t.Fatal("the lock file is the state file")
	}
	if _, err := os.Stat(LockPath(p)); err != nil {
		t.Fatalf("no lock file was created: %v", err)
	}
	if _, err := Load(p, "widgets"); err != nil {
		t.Fatalf("the document is unreadable after a locked update: %v", err)
	}
}

// A verdict may start a turn only with its lineage complete and coherent
// (#31): the old, pre-branch document shape is the exact input that
// reintroduces the stale-family failure, and it is refused.
func TestVerdictLineageFailsClosed(t *testing.T) {
	good := Verdict{ChangeID: "48", Kind: VerdictChangesRequested, SHA: "abc", Summary: "s", Branch: "producer/ci-host-0123456789", DeclaredBranch: "producer/ci-host"}
	if err := good.ValidateLineage("/x/outbox/48.json"); err != nil {
		t.Fatalf("a complete verdict was refused: %v", err)
	}
	cases := map[string]struct {
		mutate func(v *Verdict)
		path   string
		want   string
	}{
		"old format, no branch":          {func(v *Verdict) { v.Branch, v.DeclaredBranch = "", "" }, "/x/outbox/48.json", "no branch lineage"},
		"branch without family":          {func(v *Verdict) { v.DeclaredBranch = "" }, "/x/outbox/48.json", "no branch lineage"},
		"family not of the branch":       {func(v *Verdict) { v.DeclaredBranch = "producer/auth" }, "/x/outbox/48.json", "not the family"},
		"unknown kind":                   {func(v *Verdict) { v.Kind = "approved" }, "/x/outbox/48.json", "not one of"},
		"no change id":                   {func(v *Verdict) { v.ChangeID = "" }, "/x/outbox/48.json", "names no change"},
		"filename disagrees with the id": {func(v *Verdict) {}, "/x/outbox/49.json", "file named"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			v := good
			c.mutate(&v)
			err := v.ValidateLineage(c.path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%v, want %q", err, c.want)
			}
		})
	}
	if FamilyOf("producer/ci-host-0123456789") != "producer/ci-host" || FamilyOf("plain") != "plain" || FamilyOf("x-ZZZZZZZZZZ") != "x-ZZZZZZZZZZ" {
		t.Fatal("FamilyOf")
	}
}
