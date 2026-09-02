// Command fixturerec records conformance fixtures from a live provider.
//
// The fixtures it replaces were written by hand from API documentation. That is
// a check that can fail against the wrong reality: a driver matching a wrong
// fixture passes every test above it, and nothing looks broken. Error shapes
// are the part that matters most -- 403, 404, GitLab's 405 "Branch cannot be
// merged", 409 head mismatch, the draft refusal -- because that is where the
// drivers make their fail-closed decisions.
//
// This is development tooling. It is a separate binary from factoryd on
// purpose: it needs write access to a scratch repository and must never be
// something a factory can run.
//
//	go run ./cmd/fixturerec --provider gitlab \
//	  --base https://gitlab.example.com/api/v4 --project grp/scratch \
//	  --token-file ~/.config/token --out internal/scm/gitlab/testdata
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	var (
		provider  = flag.String("provider", "", "github or gitlab")
		base      = flag.String("base", "", "API root (default: the provider's public one)")
		project   = flag.String("project", "", "gitlab: group/project")
		owner     = flag.String("owner", "", "github: owner")
		repo      = flag.String("repo", "", "github: repo")
		tokenFile = flag.String("token-file", "", "file holding the API token")
		out       = flag.String("out", "", "testdata directory to write into")
		only      = flag.String("only", "", "comma-separated scenario names; default all")
		dryRun    = flag.Bool("dry-run", false, "set up and record, but do not write files")
	)
	flag.Parse()

	if *tokenFile == "" || *out == "" || *provider == "" {
		fmt.Fprintln(os.Stderr, "fixturerec: --provider, --token-file and --out are required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixturerec: %v\n", err)
		os.Exit(2)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		fmt.Fprintf(os.Stderr, "fixturerec: %s is empty\n", *tokenFile)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	var t target
	switch *provider {
	case "gitlab":
		if *project == "" {
			fmt.Fprintln(os.Stderr, "fixturerec: --project is required for gitlab")
			os.Exit(2)
		}
		t, err = newGitLabTarget(ctx, *base, *project, token)
	case "github":
		if *owner == "" || *repo == "" {
			fmt.Fprintln(os.Stderr, "fixturerec: --owner and --repo are required for github")
			os.Exit(2)
		}
		t, err = newGitHubTarget(ctx, *base, *owner, *repo, token)
	default:
		fmt.Fprintf(os.Stderr, "fixturerec: provider %q is not github or gitlab\n", *provider)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixturerec: %v\n", err)
		os.Exit(1)
	}

	wanted := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			wanted[n] = true
		}
	}

	fmt.Printf("recording %s against %s (%s)\n\n", *provider, t.describe(), t.version())

	var recorded, skipped, failed []string
	for _, sc := range scenarios() {
		if len(wanted) > 0 && !wanted[sc.name] {
			continue
		}
		if sc.synthetic != "" {
			fmt.Printf("  skip   %-28s %s\n", sc.name, sc.synthetic)
			skipped = append(skipped, sc.name)
			continue
		}
		if err := runScenario(ctx, t, sc, *out, *dryRun); err != nil {
			fmt.Printf("  FAIL   %-28s %v\n", sc.name, err)
			failed = append(failed, sc.name)
			continue
		}
		fmt.Printf("  ok     %-28s\n", sc.name)
		recorded = append(recorded, sc.name)
	}

	fmt.Println()
	report := func(label string, names []string) {
		if len(names) == 0 {
			return
		}
		sort.Strings(names)
		fmt.Printf("%s (%d): %s\n", label, len(names), strings.Join(names, ", "))
	}
	report("recorded", recorded)
	report("left synthetic", skipped)
	report("FAILED", failed)

	if len(failed) > 0 {
		// A scenario that could not be recorded keeps its hand-written fixture,
		// which is exactly the state this tool exists to leave behind. Exit
		// non-zero so that is a decision rather than something to notice later.
		os.Exit(1)
	}
}
