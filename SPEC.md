# ai-agent-factory-v2 — specification

**Status:** draft for implementation · **Language:** Go · **Successor to:** [`aicix-labs/ai-agent-factory`](https://github.com/aicix-labs/ai-agent-factory)

A config-driven toolkit for running autonomous code-review factories: a **producer**
agent opens single-purpose Draft PRs/MRs, and a separate **reviewer** agent reviews,
merges, or blocks. v1 is ~3,000 lines of bash and works. v2 is a Go rewrite of its
core, motivated by specific failures observed in production operation — listed below
with evidence, so an implementer can tell which requirements are load-bearing.

---

## 1. Why v2 exists

v1's *protocol* is sound and should be preserved almost unchanged. What fails is its
*implementation substrate*. Every item below was observed in a single day of real
operation, not theorised:

| # | Failure | Root cause | v2 requirement |
|---|---|---|---|
| 1 | `merge` printed `Branch cannot be merged` and returned **exit 0** | shell has no result type; the caller inferred success from `$?` | §5.1 typed results; a merge that did not merge is unrepresentable |
| 2 | `pkill -f "supervisor.sh"` killed the caller's own shell — twice | pattern matched the invoking command line | §5.2 process handles, never pattern-matching |
| 3 | Two false "duplicate supervisor" alarms | a child subshell shares argv with its parent | §5.2 supervise by PID/handle, expose a real process tree |
| 4 | A reviewer signal sat unseen for 2h20m | `watch.sh` is single-shot; nothing re-armed it | §4.3 both roles supervised, watcher self-heals |
| 5 | The stall detector had fired every 15 min for hours into a journal nobody read | detection existed; **no alert destination** | §7 alerts have a transport, and absence of one is itself an error |
| 6 | Producer stopped after every verdict, needing a human to re-prompt | no relaunch loop on the producer side | §4.2 supervisor owns the loop; agent turns are one-shot |
| 7 | A mutation that broke syntax produced a red that "proved" a test worked | no landed-and-compiles check | §9 evidence rules |
| 8 | CI build cache grew to 99 GB unbounded; disk hit 96% with no signal | no GC policy, no disk in the health signal | §7 resource guards |
| 9 | Handoff state spread across 8 ad-hoc files | no schema | §6 one typed, versioned state document |
| 10 | Producer could not commit at all under its sandbox | `.git` is read-only in `workspace-write` | §4.4 sandbox-aware submit |

### Found since, and load-bearing for the same reason

Later observations, from continued operation of v1 and from building v2. Kept
separate because the table above is one day’s evidence and should stay that way,
but these carry the same weight.

| # | Failure | Root cause | v2 requirement |
|---|---|---|---|
| 11 | The producer’s `git push` went out under the **reviewer’s** credential | `fetch`/`push` ignore the API token and consult the ambient credential helper; the first matching entry won | §4.4 the producer credential governs the git transport |
| 12 | `doctor` reported healthy while the git and API identities disagreed | only the API identity was ever resolved | §4.1 verify the git transport identity |
| 13 | A missing cache directory was reported as `gate FAILED`, exit 5 | a build that cannot write and a build that found a defect both exit non-zero | §9.6 environmental failure is not a red gate |
| 14 | A real GitLab merge conflict was reported as a CI failure | the fixture was hand-written from documentation and said 405; the provider answers 422 | §9.7 fixtures are recorded, not written |

Item 11 failed closed only because the reviewer credential happened to lack
repository write scope. With it, the push would have succeeded as the wrong
identity and nothing would have said so.

**Design rule derived from all of them:** *a check that cannot fail is not a check.* Before
trusting any green signal, state what would have to break for it to go red. If you
cannot, it is decoration. This rule is normative throughout this spec.

---

## 2. What v1 got right — preserve these

Do not redesign these. They are the value.

- **Three verdicts, no more:** `merged`, `changes-requested`, `operator-gated`.
  The third is not a defect signal — it means *the reviewer verified provenance and
  is escalating the merge decision to a human*, e.g. for a high-blast-radius path.
- **Two-party trust.** The producer never merges, marks ready, or resolves reviewer
  threads. The reviewer never authors code. Distinct credentials per role. This is
  the property that catches defects; everything else is convenience.
- **Config-driven factories.** One config file per factory; the same binary runs many.
- **Provider abstraction.** One command contract; GitHub and GitLab behind drivers.
- **Event-driven, not polling.** Agents are woken; they do not spin.
- **Scope policy as data.** Deny/allow/escalate path regexes decide what may merge
  automatically, what needs an adversarial pass, and what a human must approve.
- **The playbook.** The reviewer's routine lives in a document, not in code, so it can
  be revised without a release.

---

## 3. Roles and the loop

```
operator ──brief──▶ ┌───────────┐
                    │ PRODUCER  │ one turn: read trigger → edit → test → declare intent → exit
                    └─────┬─────┘
                          │ (never runs git; never pushes)
                    ┌─────▼─────┐
                    │  SUBMIT   │ model-free: branch, commit, GATE, push, open Draft, signal
                    └─────┬─────┘
                          │
                    ┌─────▼─────┐
                    │ REVIEWER  │ one turn: scope → CI → adversarial pass if escalate → verdict
                    └─────┬─────┘
                          │
              merged / changes-requested / operator-gated
                          │
                          └──▶ back to PRODUCER (supervisor relaunches)
```

Both agent roles are **one-shot turns**. Neither polls, waits, or loops. The
supervisors own all continuity. This is the single most important change from v1,
where each agent was told to "run forever" and reliably did not.

---

## 4. Components

### 4.1 `factoryd` — the single binary

One static binary, all subcommands. Rationale: this project's predecessor repeatedly
lost components during host migrations — a producer daemon, a build-cache volume, and
a prune job all failed to survive, each discovered only by its absence. A binary
either exists or it does not.

```
factoryd supervise --role producer|reviewer --config <f>
factoryd status [--json] [--serve :8080]
factoryd submit --config <f> --workdir <d>
factoryd signal <id> <verdict> <sha|auto> --summary <s>
factoryd audit post <id> <sha> --lens <l> --verdict CLEARED|BROKEN --file <f>
factoryd scm <verb> ...        # thin, scriptable access to the driver
factoryd doctor                # verify a factory's config, credentials, paths, identities
```

`doctor` is not optional. It must verify: config parses; both credentials
authenticate; **producer and reviewer resolve to different identities**; **the
identity `git` itself would present for the remote is the producer's** (§4.4);
declared paths exist and are writable; **every directory the gate command
references exists and is writable by the identity that will run it** (§9.6); the
target repo is reachable; the workdir is a clone, not a worktree (§4.4). It exits
non-zero on any failure and names which.

The git-transport check is separate from the API one because the two identities
are resolved by different mechanisms — one from an explicit header, the other
from whatever the ambient credential helper returns first — and they can
disagree. Observed in production: they did, and every API-level check still
reported healthy. "Could not determine" fails, on the same standard as
distinctness: undecided is not satisfied.

### 4.2 Supervisor

One implementation, parameterised by role. Blocks on a trigger, runs **one** agent
turn, re-arms. Never runs a turn concurrently with itself.

**Spin guard (mandatory).** An agent turn must consume the trigger it acted on. If it
does not, the supervisor would relaunch a model turn every poll — expensive, endless,
and silent. Therefore:

- track consecutive turns that left the trigger unconsumed
- **reset the counter on evidence of progress**, not on consumption (§6.3). A large
  task legitimately spans turns; a guard that cannot tell "still working" from
  "achieving nothing" halts real work. This distinction was a bug in the first
  implementation and must be designed in.
- warn at `spin_warn` (default 3), back off
- **halt at `spin_abort`** (default 6–10): write the stop sentinel, record the reason
  in health *and* in the log, exit. Never relaunch indefinitely.
- **preserve the trigger** on abort. It is the only evidence a signal arrived.

### 4.3 Watcher

Blocks until a trigger appears, then returns. In v1 this exited and nothing re-armed
it; a later signal then reached nobody. In v2 the watcher is internal to the
supervisor and cannot be independently lost.

While a trigger is pending and unhandled, the supervisor must **not** re-arm into a
tight loop and must **not** consume the trigger. It records `pending` with an age and
lets the health subsystem escalate.

### 4.4 Submit — the only network step

The producer may run in a sandbox with **no network and a read-only `.git`**
(verified: `curl` exits 6; `touch .git/x` fails with `Read-only file system` while a
plain file in the same directory succeeds). Do not attempt to widen that sandbox.
Instead the producer declares intent in plain files and `factoryd submit`, running
outside the sandbox, does the git and network work:

```
.producer-branch       branch name — required when changes exist; refuse to guess; refuse the target branch
.producer-commit-msg   commit message; non-empty is the signal that there is work to commit
```

Both are control files: excluded from the commit and deleted after use.

Submit is also **the gate**. In order:

1. resolve the **producer** credential; **fail closed** — never fall back to the
   reviewer's. Refuse if the two resolve to the same identity, naming it.
   This governs **both the API and the git transport** — see below.
2. materialize: create branch from target, stage, commit
3. re-check the gate's declared paths (§9.6). A missing one exits **3**, not 5:
   a misconfigured host is not a red branch.
4. run the configured gate command (build/vet/test). If red: write a question
   (§6.2), signal, **do not push**
5. push, create-or-update the Draft PR/MR
6. signal the reviewer

Exit codes are part of the contract: `0` submitted · `4` nothing to submit ·
`5` gate failed · `3` configuration/identity failure.

**The producer credential governs the git transport, not only the API.** This is
stated separately because it is the half an implementer will miss: `fetch` and
`push` do not take the token you pass to the API. Left alone, git consults the
ambient credential helper, and the first matching entry wins whoever it belongs
to.

Observed in production, 2026-09-01: the first entry in the host's
`~/.git-credentials` was the **reviewer's**, so the producer's push went out
under it. It failed closed only because that credential happened to lack
repository write scope. Had it carried write, the push would have **succeeded as
the reviewer** and nothing would have said so — a successful push looks identical
either way — breaking two-party trust at the git layer while every API-level
check still passed.

Therefore, for every git operation including `fetch`:

- clear inherited credential helpers and supply the producer credential
  explicitly;
- **not through argv.** `/proc/<pid>/cmdline` is world-readable, so a token on a
  command line is readable by anything on the host. Use a `0600` file the process
  owns and removes, or stdin — never a remote URL with the credential embedded.

**The workdir must be a clone, not a git worktree.** A worktree keeps its refs in the
parent repository, outside the sandbox, so the producer cannot write them. `doctor`
must check this.

### 4.5 Reconciler / health tick

Periodic, model-free. Detects: dead supervisors, unreviewed changes, stale triggers,
**resource exhaustion** (§7). Emits a structured health document and raises alerts.
Must never merge, review, or re-arm — those need a live agent.

---

## 5. Typed core

### 5.1 Results, not exit codes

Every driver operation returns a typed result. The v1 bug — a failed merge returning
success — must be unrepresentable:

```go
type MergeOutcome int
const (
    MergeUnknown MergeOutcome = iota // zero value is NOT success
    Merged
    RefusedDraft
    RefusedPipeline
    RefusedConflict
    RefusedScope
    RefusedMissingAudit
)

type MergeResult struct {
    Outcome     MergeOutcome
    MergeCommit string   // non-empty iff Merged
    Reason      string   // human-facing, always populated on refusal
}
```

Callers must switch exhaustively. The zero value means "unknown", never "fine".

**Verification is separate from the API's word.** After a merge, confirm the commit is
an ancestor of the target branch before reporting success. v1's merge reported success
while the branch was untouched; the API is not the authority on what landed.

### 5.2 Processes

Never identify a process by matching a command-line pattern — it matches the caller,
and it matches a child that shares argv. Hold `*os.Process` handles; persist PID plus
start-time so a recycled PID cannot be mistaken for a live one. Expose the real
parent/child relationship in `status`, so a supervisor's own worker is never reported
as a duplicate supervisor.

### 5.3 Provider drivers

Interface, with GitHub and GitLab implementations:

```go
type Driver interface {
    ListOpen(ctx) ([]Change, error)
    Get(ctx, id) (Change, error)
    Diff(ctx, id) ([]FileDiff, error)
    Pipeline(ctx, id) (PipelineStatus, error)
    Comment(ctx, id, body string) error
    SetDraft(ctx, id, draft bool) error
    Merge(ctx, id, expectedHead string) (MergeResult, error)
    PostAudit(ctx, id, sha string, a Audit) error
    Audits(ctx, id, sha string) ([]Audit, error)
    Whoami(ctx) (Identity, error)
}
```

Both implementations must pass one shared conformance suite against recorded fixtures.
v1's two drivers diverged: five verbs the producer half needed existed only in the
GitHub driver, which silently made the producer daemon GitHub-only. A conformance
suite makes that a build failure.

---

## 6. State and protocol

### 6.1 One state document

Replace v1's eight ad-hoc files with a single versioned, atomically-written document
per factory (`state.json`), plus **only** the trigger files that must be visible to a
sandboxed agent (§6.2). Include a `schema_version`; refuse to operate on an unknown
one rather than guessing.

### 6.2 Filesystem handoff

Deliberately kept from v1, and deliberately plain files: agents may be sandboxed,
vendor-heterogeneous, and unable to reach any network. The filesystem is the one
channel that always works when both roles share a host.

| path | direction | meaning |
|---|---|---|
| `inbox/brief.md` | operator → producer | a work order |
| `inbox/wake` | producer → reviewer | a change is ready |
| `inbox/question.md` | producer → reviewer | **blocked before having anything to push** |
| `outbox/answer.md` | reviewer → producer | reply to a question |
| `outbox/<id>.json` | reviewer → producer | a verdict |
| `inbox/producer-progress` | producer → supervisor | mtime = "I advanced" (§6.3) |

The **question channel** is a v2 addition. v1 defined signalling only for the push
path, so an agent blocked *before* having something to push had no way to reach its
counterpart and stopped, requiring a human relay. A question must carry a proposed
fix: a yes/no costs one round trip, an open question costs three.

**Verdict summaries must stand alone.** The counterpart may be a sandboxed agent that
cannot fetch the PR comment the summary references. Put the substance in the summary.

### 6.3 Progress, not consumption

`inbox/producer-progress` exists so the supervisor can distinguish *"working, not
finished"* from *"achieving nothing"*. The agent touches it whenever it advances.
A changed mtime resets the spin counter. Without this, a legitimate multi-turn task
trips the circuit breaker and halts the factory — a check firing on the wrong
condition.

### 6.4 Audits

An escalate-class change requires a recorded adversarial pass before merge, enforced
by the merge gate, not by convention. An audit that records **no attempts** must be
rejected: *an audit that lists nothing tried is not a pass.* v1 enforced this and it
caught a real omission; keep it.

---

## 7. Health, alerts, resources

The health subsystem detects; an **alert transport** delivers. v1 had the first and
not the second, and a stalled factory was recorded correctly every 15 minutes into a
journal nobody read.

- Configurable transports: command, webhook, file, syslog. Multiple, best-effort,
  independent — one failing must not suppress the others.
- **An unconfigured transport is a startup error, not a silent default.** If nothing
  can receive an alert, say so loudly at boot.
- Escalating cadence: alert after N intervals, repeat at a longer interval so it nags
  without becoming noise.
- **Resource guards** are part of health, not an afterthought: disk headroom on every
  volume the factory writes to, agent-turn duration, and cache growth. The predecessor
  filled a 232 GB volume to 96% with no signal; the first symptom would have been a
  confusing build failure.
- The factory must bound its own caches (per-turn build caches, clones, worktrees) and
  report what it reclaimed. Growth without a bound is a defect, not a tuning issue.

---

## 8. Status page

`factoryd status --serve :8080` — stdlib `net/http`, no external assets, single binary.

It answers, in this order: **is it working? what is it doing right now? what is it
waiting on? what needs me?**

- per-factory: supervisor and watcher liveness (with the real process tree), the
  running turn and its age, pending triggers with ages, open changes, last verdict,
  health state, resource headroom
- a JSON endpoint with the same data, so it can be scraped
- read-only. It must never start, stop, or consume anything.

Rationale: during operation the most common question was *"is this working? I have no
way to see status."* Everything the factory does is observable, but only if you know
the six places to look. This is those six places.

---

## 9. Evidence rules (normative)

These are project conventions, enforced in review, and they are why the factory
catches things:

1. **Sensitivity-check every check.** A test that passes identically with and without
   the behaviour is inert. Remove the behaviour; confirm the test fails.
2. **Assert a mutation landed before believing its result.** The text changed *and*
   the file still compiles/parses. A red from a syntax error proves nothing. Four
   "mutation caught" results in the predecessor were build errors.
3. **A zero needs a positive control.** Every zero has two explanations: nothing was
   wrong, or nothing was measured.
4. **Ask what silence means.** If a component going away produces no output, its
   silence is indistinguishable from success.
5. **Match command position, not the mention.** Guards that grep for a string fire on
   comments, on string literals, and on their own fixtures. Parse, or anchor.

6. **An environmental failure must not present as a failed check.** A gate whose
   build cannot write its cache directory exits non-zero exactly like a gate that
   found a real defect, and submit cannot tell them apart afterwards — so the
   discrimination has to happen *before* the gate runs. Observed in production: a
   missing directory was reported as `gate FAILED`, exit 5, and the producer would
   have been sent a `changes-requested` for someone else’s mistake. The cost is
   not the wasted cycle. A gate that cries red for environmental reasons trains the
   operator to disbelieve red gates, and the whole value of refusing to push on red
   depends on red meaning what it says.
7. **A fixture is evidence only if it came from the thing it describes.** A fixture
   written from documentation records what the API was *believed* to do. A driver
   matching it exactly passes every test above it while being wrong about the
   provider — a check that can fail against the wrong reality, which is harder to
   notice than one that cannot fail, because everything looks like it is working.
   Record fixtures from a live provider, error shapes first; state the provenance
   of every one; and when a case genuinely cannot be provoked, say why in the
   fixture itself.

---

## 10. Non-goals

- Not a CI system, not a PR bot, not a merge queue.
- No hosted control plane; no database. Files and one binary.
- Does not choose your agents. Producer and reviewer are commands you configure; any
  vendor, any model, possibly different for each role.
- Does not replace human judgement on high-blast-radius paths — that is what
  `operator-gated` is for.

---

## 11. Delivery sequence

Incremental, so an existing v1 factory never stops. Each step keeps the v1 CLI
contract so the two are swappable one verb at a time.

1. **Driver + conformance suite.** Port GitLab and GitHub behind `Driver`. Prove on
   read-only verbs (`status`, `list-open`) where a wrong answer is visible and cheap.
2. **`merge` with typed results and post-merge ancestry verification.** The one verb
   with a known v1 bug.
3. **State document + `doctor`.**
4. **Supervisor** (both roles, one implementation) with spin/abort and progress.
5. **Submit** with the gate, identity check — API *and* git transport (§4.4) —
   and sandbox-aware materialization.
6. **Health, alerts, resource guards.**
7. **Status page.**

## 12. Acceptance

v2 is done when, for one real factory: an operator drops a brief; a sandboxed producer
with no network and no git writes produces a change; submit gates and opens it; a
reviewer of a *different identity* reviews, runs an adversarial pass on an
escalate-class path, and merges; the merge is verified by ancestry; **and every one of
the failures in §1 is either impossible by construction or caught by a test that has
been shown to fail.**
