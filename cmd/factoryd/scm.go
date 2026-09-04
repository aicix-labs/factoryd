package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/factory"
	"github.com/aicix-labs/factoryd/internal/principal"
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
	operator := operatorPrincipal(cfg, factory.NewDriver)
	return scmVerb(context.Background(), d, operator, rest[0], rest[1:], &printer{json: *asJSON})
}

// operatorPrincipal resolves the operator's driver for the one verb that
// needs it. It never falls back to a role's token, and it proves the
// operator a third PROVIDER identity immediately before use: all three
// credentials resolved in this process's context and compared by stable
// id, a role that cannot be resolved being an error, never a role skipped
// (#47 review). Two files holding one token are one authority.
func operatorPrincipal(cfg *config.Config, newDriver principal.DriverBuilder) func(context.Context) (scm.Driver, error) {
	return func(ctx context.Context) (scm.Driver, error) {
		if cfg.Credentials.Operator.File == "" {
			return nil, errors.New("credentials.operator is not configured; close is an operator's act and needs the operator's own credential (a file the reviewer and producer identities cannot read)")
		}
		// One read: the driver that proved the identity is the driver that
		// closes. A second read of the file could be a different token.
		three, err := principal.Resolve(ctx, cfg, newDriver)
		if err != nil {
			return nil, err
		}
		if three.OperatorDriver == nil {
			return nil, errors.New("operator identity resolved to no driver")
		}
		return three.OperatorDriver, nil
	}
}

// scmVerb dispatches one verb against the role's driver. close alone uses
// the operator principal, obtained through operator(); a verb that could
// reach Driver.Close through the role's driver would be the reviewer
// closing. Separate from credential and driver construction so guards can
// be tested against fakes.
func scmVerb(ctx context.Context, d scm.Driver, operator func(context.Context) (scm.Driver, error), verb string, rest []string, out *printer) int {
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
		// Closing is not conditional at either provider: between the read
		// and the close another party can mark the change ready, or merge
		// it, and the close still lands or reports success. Submit avoids
		// that race by never closing; the automated reviewer protocol does
		// too. So close runs as the OPERATOR principal only -- a credential
		// the reviewer identity cannot read (#47 review) -- through a driver
		// the role's token never builds. The window is narrowed, not
		// removed: the change must be an open draft at the last read, and
		// afterwards it must be closed; merged is reported as what it is.
		if !need(1, "close <id> [reason]") {
			return exitError
		}
		if operator == nil {
			return out.fail(errors.New("close: no operator principal available"))
		}
		od, err := operator(ctx)
		if err != nil {
			return out.fail(fmt.Errorf("close: %w", err))
		}
		id := scm.ChangeID(rest[0])
		c, err := od.Get(ctx, id)
		if err != nil {
			return out.fail(err)
		}
		if c.State != scm.StateOpen {
			return out.fail(fmt.Errorf("change %s is %s, not open; nothing to close", id, c.State))
		}
		if !c.Draft {
			return out.fail(fmt.Errorf("change %s is ready, not a draft; it has left the producer's hands and is not a superseded draft to retire", id))
		}
		reason := "superseded by a newer submission"
		if len(rest) > 1 {
			reason = strings.Join(rest[1:], " ")
		}
		if err := od.Close(ctx, id, reason); err != nil {
			return out.fail(err)
		}
		// Verified, not believed: re-read, and only closed is success.
		after, err := od.Get(ctx, id)
		if err != nil {
			return out.fail(fmt.Errorf("close reported success but the change could not be re-read: %w", err))
		}
		switch after.State {
		case scm.StateClosed:
			return exitOK
		case scm.StateMerged:
			return out.fail(fmt.Errorf("change %s is MERGED after the close: another party merged it in the window the close cannot exclude; nothing was retired", id))
		default:
			return out.fail(fmt.Errorf("close reported success but change %s is %s", id, after.State))
		}

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
