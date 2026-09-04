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
	run := func(triggers, triggerPaths, changeBranch, verdicts string, brief string) (string, int) {
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
			"FACTORYD_CHANGE_BRANCH=" + changeBranch, "FACTORYD_VERDICTS=" + verdicts,
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
	prompt, rc := run("brief", briefPath, "", "[]", "Fix --verify in deploy/provision.sh.\n")
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

	// A verdict turn: the family, verbatim, and the verdicts JSON.
	vpath := filepath.Join(outbox, "48.json")
	os.WriteFile(vpath, []byte("{}"), 0o644)
	prompt, rc = run("verdict", vpath, "fix/ci-prereqs-verify-reporting", `[{"change_id":"48","kind":"changes-requested","declared_branch":"fix/ci-prereqs-verify-reporting"}]`, "")
	if rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if !strings.Contains(prompt, "THIS TURN IS A VERDICT") || !strings.Contains(prompt, "\n    fix/ci-prereqs-verify-reporting\n") || !strings.Contains(prompt, `"declared_branch":"fix/ci-prereqs-verify-reporting"`) {
		t.Fatalf("verdict prompt does not name the family verbatim:\n%s", prompt)
	}
	if _, err := os.Stat(vpath); !os.IsNotExist(err) {
		t.Fatal("the verdict trigger was not consumed")
	}

	// The exit code is the wrapper's: an agent that touched nothing is 1.
	agent = "cat > " + got
	if _, rc := run("brief", "", "", "[]", "do nothing\n"); rc != 1 {
		t.Fatalf("rc=%d, want 1 from the wrapper: the agent showed no progress", rc)
	}
	// Not under a supervisor: refused before any agent runs.
	cmd := exec.Command("sh", script, "true")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Run(); err == nil {
		t.Fatal("ran without FACTORYD_WORKDIR")
	}
}
