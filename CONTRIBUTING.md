# Contributing

## The one rule

**A check that cannot fail is not a check.**

Before you claim any green signal — a passing test, a healthy `doctor`, an empty
result set — state what would have to break for it to go red. If you cannot
answer, you have a decoration.

This is not a slogan. It is reviewed.

## Evidence rules

These are normative. They come from [SPEC.md §9](SPEC.md#9-evidence-rules-normative),
and each one has a real failure behind it.

1. **Sensitivity-check every check.** A test that passes identically with and
   without the behaviour is inert. Remove the behaviour, confirm the test fails,
   put it back. Say in the PR that you did.

2. **Sensitivity-check the delta, not the feature.** A test that also passes
   against the pre-change code tells you the feature works, not that your change
   does. Mutate the lines you actually changed.

3. **Assert a mutation landed before believing its result.** The text changed
   *and* the file still compiles. A red produced by a syntax error proves
   nothing. Four "mutation caught" results in the predecessor were build errors.

4. **A zero needs a positive control.** Every zero has two explanations: nothing
   was wrong, or nothing was measured. Pair it with a control that must be
   non-zero. `TestCleanDriversPass` exists for exactly this reason — without it,
   the mutant tests' reds could just mean the harness is broken.

5. **Ask what silence means.** If a component going away produces no output, its
   silence is indistinguishable from success. This is why a commit with no
   pipeline is `PipelineNone` and not green, and why unimplemented subcommands
   exit non-zero instead of doing nothing.

6. **Match command position, not the mention.** Guards that grep for a string
   fire on comments, on string literals, and on their own fixtures. Parse, or
   anchor.

## Adding a driver verb

Both drivers must implement it, and the conformance suite must exercise it. The
suite checks its own verb coverage: adding a method to `scm.Driver` without a
scenario that calls it fails `_verb_coverage`. This is deliberate — in v1, five
verbs existed only in the GitHub driver and the producer was silently
GitHub-only for weeks.

Neither side of that check is maintained by hand. The method list comes from
reflection over `scm.Driver`; the exercised list comes from a recorder that
observes what each scenario actually called. A verb you forget to cover is
caught whether or not you remember this file exists.

1. Add the method to `scm.Driver`.
2. Forward it in `internal/scm/conformance/recorder.go`. The recorder implements
   `scm.Driver`, so until you do, the package does not compile.
3. Add a scenario to `internal/scm/conformance/conformance.go` that calls it.
4. Implement it in both drivers.
5. Record a fixture for **both** providers with `cmd/fixturerec` (see below).

The fixture player is strict in both directions: a request the bundle does not
contain fails the test, and an exchange the driver never made also fails it. A
permissive fixture player is a check that cannot fail.

## Fixtures are recorded, not written

Every fixture declares where it came from, and `Load` refuses one that does not.
Write them by hand only when a case genuinely cannot be produced on a live
provider, and say why in the `source.note` — "hand-written" has to be a decision,
not an omission.

A fixture written from API documentation describes what the API was *believed*
to do. A driver that matches it exactly passes every test above it while being
wrong about the provider, and nothing looks broken. That is a check that can
fail against the wrong reality, which is harder to notice than one that cannot
fail at all. It is not hypothetical: the hand-written GitLab fixture said an
unmergeable change answers 405, so the driver mapped a real 422 conflict to
`RefusedPipeline` — the wrong refusal, with the suite green.

```console
$ go run ./cmd/fixturerec --provider gitlab     --base https://gitlab.example.com/api/v4 --project grp/scratch     --token-file ~/.config/token --out internal/scm/gitlab/testdata
```

It needs a scratch repository it may push branches to and open, merge and close
changes in. It resets that repository to a known state before every scenario —
completely, not create-if-missing, because a scenario that merges lands its
changes on the default branch and the next recording would otherwise describe
state it did not create.

**Record the error shapes, not just the happy paths.** 404, 403, merge conflicts,
draft refusals. That is where the drivers make their fail-closed decisions, and
where a hand-authored body is most likely to be wrong.

Redaction rewrites unmapped identifiers too, deterministically. A redactor that
only rewrites what it was told about leaks whatever it was not told about, and
the fixture still looks fine. `Write` refuses to emit a fixture containing a
declared secret.

## Merge results

`Driver.Merge` returns `ProviderMerge` — what the provider said. Only
`MergeVerified`, having confirmed the commit is an ancestor of the target
branch, produces a `MergeResult` with `Merged`. The `verified` field is
unexported, so a `Merged` result nothing confirmed cannot be constructed outside
`internal/scm/merge.go`, and `Validate` rejects one if you try.

Do not add a way to build a verified merge from a driver. A provider cannot
attest to what landed on a branch; that is the whole point of the split.

## The supervisor

Agent turns are one-shot. They read their trigger, do one unit of work, and
exit. Nothing in an agent loops, polls, or waits — the supervisor owns all
continuity. In v1 each agent was told to "run forever" and reliably did not.

Two rules are easy to break by accident:

- **Do not consume a trigger anywhere but in the agent turn.** The spin guard is
  built on being able to see that a turn did not consume its trigger. A watcher
  or supervisor that tidied it away would blind the guard.
- **Reset the spin counter on progress, not on consumption.** A large task
  legitimately spans turns. A guard that cannot tell "still working" from
  "achieving nothing" halts real work, which is what the first implementation
  of this rule did.
- **Progress means the marker *moved*, not that it exists.** The progress file
  is durable: once a turn touches it, it is there forever. A guard reading
  existence sees progress on every subsequent turn, resets the counter every
  time, and never fires again — so an agent that advanced once and then
  crash-loops is relaunched indefinitely. That is the exact failure
  `spin_abort` exists to prevent, and the existence version compiles, reads
  naturally, and is easier to write. `TestProgressOnceThenStallStillHalts` and
  `TestStaleProgressFileIsNotProgress` are what stand between the two.

If you add a loop, add a cancellation check at the top of it. A supervisor whose
trigger is always pending never blocks in `Wait`, so a check only inside `Wait`
leaves a busy supervisor that cannot be stopped — and a process you cannot stop
is one you end up killing by command-line pattern, which is where this project
started.

## Adding a mutant

If you add behaviour the conformance suite is supposed to police, add a mutant to
`internal/scm/conformance/control_test.go` that breaks it, and assert exactly
which scenarios go red. If no scenario catches your mutant, the suite does not
actually cover the behaviour you thought it did.

## Style

- Go 1.25, standard library only. A new dependency needs a reason in the PR.
- `gofmt`, `go vet`, and `go test -race -count=1 ./...` all clean.
- Comments explain *why*, especially why a check is shaped the way it is. The
  failure a check exists to catch is the most useful thing you can write down.
- One purpose per PR. The factory itself is built on single-purpose changes.
