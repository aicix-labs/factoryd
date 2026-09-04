# factoryd

A config-driven toolkit for running autonomous code-review factories: a
**producer** agent opens single-purpose draft PRs/MRs, and a separate
**reviewer** agent — different credential, different identity — reviews, merges,
or blocks.

Successor to [`aicix-labs/ai-agent-factory`](https://github.com/aicix-labs/ai-agent-factory)
(~3,000 lines of bash, and it works). The protocol is preserved almost unchanged.
What is being replaced is the substrate: shell has no result type, no process
handles, and no schema, and each of those cost a real outage. The full
specification, including the ten production failures that motivate it, is in
[SPEC.md](SPEC.md).

## The rule the project is built on

> **A check that cannot fail is not a check.** Before trusting any green signal,
> state what would have to break for it to go red. If you cannot, it is
> decoration.

That rule is enforced on this repository, not just described by it. The
conformance suite has its own control test that asserts the suite *rejects*
seven deliberately broken drivers, and names which scenario catches each one —
because a suite nothing has ever been shown to reject proves nothing. Its
verb-coverage guard derives both sides of its comparison (reflection over the
interface, observation of what scenarios called), so it cannot pass by virtue of
a list nobody updated.

## Status

Early. Steps 1–4 of the [delivery sequence](SPEC.md#11-delivery-sequence) are
implemented and tested; the rest is not, and the binary says so rather than
pretending.

| | area | state |
|---|---|---|
| 1 | `Driver` interface, GitHub + GitLab drivers, shared conformance suite | **done** |
| 2 | Typed merge results, post-merge ancestry verification | **done** |
| 3 | Versioned state document, process handles, `doctor` | **done** |
| 4 | Supervisor (both roles, one implementation), spin/abort, progress | **done** |
| 5a | Git transport (https), the owned environment, the two-directory boundary, `doctor`'"'"'s probe | **done** |
| 5b | `submit`: materialize, gate, open the Draft PR/MR, signal | **done** |
| 6 | Health tick, alert transports (`file`, `command`), resource guards, bounded caches | **done** |
| 7 | Status page: `factoryd status`, HTML + JSON, read-only | **done** |
| §12 | Acceptance: one real factory end-to-end under the real supervisors, sandbox, identities and gate — see [ACCEPTANCE.md](ACCEPTANCE.md) | **done** with scripted turns; real agent CLIs next |

Every verb in the SPEC's surface is built. `signal merged` is the merge gate:
the `scope` policy in the config (deny / allow / hold-diff / escalate regexes,
preserved from v1 as data) decides what may merge, what needs a recorded
adversarial audit on the exact head, and what a human must approve — and a
gate result never substitutes a verdict the reviewer did not choose. Two-party
trust is decided there too, on the provider's stable ids: the reviewer is never
the author, an audit counts only when the provider says the reviewer posted it,
and the gate never marks a draft ready.

## What works today

The producer's workdir is refreshed to the target branch before a turn when no
change is in flight, by a helper running as the producer from a bundle factoryd
wrote (`factoryd refresh --config <f>` by hand, `--force` under a change in
flight). Nothing else brings a credential-less, network-less producer forward.

```console
$ factoryd doctor --config examples/factory.github.json
ok    config                       widgets, provider github, target branch main
ok    paths.root                   /var/lib/factoryd/widgets
ok    handoff inbox                /var/lib/factoryd/widgets/inbox
ok    handoff outbox               /var/lib/factoryd/widgets/outbox
FAIL  producer workdir             /var/lib/factoryd/widgets/clone (worktree)
      ... is a git worktree, not a clone: .git is a file pointing at
      /elsewhere/.git/worktrees/w. Its refs live in the parent repository,
      outside the producer's sandbox, so the producer cannot commit
ok    gate command                 bash -c go build ./... && go vet ./... && ...
ok    alert file                   file /var/log/factoryd/widgets-alerts.log: probe alert delivered
ok    alert command                command /usr/local/bin/notify-operator: probe alert delivered
ok    health thresholds            alert after 3 ticks, repeat every 1800s; stale trigger 900s, turn grace 120s, disk headroom 10%, 1 bounded cache(s)
ok    credential producer          file /etc/factoryd/widgets/producer.token
ok    identity producer            producer-bot (id 1001)
ok    credential reviewer          file /etc/factoryd/widgets/reviewer.token
ok    identity reviewer            factory-reviewer (id 2002)
ok    repository                   repository reachable
ok    distinct identities          producer producer-bot (id 1001), reviewer factory-reviewer (id 2002)

1 of 13 checks FAILED: producer workdir
```

the model-free health tick — one observation, or `--loop`. It detects, alerts
through every configured transport (independently: one failing does not
suppress the next), bounds the caches it was told to bound, and writes
`<root>/health.json`. It never merges, reviews, or re-arms anything:

```console
$ factoryd health --config f.json
2026-09-03T14:02:11Z UNHEALTHY: 2 finding(s)
  disk_low//var/lib/factoryd/widgets   volume of /var/lib/factoryd/widgets has 4.1% free (9.3 GiB of 232.0 GiB), below the 10% headroom
  stale_trigger/reviewer               reviewer trigger inbox/wake has waited 1h12m0s unconsumed
  volume /var/lib/factoryd/widgets     4.1% free (9.3 GiB of 232.0 GiB)
  cache  /var/cache/factoryd/widgets/go 4.8 GiB of 5.0 GiB; reclaimed 3 entries, 1.2 GiB
  alerted  disk_low//var/lib/factoryd/widgets
$ echo $?
1
```

Exit 0 is healthy, 1 is findings, 3 is "could not look" — a tick that cannot
stat a volume, reach the provider, or read the state document is a different
failure from an unhealthy factory and says so. Reclamation deletes only inside
`paths.cache_root`, a dedicated directory that may not overlap anything the
factory depends on; the check is lexical at load, and at the moment of deletion every removal is
bound to an opened, verified directory handle — never to a path, which anyone
with write access to an ancestor can retarget between a check and a delete. A corrupt state document does not silence
the tick: a fail-safe alert goes out through the transports regardless. `doctor` delivers
a probe alert through every transport rather than inspecting them, because
"the path looks writable" is not "the alert landed".

the status page — is it working, what is it doing right now, what is it waiting
on, what needs me — as one-shot text, JSON, or a served page with a JSON
endpoint. Read-only by contract and by test: nothing under the factory root
changes when it is read, and the provider is asked once per health interval,
not per reload. The page is unauthenticated, so the process tree shows
pids and structure only, labelled from what factoryd itself recorded — never
argv, `comm` or an executable path, all of which a process controls — and
`--serve` warns when the address it actually bound is not loopback:

```console
$ factoryd status --config f.json
widgets  NOT WORKING  (github -> main)  at 2026-09-04T14:02:11Z
needs you:
  ! reviewer supervisor pid 4120 (start token 3ab1) is dead and did not halt
  ! change 61 is operator-gated: touches deploy/ (escalate-class path)
producer  supervisor pid 4118 alive (inotify); idle, last turn producer-20260904T135501-3 exit 0 after 7m12s; waiting on verdict(7m10s)
reviewer  supervisor DEAD; running reviewer-20260904T140104-9 (wake) for 1m07s
health    healthy, 41s ago
  volume /var/lib/factoryd/widgets      71.2% free
changes   1 open as of 2026-09-04T14:01:40Z
  61     draft producer/fix-3f9a1c2b7d -> main  gate: match command position
verdict   operator-gated on 61 at 2026-09-04T13:58:02Z: touches deploy/ (escalate-class path)
$ factoryd status --config f.json --config g.json --serve 127.0.0.1:8080
```

the reviewer's verdict, where `merged` is the merge gate:

```console
$ factoryd signal --config f.json 61 merged auto --summary "cleared: authz bypass attempts recorded"
factoryd signal: scope policy requires an adversarial audit of 61 at 3f9a1c2b7d: no CLEARED audit is recorded on this head
  internal/auth/session.go matches escalate "(^|/)(auth|authn|authz|login|session|oauth|jwt|token)"
$ echo $?
5
$ factoryd audit --config f.json post 61 3f9a1c2b7d --lens authz-bypass --verdict CLEARED --file attempts.json
audit authz-bypass CLEARED posted on 61 at 3f9a1c2b7d by factory-reviewer (3 attempts)
$ factoryd scm --config f.json set-draft 61 false      # the reviewer's own act; the gate never does it
$ factoryd signal --config f.json 61 merged auto --summary "cleared: authz bypass attempts recorded"
merged 61 at 3f9a1c2b7d merged as 8c2e41d0aa (verified); wrote /var/lib/factoryd/widgets/outbox/61.json
```

a supervised role loop, where the agent turn is any command you configure. The
producer's turn can be sandboxed by the supervisor itself
(`roles.producer.sandbox.no_network`: a new network namespace, created as root
before the identity switch, proved by `doctor` from inside — for scripted turns
or tool-sandboxed agents, **not** for a producer that is itself a hosted-model
CLI, which must reach its API), and `submit` is the
producer supervisor's after-turn step — the turn declares intent in files and
exits; the supervisor does the git and network work outside the sandbox:

```console
$ factoryd supervise --config f.json --role reviewer
INFO msg="watcher armed" role=reviewer mode=inotify
INFO msg="turn starting"  turn=reviewer-20260901T194139-1 triggers=wake
INFO msg="turn finished"  turn=reviewer-20260901T194139-1 exit=0 consumed=false progressed=true spin=0
INFO msg="turn starting"  turn=reviewer-20260901T194139-2 triggers=wake
INFO msg="turn finished"  turn=reviewer-20260901T194139-2 exit=0 consumed=true  progressed=false spin=0
```

That first turn left its trigger in place and still did not count against the
spin guard, because it touched its progress file. A turn that leaves the trigger
*and* reports nothing does count, and at `spin_abort` the supervisor stops:

```console
WARN  msg="turn achieved nothing" spin=3 warn_at=2 abort_at=4
ERROR msg="supervisor halting" reason="4 consecutive turns consumed no trigger
      and reported no progress (spin_abort=4); last trigger wake, last exit 1"
      sentinel=/var/lib/factoryd/widgets/reviewer.stop triggers_preserved=wake
$ echo $?
3
```

The trigger is deliberately left on disk: it is the only evidence a signal ever
arrived. The sentinel is removed by an operator and by nobody else — a circuit
breaker that resets itself is a slower version of the loop it was meant to stop.

and direct, scriptable access to the provider:

```console
$ factoryd scm --config f.json list-open
42	producer/fix-thing -> main	gate: match command position, not the mention

$ factoryd scm --config f.json pipeline 42
none (0 pipelines for aaaa...) green=false

$ factoryd scm --config f.json merge 42 aaaa...
refused-conflict: gitlab refused the merge: Branch cannot be merged
$ echo $?
4
```

That last exit code is the whole project in miniature. In v1 the same command
printed the same message and exited **0**, and the caller read `$?` as success.

## Design notes

**Three verdicts, no more.** `merged`, `changes-requested`, `operator-gated`.
The third is not a defect signal — it means the reviewer verified provenance and
is escalating the merge decision to a human, e.g. for a high-blast-radius path.

**Two-party trust.** The producer never merges, marks ready, or resolves reviewer
threads. The reviewer never authors code. Distinct credentials, distinct provider
identities — `doctor` fails closed if the two resolve to the same account, and
credential resolution never falls back from one role to the other.

**The zero value is never success.** `MergeOutcome(0)` is `MergeUnknown`.
`PipelineState(0)` is `PipelineUnknown`, and a commit with *no* pipeline is
`PipelineNone`, which is not green. `AuditVerdict(0)` is `AuditUnknown`, which
never clears a change. A caller who forgets to inspect an outcome gets an
unusable value, not a false green.

**The API is not the authority on what landed.** `Driver.Merge` returns
`ProviderMerge` — what the provider *said*. Only `MergeVerified`, having
confirmed the commit is an ancestor of the target branch, can produce a `Merged`
`MergeResult`; the `verified` field is unexported, so an unconfirmed merge is not
a bug to test for, it does not typecheck. A provider that reports a merge which
is not on the branch yields `MergeUnknown` — not `Merged`, and not a refusal,
because what happened is genuinely not known and a human has to look. The sha it
claimed is kept in `ClaimedCommit`, since that is what has to be checked by hand.

**Processes are held by handle, never matched by pattern.** `pkill -f
"supervisor.sh"` matched the invoking shell (killing the operator's session,
twice) and matched child subshells that share argv (two false "duplicate
supervisor" alarms). A PID alone is not an identity either, so every reference
carries the kernel's process start token; a recycled PID reports dead.

**Progress, not consumption, resets the spin guard.** A supervisor that halted
whenever a turn left its trigger behind would halt every task too large for one
turn. So an agent touches a progress file to say "I advanced", and only turns
that consume nothing *and* report nothing count toward the circuit breaker. This
distinction was a bug in the first implementation of the rule and is now the
thing its tests are mostly about.

**The watcher cannot be lost.** In v1 it was a separate single-shot script; it
exited after firing, nothing re-armed it, and a reviewer signal then sat unseen
for two hours and twenty minutes. Here it is a value the supervisor owns for its
whole life. It is event-driven via inotify with a poll fallback, it never
consumes a trigger, and it returns immediately for a trigger that was already
there — a watcher that only reported *new* events would reproduce the same gap.
When it falls back to polling it says so at boot and records it in state, because
silent degradation is indistinguishable from working.

**One state document, one writer at a time.** Both roles'"'"' supervisors write the
same `state.json`, and `Update` is a read-modify-write. Without a lock the later
writer silently discards the other'"'"'s changes — measured at 165 of 200 updates
lost under contention — including a halt reason, which is the one field nobody
can afford to lose.

**Fixtures are recorded from live providers, not written from documentation.**
A hand-written fixture describes what the API was believed to do, and a driver
matching it exactly passes every test above it while being wrong about the
provider — a check that can fail against the wrong reality. Recording found one
immediately: GitLab answers a real conflict with 422 `Branch cannot be merged`,
not the 405 the hand-written fixture claimed, so a conflict was being reported as
a CI failure. Every fixture states its provenance, and the few that stay
synthetic say why.

**Git never runs in a repository the producer can write.** There are two
directories: the producer edits `producer_workdir`, whose `.git` is ignored
entirely; every git command runs in `submit_repo`, which factoryd owns. Submit
copies the source tree across — excluding any `.git` at any depth, refusing
symlinks that escape — so nothing the producer wrote reaches the configuration
git reads. This replaced a design that validated the producer'"'"'s `.git/config`
before each push, which was a check-then-exec race: the producer could replace
the file between the check and git reading it. The boundary is **probed, not
asserted**: `doctor` runs a write attempt *as the producer'"'"'s OS user*, requires
it to fail against `submit_repo`, and requires the same probe to succeed against
the producer'"'"'s workdir — otherwise "cannot write" is just a probe that cannot
write anything.

**The transport environment is constructed, not filtered.** Every git process
starts from an environment factoryd builds — `PATH` and `HOME` from config,
global and system git config at `/dev/null`, no interactive fallback — and
nothing else. "Drop inherited `GIT_*`" was a denylist wearing an environment'"'"'s
clothes: `https_proxy` and `SSL_CERT_FILE` steer the transport and neither
starts with `GIT_`. A proxy or trust store reaches git only through typed
config. The repository'"'"'s own `.git/config` is **default-deny**: twelve keys
that make a clone a clone, and anything else — `http.proxy`, `insteadOf`, a key
Git adds next year — refuses the operation. Recording the real GitLab answer
had already shown why hand-written expectations are dangerous; this is the same
lesson applied to the transport'"'"'s inputs.

**Git'"'"'s identity is asked of git, not inferred from config.** `doctor` runs
`git credential fill` under the exact isolation the push will use, then asks
the provider API whose token that is, and requires the answer to match the
producer'"'"'s API identity. In production the two mechanisms disagreed — git
pushed as the reviewer while every API check passed — and it failed closed only
because that credential happened to lack write scope.

**An audit that lists nothing tried is not a pass.** An adversarial pass with an
empty `attempts` list is rejected at the type boundary, before it reaches the
provider.

## Build

```console
$ go build ./...
$ go test -race -count=1 ./...
```

Go 1.25, no third-party dependencies — inotify comes from the standard
library'"'"'s `syscall` package.

`-count=1` is not decoration: a shared build cache replays cached test *results*,
so a green run without it can mean nothing executed.

## Repository layout

```
cmd/factoryd/          the single binary
internal/scm/          Driver interface, typed results, audit wire format
  conformance/         the one suite both drivers must pass, plus its control test
  github/ gitlab/      the two drivers and their recorded fixtures
  httpfixture/         strict recorded-exchange replay (unrecorded request = failure,
                       unused exchange = failure), plus the recorder and its
                       redaction
cmd/fixturerec/        records fixtures from a live provider (dev tooling)
  httpjson/            the shared JSON-over-HTTP client
internal/supervise/    one role loop, parameterised by role: block, run one
                       turn, re-arm; the spin and fail guards, the halt sentinel
internal/gittransport/ git under an owned environment and identity: the
                       allowlist, the effective-URL guard, the tree copy
internal/watch/        inotify with a poll fallback; never consumes a trigger
internal/config/       one JSON config per factory, decoded strictly
internal/state/        the single versioned state document, written atomically
                       under an exclusive lock
internal/proc/         process references by handle and start token
internal/doctor/       "could this factory actually run?"
internal/factory/      config -> driver
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The short version: every check you add
must be shown to fail. Remove the behaviour, watch the test go red, put it back.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
