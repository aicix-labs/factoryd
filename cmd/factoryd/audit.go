package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/scm"
)

// auditFile is what --file holds: the attempts an adversarial pass actually
// made, and notes. An audit that lists nothing tried is not a pass, and the
// driver refuses it before any request (SPEC.md §6.4).
type auditFile struct {
	Attempts []string `json:"attempts"`
	Notes    string   `json:"notes,omitempty"`
}

// runAudit: factoryd audit --config <f> post <id> <sha> --lens <l> --verdict CLEARED|BROKEN --file <f>
func runAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	lens := fs.String("lens", "", "what the pass looked for (e.g. authz-bypass)")
	verdict := fs.String("verdict", "", "CLEARED or BROKEN")
	file := fs.String("file", "", "JSON file: {\"attempts\": [...], \"notes\": \"...\"}")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	rest := fs.Args()
	if len(rest) > 3 {
		if err := fs.Parse(rest[3:]); err != nil {
			return exitError
		}
		rest = rest[:3]
	}
	if *cfgPath == "" || len(rest) != 3 || rest[0] != "post" {
		fmt.Fprintln(os.Stderr, "usage: factoryd audit --config <f> post <id> <sha> --lens <l> --verdict CLEARED|BROKEN --file <f>")
		return exitConfig
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd audit: %v\n", err)
		return exitConfig
	}
	var body auditFile
	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd audit: --file: %v\n", err)
		return exitConfig
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		fmt.Fprintf(os.Stderr, "factoryd audit: --file %s: %v\n", *file, err)
		return exitConfig
	}
	a := scm.Audit{Lens: *lens, SHA: rest[2], Attempts: body.Attempts, Notes: body.Notes}
	switch *verdict {
	case "CLEARED":
		a.Verdict = scm.AuditCleared
	case "BROKEN":
		a.Verdict = scm.AuditBroken
	default:
		fmt.Fprintln(os.Stderr, "factoryd audit: --verdict must be CLEARED or BROKEN")
		return exitConfig
	}
	if err := a.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "factoryd audit: %v\n", err)
		return exitConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, who, err := reviewerDriver(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd audit: %v\n", err)
		return exitConfig
	}
	// The head must be the head: an audit pinned to a commit that is no
	// longer the change's head is an audit of something else.
	c, err := d.Get(ctx, scm.ChangeID(rest[1]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd audit: %v\n", err)
		return exitConfig
	}
	if c.HeadSHA != a.SHA {
		fmt.Fprintf(os.Stderr, "factoryd audit: change %s head is %s, not %s; audit the head that exists\n", c.ID, c.HeadSHA, a.SHA)
		return exitUnhealthy
	}
	if err := d.PostAudit(ctx, c.ID, a.SHA, a); err != nil {
		fmt.Fprintf(os.Stderr, "factoryd audit: %v\n", err)
		return exitConfig
	}
	fmt.Printf("audit %s %s posted on %s at %s by %s (%d attempts)\n", a.Lens, *verdict, c.ID, a.SHA, who.Login, len(a.Attempts))
	return exitOK
}
