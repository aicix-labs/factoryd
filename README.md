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

Early. Steps 1–3 of the [delivery sequence](SPEC.md#11-delivery-sequence) are
implemented and tested; the rest is not, and the binary says so rather than
pretending.

| | area | state |
|---|---|---|
| 1 | `Driver` interface, GitHub + GitLab drivers, shared conformance suite | **done** |
| 2 | Typed merge results, post-merge ancestry verification | **done** |
| 3 | Versioned state document, process handles, `doctor` | **done** |
| 4 | Supervisor (both roles, one implementation), spin/abort, progress | not started |
| 5 | `submit`: the gate, identity check, sandbox-aware materialization | not started |
| 6 | Health, alert transports, resource guards | not started |
| 7 | Status page | not started |

`factoryd supervise`, `submit`, `signal`, `audit` and `status` exit non-zero with
"not implemented in this build". A subcommand that silently did nothing would be
indistinguishable from one that ran and found nothing to do.

## What works today

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
ok    alert transports             file, command
ok    credential producer          file /etc/factoryd/widgets/producer.token
ok    identity producer            producer-bot (id 1001)
ok    credential reviewer          file /etc/factoryd/widgets/reviewer.token
ok    identity reviewer            factory-reviewer (id 2002)
ok    repository                   repository reachable
ok    distinct identities          producer producer-bot (id 1001), reviewer factory-reviewer (id 2002)

1 of 13 checks FAILED: producer workdir
```

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

**An audit that lists nothing tried is not a pass.** An adversarial pass with an
empty `attempts` list is rejected at the type boundary, before it reaches the
provider.

## Build

```console
$ go build ./...
$ go test -race -count=1 ./...
```

Go 1.25, no third-party dependencies.

`-count=1` is not decoration: a shared build cache replays cached test *results*,
so a green run without it can mean nothing executed.

## Repository layout

```
cmd/factoryd/          the single binary
internal/scm/          Driver interface, typed results, audit wire format
  conformance/         the one suite both drivers must pass, plus its control test
  github/ gitlab/      the two drivers and their recorded fixtures
  httpfixture/         strict recorded-exchange replay (unrecorded request = failure,
                       unused exchange = failure)
  httpjson/            the shared JSON-over-HTTP client
internal/config/       one JSON config per factory, decoded strictly
internal/state/        the single versioned state document, written atomically
internal/proc/         process references by handle and start token
internal/doctor/       "could this factory actually run?"
internal/factory/      config -> driver
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The short version: every check you add
must be shown to fail. Remove the behaviour, watch the test go red, put it back.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
