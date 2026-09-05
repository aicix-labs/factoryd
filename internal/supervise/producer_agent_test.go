package supervise_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// examples/producer-turn-agent.sh composes the intent protocol once, in
// the wrapper, so a brief only has to describe work (#38). Run for real,
// with a fake agent that records the prompt it was given on stdin.
func TestProducerAgentWrapperComposesTheProtocolAroundTheBrief(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "producer-turn-agent.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	inbox, outbox, work := filepath.Join(root, "inbox"), filepath.Join(root, "outbox"), filepath.Join(root, "work")
	for _, d := range []string{inbox, outbox, work} {
		os.MkdirAll(d, 0o755)
	}
	progress := filepath.Join(inbox, "producer-progress")
	got := filepath.Join(root, "prompt-seen")
	// The fake agent: record stdin, then "do the work" (touch progress).
	agent := "cat > " + got + " && touch " + progress
	run := func(triggers, triggerPaths, tsv string, brief string) (string, int) {
		t.Helper()
		os.Remove(got)
		if brief != "" {
			os.WriteFile(filepath.Join(inbox, "brief.md"), []byte(brief), 0o644)
		}
		cmd := exec.Command("sh", script, "sh", "-c", agent)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"FACTORYD_WORKDIR=" + work, "FACTORYD_INBOX=" + inbox, "FACTORYD_OUTBOX=" + outbox,
			"FACTORYD_PROGRESS=" + progress,
			"FACTORYD_TRIGGERS=" + triggers, "FACTORYD_TRIGGER_PATHS=" + triggerPaths,
			"FACTORYD_VERDICTS_TSV=" + tsv,
		}
		out, err := cmd.CombinedOutput()
		rc := 0
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		b, _ := os.ReadFile(got)
		return string(b), rc
	}

	// A brief turn: the protocol, then the brief, and the trigger consumed.
	briefPath := filepath.Join(inbox, "brief.md")
	prompt, rc := run("brief", briefPath, "", "Fix --verify in deploy/provision.sh.\n")
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	for _, want := range []string{".producer-branch", ".producer-commit-msg", "Nothing is submitted without both", "gate", "Do NOT try to push", "touch " + progress, "THE WORK (from the operator)", "Fix --verify in deploy/provision.sh."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Count(prompt, "Declare intent by writing TWO files") != 1 {
		t.Fatalf("the protocol appears %d times", strings.Count(prompt, "Declare intent by writing TWO files"))
	}
	if strings.Contains(prompt, "THIS TURN IS A VERDICT") {
		t.Fatal("a brief turn was told it is a verdict")
	}
	if _, err := os.Stat(briefPath); !os.IsNotExist(err) {
		t.Fatal("the brief was not consumed")
	}

	// Verdict turns, by kind (#50 review). merged and operator-gated must
	// produce NO declaration -- a re-declaration there resubmits a change
	// that left the producer's hands (#40); only changes-requested
	// re-declares, and then the exact family, read from the runner's
	// tab-separated rendering, never parsed out of JSON.
	line := func(path, id, kind, fam string) string {
		return path + "\t" + id + "\t" + kind + "\t" + fam + "-0123456789\t" + fam + "\n"
	}
	vpath := filepath.Join(outbox, "48.json")
	for _, c := range []struct {
		kind       string
		wantDecl   bool
		wantPhrase string
	}{
		{"changes-requested", true, "fix it, then re-declare the SAME FAMILY"},
		{"merged", false, "Declare NOTHING for it (write neither file)"},
		{"operator-gated", false, "Declare NOTHING for it; wait"},
	} {
		os.WriteFile(vpath, []byte("{}"), 0o644)
		prompt, rc := run("verdict", vpath, line(vpath, "48", c.kind, "fix/{example}"), "")
		if rc != 0 {
			t.Fatalf("%s: rc=%d", c.kind, rc)
		}
		if !strings.Contains(prompt, "THIS TURN IS A VERDICT") || !strings.Contains(prompt, "  - change 48: "+c.kind+" (family fix/{example})") {
			t.Fatalf("%s: the verdict is not listed by change, kind and family (a { family must survive):\n%s", c.kind, prompt)
		}
		if !strings.Contains(prompt, c.wantPhrase) {
			t.Fatalf("%s: prompt lacks %q:\n%s", c.kind, c.wantPhrase, prompt)
		}
		named := strings.Contains(prompt, "verbatim:\n    fix/{example}\n")
		if c.wantDecl && (!named || !strings.Contains(prompt, "THIS TURN ACTS ON ONE changes-requested verdict: change 48")) {
			t.Fatalf("%s: the family to re-declare is not stated verbatim:\n%s", c.kind, prompt)
		}
		if !c.wantDecl && (named || !strings.Contains(prompt, "No verdict this turn asks for a declaration")) {
			t.Fatalf("%s: the producer was told to re-declare, or not told to declare nothing:\n%s", c.kind, prompt)
		}
		// The fake agent touched progress and declared nothing. For merged
		// and operator-gated that is the right outcome and the trigger is
		// consumed; for changes-requested the verdict was NOT acted on, and
		// the trigger is kept for the retry (#50 review).
		_, statErr := os.Stat(vpath)
		if c.wantDecl && statErr != nil {
			t.Fatalf("%s: the selected verdict was consumed by a turn that declared nothing; the retry would have no verdict", c.kind)
		}
		if !c.wantDecl && !os.IsNotExist(statErr) {
			t.Fatalf("%s: the verdict trigger was not consumed", c.kind)
		}
		os.Remove(vpath)
	}

	// The selected changes-requested trigger is consumed only by a turn
	// that succeeded AND declared completely. Non-zero exit: kept. Zero
	// with progress and no intent: kept (proved above). Zero with one
	// control file: kept. Zero with both: consumed.
	declaring := func(files string) string {
		return "cat > " + got + " && touch " + progress + " && " + files
	}
	cases := []struct {
		name  string
		agent string
		keep  bool
	}{
		{"exit 7 after declaring both", declaring("printf 'fix/{example}\\n' > "+work+"/.producer-branch && printf 'msg\\n' > "+work+"/.producer-commit-msg") + " && exit 7", true},
		{"only .producer-branch", declaring("printf 'fix/{example}\\n' > " + work + "/.producer-branch"), true},
		{"only .producer-commit-msg", declaring("printf 'msg\\n' > " + work + "/.producer-commit-msg"), true},
		{"empty control files", declaring(": > " + work + "/.producer-branch && : > " + work + "/.producer-commit-msg"), true},
		{"both files, exit 0", declaring("printf 'fix/{example}\\n' > " + work + "/.producer-branch && printf 'msg\\n' > " + work + "/.producer-commit-msg"), false},
	}
	for _, c := range cases {
		os.Remove(filepath.Join(work, ".producer-branch"))
		os.Remove(filepath.Join(work, ".producer-commit-msg"))
		os.WriteFile(vpath, []byte("{}"), 0o644)
		agent = c.agent
		_, _ = run("verdict", vpath, line(vpath, "48", "changes-requested", "fix/{example}"), "")
		_, err := os.Stat(vpath)
		if c.keep && err != nil {
			t.Fatalf("%s: the selected verdict was consumed", c.name)
		}
		if !c.keep && !os.IsNotExist(err) {
			t.Fatalf("%s: the selected verdict was not consumed after a complete declaration", c.name)
		}
	}
	agent = "cat > " + got + " && touch " + progress
	os.Remove(filepath.Join(work, ".producer-branch"))
	os.Remove(filepath.Join(work, ".producer-commit-msg"))

	// Two changes-requested verdicts (#50 review): a turn has one
	// declaration, so exactly one is acted on -- the first -- its family
	// stated verbatim, its trigger consumed; the other's trigger is KEPT
	// for a later turn, never lost. A merged verdict alongside is listed and
	// its trigger consumed.
	p48, p49, p50 := filepath.Join(outbox, "48.json"), filepath.Join(outbox, "49.json"), filepath.Join(outbox, "50.json")
	for _, p := range []string{p48, p49, p50} {
		os.WriteFile(p, []byte("{}"), 0o644)
	}
	tsv := line(p48, "48", "merged", "fix/done") + line(p49, "49", "changes-requested", "fix/first") + line(p50, "50", "changes-requested", "fix/second")
	agent = declaring("printf 'fix/first\\n' > " + work + "/.producer-branch && printf 'msg\\n' > " + work + "/.producer-commit-msg")
	prompt, rc = run("verdict", p48+":"+p49+":"+p50, tsv, "")
	if rc != 0 {
		t.Fatalf("two changes-requested: rc=%d", rc)
	}
	for _, want := range []string{"  - change 48: merged (family fix/done)", "  - change 49: changes-requested (family fix/first)", "  - change 50: changes-requested (family fix/second)",
		"THIS TURN ACTS ON ONE changes-requested verdict: change 49", "verbatim:\n    fix/first\n", "gets its own turn later"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("two changes-requested: prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "verbatim:\n    fix/second\n") {
		t.Fatal("two changes-requested: told to re-declare both families in one turn")
	}
	if _, err := os.Stat(p48); !os.IsNotExist(err) {
		t.Fatal("the merged verdict's trigger was not consumed")
	}
	if _, err := os.Stat(p49); !os.IsNotExist(err) {
		t.Fatal("the selected changes-requested trigger was not consumed")
	}
	if _, err := os.Stat(p50); err != nil {
		t.Fatal("the unselected changes-requested trigger was consumed; that verdict is lost")
	}
	// The next turn sees the remaining one and acts on it.
	agent = declaring("printf 'fix/second\\n' > " + work + "/.producer-branch && printf 'msg\\n' > " + work + "/.producer-commit-msg")
	prompt, rc = run("verdict", p50, line(p50, "50", "changes-requested", "fix/second"), "")
	if rc != 0 || !strings.Contains(prompt, "verbatim:\n    fix/second\n") {
		t.Fatalf("the kept verdict was not acted on next: rc=%d\n%s", rc, prompt)
	}
	if _, err := os.Stat(p50); !os.IsNotExist(err) {
		t.Fatal("the kept verdict's trigger was not consumed on its own turn")
	}
	agent = "cat > " + got + " && touch " + progress
	os.Remove(filepath.Join(work, ".producer-branch"))
	os.Remove(filepath.Join(work, ".producer-commit-msg"))
	// An old factoryd without the rendering: refused, nothing consumed.
	os.WriteFile(p48, []byte("{}"), 0o644)
	if _, rc := run("verdict", p48, "", ""); rc != 3 {
		t.Fatalf("no TSV: rc=%d, want 3", rc)
	}
	if _, err := os.Stat(p48); err != nil {
		t.Fatal("a refused turn consumed its trigger")
	}

	// The supervisor's retry marker is not the turn's to remove (#50
	// review): it survives the turn, whatever the agent did.
	retry := filepath.Join(inbox, "producer-retry")
	os.WriteFile(retry, []byte("retry 3 of 3\n"), 0o644)
	os.WriteFile(briefPath, []byte("again\n"), 0o644)
	if _, rc := run("retry,brief", retry+":"+briefPath, "", ""); rc != 0 {
		t.Fatalf("retry turn rc=%d", rc)
	}
	if _, err := os.Stat(retry); err != nil {
		t.Fatal("the wrapper removed the supervisor's retry marker")
	}
	if _, err := os.Stat(briefPath); !os.IsNotExist(err) {
		t.Fatal("the brief beside the retry marker was not consumed")
	}
	// And on a failing agent the marker is still there for the supervisor.
	agentFail := "cat > " + got + "; exit 7"
	os.WriteFile(retry, []byte("retry 3 of 3\n"), 0o644)
	failing := exec.Command("sh", script, "sh", "-c", agentFail)
	failing.Env = []string{"PATH=" + os.Getenv("PATH"), "FACTORYD_WORKDIR=" + work, "FACTORYD_INBOX=" + inbox, "FACTORYD_OUTBOX=" + outbox, "FACTORYD_PROGRESS=" + progress, "FACTORYD_TRIGGERS=retry", "FACTORYD_TRIGGER_PATHS=" + retry, "FACTORYD_VERDICTS_TSV="}
	if err := failing.Run(); err == nil {
		t.Fatal("a failing agent exited 0")
	}
	if _, err := os.Stat(retry); err != nil {
		t.Fatal("the retry marker was removed by a failing turn; the halt's evidence is gone")
	}

	// The claim about the sandbox is the true one: no remote, no credential,
	// no push or fetch -- not "no network", which a hosted model needs.
	if strings.Contains(prompt, "no network") {
		t.Fatal("the prompt claims no network; a hosted-model producer has network")
	}
	if !strings.Contains(prompt, "no git remote and no provider credential") {
		t.Fatalf("the prompt does not state the real boundary:\n%s", prompt)
	}

	// The exit code is the wrapper's: an agent that touched nothing is 1.
	agent = "cat > " + got
	if _, rc := run("brief", "", "", "do nothing\n"); rc != 1 {
		t.Fatalf("rc=%d, want 1 from the wrapper: the agent showed no progress", rc)
	}
	// Not under a supervisor: refused before any agent runs.
	cmd := exec.Command("sh", script, "true")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Run(); err == nil {
		t.Fatal("ran without FACTORYD_WORKDIR")
	}
}
