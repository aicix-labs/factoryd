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

### Not yet done

- The second pass with real agent CLIs in `roles.*.command` (the operator names
  the CLI and the account per role).
- GitLab: the same run on `aicix/factoryd-fixtures`.
