package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/scm"
)

func runSCM(args []string) int {
	fs := flag.NewFlagSet("scm", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "factory config file")
	role := fs.String("role", "reviewer", "producer or reviewer; selects the credential")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	rest := fs.Args()
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "factoryd scm: --config is required")
		return exitConfig
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "factoryd scm: a verb is required")
		return exitError
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd scm: %v\n", err)
		return exitConfig
	}

	// The credential is chosen by role and never falls back. A producer that
	// cannot resolve its own token must fail, not borrow the reviewer's.
	var ref config.CredentialRef
	switch *role {
	case "producer":
		ref = cfg.Credentials.Producer
	case "reviewer":
		ref = cfg.Credentials.Reviewer
	default:
		fmt.Fprintf(os.Stderr, "factoryd scm: --role %q is not producer or reviewer\n", *role)
		return exitConfig
	}
	token, err := ref.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd scm: %s credential: %v\n", *role, err)
		return exitConfig
	}

	d, err := factory.NewDriver(cfg, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factoryd scm: %v\n", err)
		return exitConfig
	}
	return scmVerb(context.Background(), d, rest[0], rest[1:], &printer{json: *asJSON})
}

// scmVerb dispatches one verb against a driver. Separate from the credential
// and driver construction so a verb's guards can be tested against a fake.
func scmVerb(ctx context.Context, d scm.Driver, verb string, rest []string, out *printer) int {
	need := func(n int, form string) bool {
		if len(rest) < n {
			fmt.Fprintf(os.Stderr, "factoryd scm %s: usage: %s\n", verb, form)
			return false
		}
		return true
	}

	switch verb {
	case "whoami":
		id, err := d.Whoami(ctx)
		return out.emit(id, err, func() string { return id.String() + "\n" })

	case "list-open":
		cs, err := d.ListOpen(ctx)
		return out.emit(cs, err, func() string {
			var sb strings.Builder
			for _, c := range cs {
				draft := ""
				if c.Draft {
					draft = " [draft]"
				}
				fmt.Fprintf(&sb, "%s\t%s -> %s\t%s%s\n", c.ID, c.SourceBranch, c.TargetBranch, c.Title, draft)
			}
			if len(cs) == 0 {
				sb.WriteString("no open changes\n")
			}
			return sb.String()
		})

	case "get":
		if !need(1, "get <id>") {
			return exitError
		}
		c, err := d.Get(ctx, scm.ChangeID(rest[0]))
		return out.emit(c, err, func() string {
			return fmt.Sprintf("%s %q\n  author %s\n  %s -> %s\n  head %s\n  draft %v state %v\n  %s\n",
				c.ID, c.Title, c.Author, c.SourceBranch, c.TargetBranch, c.HeadSHA, c.Draft, c.State, c.WebURL)
		})

	case "diff":
		if !need(1, "diff <id>") {
			return exitError
		}
		fsx, err := d.Diff(ctx, scm.ChangeID(rest[0]))
		return out.emit(fsx, err, func() string {
			var sb strings.Builder
			for _, f := range fsx {
				name := f.Path
				if f.Renamed {
					name = f.OldPath + " -> " + f.Path
				}
				fmt.Fprintf(&sb, "+%d -%d\t%s\n", f.Added, f.Removed, name)
			}
			return sb.String()
		})

	case "pipeline":
		if !need(1, "pipeline <id>") {
			return exitError
		}
		p, err := d.Pipeline(ctx, scm.ChangeID(rest[0]))
		return out.emit(p, err, func() string {
			return fmt.Sprintf("%s (%d pipelines for %s) green=%v\n%s\n",
				p.State, p.Count, p.SHA, p.State.Green(), p.WebURL)
		})

	case "audits":
		if !need(1, "audits <id>") {
			return exitError
		}
		c, err := d.Get(ctx, scm.ChangeID(rest[0]))
		if err != nil {
			return out.fail(err)
		}
		as, err := d.Audits(ctx, c.ID, c.HeadSHA)
		return out.emit(as, err, func() string {
			var sb strings.Builder
			for _, a := range as {
				fmt.Fprintf(&sb, "%s\t%s\t%d attempts\tby %s\n", a.Lens, a.Verdict, len(a.Attempts), a.PostedBy)
			}
			if len(as) == 0 {
				fmt.Fprintf(&sb, "no audits recorded against head %s\n", c.HeadSHA)
			}
			return sb.String()
		})

	case "is-ancestor":
		if !need(2, "is-ancestor <sha> <ref>") {
			return exitError
		}
		ok, err := d.IsAncestor(ctx, rest[0], rest[1])
		if err != nil {
			return out.fail(err)
		}
		if out.json {
			return out.emit(map[string]bool{"is_ancestor": ok}, nil, nil)
		}
		fmt.Printf("%v\n", ok)
		if !ok {
			// A false answer is a real answer, not an error; but a caller
			// scripting this needs it to be distinguishable.
			return exitNothing
		}
		return exitOK

	case "comment":
		if !need(2, "comment <id> <body>") {
			return exitError
		}
		if err := d.Comment(ctx, scm.ChangeID(rest[0]), strings.Join(rest[1:], " ")); err != nil {
			return out.fail(err)
		}
		return exitOK

	case "set-draft":
		if !need(2, "set-draft <id> true|false") {
			return exitError
		}
		draft, err := strconv.ParseBool(rest[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "factoryd scm set-draft: %q is not true or false\n", rest[1])
			return exitError
		}
		if err := d.SetDraft(ctx, scm.ChangeID(rest[0]), draft); err != nil {
			return out.fail(err)
		}
		return exitOK

	case "close":
		// The reviewer retires a draft it has determined is superseded
		// (#36). Submit never closes: a read is stale by the time a write
		// lands. The reviewer, who has read both, does -- and only an open
		// change: the provider would refuse a closed or merged one, but a
		// refusal here says what was found rather than what the provider
		// felt like saying.
		if !need(1, "close <id> [reason]") {
			return exitError
		}
		id := scm.ChangeID(rest[0])
		c, err := d.Get(ctx, id)
		if err != nil {
			return out.fail(err)
		}
		if c.State != scm.StateOpen {
			return out.fail(fmt.Errorf("change %s is %s, not open; nothing to close", id, c.State))
		}
		reason := "superseded by a newer submission"
		if len(rest) > 1 {
			reason = strings.Join(rest[1:], " ")
		}
		if err := d.Close(ctx, id, reason); err != nil {
			return out.fail(err)
		}
		// Verified, not believed: the change is re-read and must be closed.
		after, err := d.Get(ctx, id)
		if err != nil {
			return out.fail(fmt.Errorf("close reported success but the change could not be re-read: %w", err))
		}
		if after.State == scm.StateOpen {
			return out.fail(fmt.Errorf("close reported success but change %s is still open", id))
		}
		return exitOK

	case "merge":
		if !need(2, "merge <id> <expected-head>") {
			return exitError
		}
		id := scm.ChangeID(rest[0])
		c, err := d.Get(ctx, id)
		if err != nil {
			return out.fail(err)
		}
		r, err := scm.MergeVerified(ctx, d, id, rest[1], c.TargetBranch)
		if err != nil {
			return out.fail(err)
		}
		if out.json {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
				"outcome": r.Outcome.String(), "merge_commit": r.MergeCommit,
				"claimed_commit": r.ClaimedCommit,
				"verified":       r.Verified(), "reason": r.Reason,
			})
		} else if r.Outcome == scm.Merged {
			fmt.Printf("merged %s into %s (verified)\n", r.MergeCommit, c.TargetBranch)
		} else {
			fmt.Printf("%s: %s\n", r.Outcome, r.Reason)
			if r.ClaimedCommit != "" {
				fmt.Printf("  the provider claimed commit %s; check it by hand\n", r.ClaimedCommit)
			}
		}
		// The exit code carries the outcome. A merge that did not merge must
		// never exit 0 -- that was v1's defining bug.
		if r.Outcome != scm.Merged {
			return exitNothing
		}
		return exitOK

	default:
		fmt.Fprintf(os.Stderr, "factoryd scm: unknown verb %q\n", verb)
		return exitError
	}
}

type printer struct{ json bool }

func (p *printer) fail(err error) int {
	fmt.Fprintf(os.Stderr, "factoryd scm: %v\n", err)
	return exitError
}

func (p *printer) emit(v any, err error, text func() string) int {
	if err != nil {
		return p.fail(err)
	}
	if p.json || text == nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return p.fail(err)
		}
		return exitOK
	}
	fmt.Print(text())
	return exitOK
}
