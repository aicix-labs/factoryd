# Acceptance (SPEC §12)

v2 is done when, for one real factory: an operator drops a brief; a sandboxed
producer with no network and no git writes produces a change; submit gates and
opens it; a reviewer of a different identity reviews, runs an adversarial pass on
an escalate-class path, and merges; the merge is verified by ancestry; **and every
one of the failures in §1 is either impossible by construction or caught by a
test that has been shown to fail.**

This document is the second half of that sentence, and the record of the first.

## §1 failures → evidence

"Shown to fail" means: a named mutation of the guarded code was applied, the
build and `go vet` still passed (a mutation that does not compile proves nothing
— §1 row 7), and the named test failed. The reviewer-relayed rounds on PRs #11–#18
record each mutation and the test that caught it.

| # | v1 failure | how v2 closes it | evidence shown to fail |
|---|---|---|---|
| 1 | `merge` printed *Branch cannot be merged* and returned exit 0 | **by construction**: `scm.MergeResult` can only be `Merged` through `MergeVerified`, whose `verified` field is unexported; a merge the repository does not confirm is `MergeUnknown`, never `Merged` | `TestMergeVerified`; conformance `merge_reported_but_absent`, `merge_unverified`, `merge_head_moved`; the gate's "merge unverified (driver Merge directly)" mutation |
| 2 | `pkill -f` killed the caller's own shell | **by construction**: nothing is ever identified by command line; `proc.Ref` holds pid + start token, signals go to the handle | `TestRefWithoutStartTokenIsRefused`, `TestRecycledPIDIsNotAlive` |
| 3 | false "duplicate supervisor" alarms from argv matching | **by construction**: same; the status page shows pids and factoryd's own labels, never argv or comm | `TestSelfIsAlive`, `TestExitedProcessIsNotAlive`, `TestNoProcessSuppliedLabelReachesAnyOutput` |
| 4 | a reviewer signal sat unseen for 2h20m (single-shot watcher) | both roles supervised; the watcher is internal and re-arms; consumed-then-failed turns leave a retry marker | `TestConsumedTriggerRunsOneTurnAndReArms`, `TestRetryMarkerWriteFailureHalts`, `TestRetryMarkerRemovalFailureHalts`, `internal/watch` suite |
| 5 | stall detector fired for hours into a journal nobody read | alert transports are required at load; `doctor` **delivers** a probe through each; a corrupt state document still alerts | `TestAlertProbeActuallyDelivers`, `TestFanoutIsIndependentAndNamesFailures`, `TestCadenceAlertsAfterNTicksRepeatsAndRecoversOnce`, `TestCorruptStateStillAlertsAndIsBounded` |
| 6 | producer stopped after every verdict | the producer supervisor owns continuity; SUBMIT is its after-turn step | `TestAfterTurnRunsOnceAfterACleanTurnOnly`, `TestAfterTurnFailureCountsOnTheFailStreak`, `TestProgressResetsTheFailStreak` |
| 7 | a syntax-breaking mutation "proved" a test | **by rule**: every mutation is vetted before its test run; one that does not vet is reported as such, never as caught | the mutation harness's `DOES NOT VET` outcome, hit and re-done in rounds on #16, #17, #18 |
| 8 | build cache grew to 99 GB; disk at 96% with no signal | disk headroom per volume; caches bounded, reclaimed only through an opened, verified root handle | `TestLowDiskIsDetectedOncePerVolume`, `TestCacheIsBoundedOldestFirstAndReported`, `TestDeletionFollowsTheHandleNotThePath`, `TestCacheRootReplacedBySymlinkAfterVerificationDeletesNothing` |
| 9 | handoff state across 8 ad-hoc files | one typed, versioned state document under a lock | `TestUnknownSchemaVersionIsRefused`, `internal/state` suite |
| 10 | producer could not commit under its sandbox | **by construction**: git never runs where the producer can write; submit copies the tree, excluding `.git` at any depth | `TestCopyTreeExcludesGitControlDataAtAnyDepth`, `TestCopyTreeRefusesASymlinkEscape`, `TestCopyTreeRefusesASymlinkIntoDotGit`, `TestBoundaryTransportAndGateFailuresAreCaught` |
| 11 | the producer's push went out under the reviewer's credential | constructed git environment, explicit credential file, identity oracle via `git credential fill` | `TestIdentityResolvesThroughCredentialFill`, `TestIdentityRefusesAForeignCredential`, `TestGuardRefusesAnInsteadOfRewrite`, `TestProducerEnvironmentCarriesNoReviewerCredential` |
| 12 | `doctor` healthy while git and API identities disagreed | both identities resolved and compared; undecided is not distinct; the gate re-decides on stable ids at merge time | `TestSharedIdentityIsCaught`, `TestIdentityIsUndecidedWhenTheProviderCannotSay`, `TestSelfSignalRefusedForEveryVerdict` |
| 13 | a missing cache directory reported as `gate FAILED` | a host that cannot run the gate is exit 3, never 5 | `TestUnprovisionableGatePathIsExit3NotARedGate`, `TestGateThatCannotRunIsExit3NotExit5`, `TestGatePathThatCannotBeCreatedIsExit3` |
| 14 | a real GitLab conflict reported as CI failure (hand-written fixture said 405) | fixtures are recorded live with provenance, or refused; the conflict scenario is the provider's own 422 | `TestLoadRequiresProvenance`, `TestFixtureProvenance`, conformance `merge_refused_unmergeable` on both providers |
| 15 | a factory idled 3.5h with work stranded and every signal green | a fail streak counted independently of trigger consumption; a durable retry marker | `TestStallAfterConsumedFailureIsRetriedThenHalted`, `TestConsecutiveConsumedFailuresHalt`, `TestSingleConsumedFailureIsRecordedNotHalted` |

## The run

Performed 2026-09-04 on the scratch factory `aicix-labs/factoryd-fixtures`, with
scripted stand-in turns (`examples/acceptance/`) under the two real supervisors,
the real sandbox, the real identities and the real gate. A second pass with agent
CLIs in the turn commands changes nothing below except who writes the code.

1. `factoryd doctor` — 43 checks, including `sandbox producer` (a listener doctor
   opened was **unreachable from inside** the producer's namespace and reachable
   without it) and `handoff inbox/outbox as producer`.
2. The operator dropped `inbox/brief.md`.
3. The producer supervisor ran the producer turn as `factoryd-producer` (uid 996)
   in a new network namespace; the turn proved `127.0.0.1:22` unreachable, wrote
   `auth/session.go` (an escalate-class path), declared intent, recorded progress
   and consumed the brief. The supervisor's after-turn step ran `submit`: gate,
   push as the producer, draft **#67** opened, `inbox/wake` written.
4. The reviewer supervisor ran the reviewer turn as a different identity
   (`aicix-labs`, provider id distinct from the producer's `aicix-lab`): found the
   draft, recorded an adversarial pass with three attempts on the escalate-class
   path (`audit post … session-fixation CLEARED`), marked the draft ready as its
   own act, and `signal merged` → the gate classified `auth/session.go` as
   escalate, found the reviewer's audit on the exact head, merged with the
   expected head, and **verified by ancestry**: `a2b82618` merged as `a52b1531`.
   `outbox/67.json` was written, the state document records the verdict, the
   comment was posted.
5. Independently of factoryd, GitHub's compare of the merge commit against `main`
   reports `identical`.
6. The verdict file woke the producer, which read it and consumed it.

### What the run itself found (fixed in the same change)

- The `list-open`/`get` parsing in the first reviewer stand-in was wrong — the
  turn failed, and the supervisor **preserved the wake trigger** for the retry,
  as §4.2 requires.
- The first producer turn did not consume its brief, so the producer ran again
  and opened a superseding draft. In the protocol the *turn* consumes its
  trigger; the stand-in now does.
- A root-owned inbox made the sandboxed producer's progress touch fail silently:
  every clean turn read as "no progress". A root-owned outbox left the verdict
  unconsumed. `doctor` now probes **both handoff directories as the producer**,
  because factoryd being able to write them says nothing (`TestIndividualFailuresAreCaught/producer_cannot_write_the_inbox`, `…outbox`).

### On the sandbox flag in the example

`examples/acceptance/factory.json` sets `roles.producer.sandbox.no_network`
because its producer is a script. A producer that is itself a hosted-model CLI
(codex, claude) must reach its own API and **must not** set it — see SPEC §4.4.

### The GitLab run, by the canary session (2026-09-04)

The session migrating the prod-next factory built a second factory against
`aicix/factoryd-fixtures` with production-style paths and ran the same cycle:
`doctor` 48 checks (sandbox proved from inside); brief → sandboxed producer turn
(uid 996, network unreachable) → gate green → pushed as the producer (id 28) →
draft !63 → reviewer turn as factoryd itself → `session-fixation CLEARED` on the
exact head → `signal merged` → merged as `1e6d6f64`, verified by ancestry; GitLab
independently reports `1e6d6f64` as `main`'s head, carrying the producer's exact
file, `merged_by` the reviewer (id 5), not the producer.

Two things this record did not cover and that run did: **restart continuity** —
the producer supervisor was stopped for the whole review; on restart it read and
consumed the waiting verdict, and replaced the dead supervisor record by pid and
start token, never argv; and **failing correctly** — the reviewer halted after
five consecutive failed turns with a loud sentinel and the wake trigger
preserved, and a restart was refused by the sentinel (§1 row 15, observed).

It also found three things fixtures could not: a real gate cannot walk `.git`
(#21: the example gate was `true`, which cannot fail this way — the shipped
gates are now real and prune `.git`); `FACTORYD_CONFIG` was needed by the
reviewer turn but injected by hand (#22: the runner injects it); and a reviewer
turn must match the branch **family by prefix**, because the pushed name is
`<declared>-<tree>`, never the declared name.

### The second pass, with a real model (canary session, 2026-09-04)

Codex ran as `factoryd-producer` — its own OS account (nologin, locked
password), its own `0700` home, its own `0600` `auth.json` obtained by device
auth, the CLI binaries copied root-owned to `/usr/local/lib/factoryd/codex/bin`
and nothing borrowed from the operator's home. `doctor`: 53 checks on prod-next,
still including *producer cannot read producer credential* and *… reviewer
credential*: the producer holds a model credential and no provider credential.
On the scratch repo: brief → Codex tightened `SessionValid` and added a
table-driven test, declared intent → immutable branch
`accept/session-format-9c1db05bba`, gate green, pushed as the producer (id 28),
draft !64 → reviewer (id 5) classified both files as escalate, recorded the
adversarial pass on the exact head, `signal merged` → `b7c7bfbc`, verified by
ancestry, GitLab's own head and `merged_by`. The code that landed is the
model's, not a fixture. Two corrections the run made to the stand-ins:
`no_network` is absent for a hosted-model producer, and the reviewer matches
the branch family by prefix.

### Operating notes from the runs

- **The producer's workdir is refreshed before a turn** (#35). The second
  production cycle found the clone one merge behind with cycle 1's script
  still untracked; the brief described the merged tree, the producer saw an
  older one, declared nothing five times, and halted — every signal right,
  the workdir simply not the thing the brief described. The supervisor now
  brings it to the target branch at the start of a cycle (SPEC §3, REFRESH:
  after a refresh, or after the cycle's own draft merged — decided by the
  write-ahead `cycle` record, never by absence), and `factoryd refresh` does
  it by hand. The helper runs as the producer under its sandbox and replaces
  `.git`, so no producer-supplied hook or filter runs. The clean is total:
  caches the model wants to keep belong under the cache root. Upgrading a
  live factory across this change: the old state loads as `cycle: unknown`
  and the first refresh is the operator's `factoryd refresh --force`.

- **The verdict now names the family to re-declare** (#29). A fix is never an
  in-place update: it is a new immutable branch that *supersedes* the old draft
  only when declared under the same family name. The first production cycle
  found the producer re-declaring a stale name from its seed clone and getting
  an unrelated draft beside the change under review. `outbox/<id>.json` carries
  `branch` and `declared_branch`, and a verdict turn is told
  every verdict it carries in `FACTORYD_VERDICTS` (the scalars only when there is
  exactly one); a wrapper that persisted `.producer-branch` before
  submit consumed it is no longer needed.

- **"A second pass with agent CLIs changes nothing except who writes the code"
  was not quite true** (#27): an agent CLI exits 0 whatever the model concluded,
  and a model cannot set an exit code by narrating it. Wire agent CLIs through
  `examples/turn-wrapper.sh` (or an equivalent), which derives the turn's exit
  code from whether `$FACTORYD_PROGRESS` moved; the wrapper is tested for each
  way it can be wrong (`TestTurnWrapperDerivesTheExitCodeFromProgress`).

- **Never run `git` as root in the producer's clone.** git refuses a
  repository owned by someone else ("dubious ownership"), and the honest
  reading of that refusal is that the repository is not yours: it is the
  producer's. Use `sudo -u factoryd-producer git -C <clone> …`, and do not add
  a `safe.directory` exception. The refusal is silent in effect — a reset that
  did nothing let a turn start on an unclean clone — so read git's output.
- The producer's home must be unreadable by the gate identity; `doctor` now
  proves it (*gate cannot read producer home*). Codex created `.codex` as
  `0775` beside a `0600` `auth.json`; the `0700` home is what keeps the gate
  out, and it is probed, not assumed.

### Not yet done

- Nothing on factoryd's side. The prod-next cutover (service units, stopping
  the legacy factory) is the canary session's.
