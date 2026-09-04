// Command factoryd is the whole factory in one binary.
//
// One binary, all subcommands, deliberately: this project's predecessor
// repeatedly lost components across host migrations -- a producer daemon, a
// build-cache volume, a prune job -- and each absence was discovered only by
// its consequences. A binary either exists or it does not.
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

// Exit codes are part of the CLI contract.
const (
	exitOK      = 0
	exitError   = 1
	exitConfig  = 3 // configuration or identity failure
	exitNothing = 4 // nothing to do
	exitGate    = 5 // the gate rejected the change
)

// version is set at build time: -ldflags "-X main.version=..."
var version = "dev"

var usage = `factoryd - config-driven code review factories

usage:
  factoryd doctor    --config <f>                    verify a factory could actually run
  factoryd supervise --config <f> --role <r>         run one role's loop
  factoryd submit    --config <f>                    gate and open the producer's declared change
  factoryd refresh   --config <f> [--force]          bring the producer's workdir to the target branch
                                                     (refused while a change is in flight; the
                                                     producer supervisor does this before a turn)
                                                     exits 0 submitted, 3 config/identity, 4 nothing, 5 gate red
  factoryd health    --config <f> [--loop] [--json]  one model-free tick: detect, alert, write health.json
                                                     exits 0 healthy, 1 findings, 3 could not look
  factoryd status    --config <f> [--config <g>] [--serve :8080] [--json] [--provider=false]
                                                     read-only: is it working, what is it doing, what is it waiting on, what needs me
  factoryd signal    --config <f> <id> <verdict> <sha|auto> --summary <s>
                                                     record the reviewer's verdict; "merged" IS the merge gate
                                                     exits 0 done, 3 config, 5 refused (scope/audit/head/provider), 6 merge unknown
  factoryd audit     --config <f> post <id> <sha> --lens <l> --verdict CLEARED|BROKEN --file <f>
                                                     record an adversarial pass on the head; no attempts is not a pass
  factoryd scm       --config <f> <verb>...         drive the provider directly
  factoryd version

verbs for scm:
  whoami                          who the credential authenticates as
  list-open                       open changes
  get <id>                        one change
  diff <id>                       changed files
  pipeline <id>                   CI status of the change's head
  audits <id>                     recorded adversarial passes over the head
  is-ancestor <sha> <ref>         is sha reachable from ref
  comment <id> <body>             post a comment
  set-draft <id> true|false       mark draft or ready
  merge <id> <expected-head>      merge, then verify the commit landed
  close <id> [reason]             retire a superseded open draft as the OPERATOR principal
                                  (credentials.operator, unreadable by the roles), verified closed

flags for supervise:
  --role <r>       producer or reviewer (required)
  --max-turns <n>  stop after n turns; 0 runs until stopped

global flags:
  --config <f>     factory config (required except for version)
  --role <r>       producer or reviewer; selects the credential (default reviewer)
  --json           machine-readable output where supported
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitError)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "doctor":
		os.Exit(runDoctor(args))
	case "supervise":
		os.Exit(runSupervise(args))
	case "refresh":
		os.Exit(runRefresh(args))
	case "_refresh":
		os.Exit(runRefreshHelper(args))
	case "submit":
		os.Exit(runSubmit(args))
	case "health":
		os.Exit(runHealth(args))
	case "status":
		os.Exit(runStatus(args))
	case "_netprobe":
		// Internal: doctor's sandbox probe. Exit 0 if addr is reachable.
		if len(args) != 1 {
			os.Exit(exitError)
		}
		c, err := net.DialTimeout("tcp", args[0], 3*time.Second)
		if err != nil {
			os.Exit(1)
		}
		c.Close()
		os.Exit(exitOK)
	case "signal":
		os.Exit(runSignal(args))
	case "audit":
		os.Exit(runAudit(args))
	case "scm":
		os.Exit(runSCM(args))
	case "version":
		fmt.Println(version)
		os.Exit(exitOK)
	case "-h", "--help", "help":
		fmt.Print(usage)
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "factoryd: unknown command %q\n\n%s", cmd, usage)
		os.Exit(exitError)
	}
}
