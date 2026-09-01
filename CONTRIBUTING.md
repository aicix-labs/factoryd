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
4. Record a fixture bundle for **both** providers under
   `internal/scm/{github,gitlab}/testdata/<scenario>.json`.
5. Implement it in both drivers.

The fixture player is strict in both directions: a request the bundle does not
contain fails the test, and an exchange the driver never made also fails it. A
permissive fixture player is a check that cannot fail.

## Merge results

`Driver.Merge` returns `ProviderMerge` — what the provider said. Only
`MergeVerified`, having confirmed the commit is an ancestor of the target
branch, produces a `MergeResult` with `Merged`. The `verified` field is
unexported, so a `Merged` result nothing confirmed cannot be constructed outside
`internal/scm/merge.go`, and `Validate` rejects one if you try.

Do not add a way to build a verified merge from a driver. A provider cannot
attest to what landed on a branch; that is the whole point of the split.

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
