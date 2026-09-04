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
| 15 | A factory idled 3.5h with completed work stranded and every signal green | a turn consumed its trigger, then exited 1; the spin guard resets on consumption, so nothing counted and nothing re-armed | §4.2 a failure counter independent of the trigger |

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

That is the finished surface, not the current one. §11 gives the order the
subcommands arrive in; anything not yet built exits non-zero saying so, because a
subcommand that silently did nothing would be indistinguishable from one that ran
and found nothing to do.

`doctor` is not optional. It must verify:

- config parses;
- both credentials authenticate, and **producer and reviewer resolve to
  different identities**;
- **the identity `git` itself would present for the remote is the producer's**,
  and **the URL git would actually contact — after rewriting — is the configured
  remote, addressing the project the provider block names** (§5.4);
- declared paths exist and are writable; every path in
  `gate.required_writable_paths` is a writable directory **or is absent with a
  writable nearest existing ancestor**, since submit creates it (§4.4);
- **the producer cannot write `paths.submit_repo`** — established by running a
  write probe through the same mechanism the producer turn runs under, with the
  same probe succeeding against `paths.producer_workdir` as its control (§4.4);
- the submit repository's local git config carries **only** allowlisted keys
  (§5.4) — defence in depth, since the producer cannot reach that file;
- `gate.env` declares `PATH`, and every `${VAR}` referenced by a declared path
  is set in it — `doctor` resolves those paths against the same fully-declared
  environment `submit` will give the gate, not against its own (§4.4);
- the target repo is reachable;
- `paths.submit_repo` is a clone, not a worktree (§4.4).

It exits non-zero on any failure and names which.

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

**A failed turn is a failure whether or not it consumed its trigger.** The spin
guard keys on the trigger, and that gives it a blind spot with a production
incident behind it (§1, item 15): a turn that consumes its trigger and then
exits non-zero leaves nothing pending, so nothing re-arms, the spin counter
resets on consumption, and every signal reads as an idle factory with an empty
queue — while finished work sits stranded in the workdir. It idled for 3.5 hours.

So the supervisor keeps a second counter, **independent of consumption**:
consecutive turns that exited non-zero or timed out without reporting progress.
Progress resets it; consuming the trigger does not. It halts at `fail_abort`
(default 5) exactly as the spin guard halts at `spin_abort`, with a reason that
names which guard fired.

A counter alone is not enough, because after a consumed failure nothing is
pending and nothing external will start the next turn: the counter would sit at
one forever, which is the stall with a number on it. So the supervisor
**re-arms itself**. It writes a supervisor-owned marker (`inbox/<role>-retry`),
backs off, and runs the next turn on it. The marker is a file rather than a
memory, because a retry lost on restart recreates the stall it exists to fix; it
carries the original trigger and the failure that led here, so the agent — and
the operator — can read why this turn is a retry. The supervisor removes it once
the retry has run, unless that retry is the one that halts: then it stays, as a
real trigger does, because a halt should not erase its own evidence.

An interrupted turn counts toward neither guard. Stopping the supervisor kills
the running turn with its process group, and the exit code that produces says
nothing about the agent; counted, an ordinary shutdown would leave a failure on
the streak, a later unrelated failure would halt the factory, and the operator
who stopped it would never connect the two. The turn is recorded as
interrupted. But if the agent had already consumed its trigger when it was
killed, a restart would find nothing pending — the same stall, arriving via
Ctrl-C — so the supervisor persists a marker saying so, and the restart runs
exactly one recovery turn. Continuity, not failure: the streak stays at zero.

**The marker's own I/O is control-plane, and its failure halts.** A marker that
cannot be written leaves nothing to re-arm on; a marker that cannot be removed
re-arms the same retry forever, every run a success, invisible to both guards.
Neither may be logged and stepped over as though the marker state had changed.
Both halt, with a reason that names the marker.

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

That observation is what prompted the design; it is **not** what the design rests
on. A sandbox that permitted the producer to write its `.git` must change
nothing, because submit does not use it — see the two-directory boundary below.
A safety property inferred from how one sandbox happened to behave is an
assumption, not a boundary, and it would go quiet the day someone changed the
sandbox.

```
.producer-branch       branch name — required when changes exist; refuse to guess; refuse the target branch
.producer-commit-msg   commit message; non-empty is the signal that there is work to commit
```

Both are control files: excluded from the commit and deleted after use.

Submit is also **the gate**. In order:

1. resolve the **producer** credential; **fail closed** — never fall back to the
   reviewer's. Refuse if the two resolve to the same identity, naming it.
   This governs **both the API and the git transport** — see below.
2. materialize: create branch from target, stage, commit. The branch that is
   pushed is **derived from the content**: `<declared>-<tree[:10]>` — the
   *tree* sha, not the commit sha, which carries timestamps and would make the
   same content a new branch every time. The declared name is the *family*;
   each submission is an immutable member of it.
3. list the open changes in the family; **every one must be ours** (below). A
   ready or foreign change anywhere in the family stops the submission here,
   before the gate spends anything — exit **3**. A draft already on *this*
   branch is the same tree, already submitted: exit **4**, naming it; nothing
   is gated or pushed.
4. re-check `gate.required_writable_paths` (below). A missing or unwritable one
   exits **3**, not 5: a misconfigured host is not a red branch (§9.6).
5. run the configured gate command (build/vet/test). If red: write a question
   (§6.2), signal, **do not push**
6. **re-read the family immediately before the push**. The gate may have run
   for a long time; a draft a reviewer marked ready in the meantime has left
   the producer's hands and the push does not happen — exit **3**, the
   producer's newer work waits for the reviewer's decision.
7. push, **non-force**, to the content-derived branch; create-or-update the
   Draft PR/MR; re-read it after the push and validate it in full.
8. **supersede — by report, never by write**: the earlier drafts in the
   family are named in the new draft's body, the log and the result, and left
   open. Submit never writes to an existing change. A read of one is stale by
   the time a write would land, and neither provider offers a close
   conditional on the state that was read; a reread before a close is not a
   guard, it is the same race one call later. The reviewer, who holds the
   merge, closes what is superseded.
9. signal the reviewer

**Why the branch is immutable.** A check-then-push on a mutable branch has a
window: however close the ready/draft check sits to the push, a reviewer can
mark the change ready in the instant between, and the push then puts new code
into a change that is no longer the producer's. No provider offers a lease
that closes that window. The immutable branch makes the pre-push check
*sufficient* rather than merely *recent*: the push targets a branch that no
existing change references, so even a flip in that instant cannot be pushed
*into* — the worst case is a superfluous new draft beside a change the
reviewer took, and step 8 leaves that change alone. Identical content is the
same tree, the same branch, and the draft that already exists.

**A change is ours only when it says so.** Ownership requires an open draft
targeting the configured branch, a producer login that is known, and an author
equal to it. Both drivers map an omitted author to `""`; an unknown owner is
not the same as ours, and a check that accepted it would pass on exactly the
incomplete response it should refuse.

Exit codes are part of the contract: `0` submitted · `4` nothing to submit ·
`5` gate failed · `3` configuration/identity failure.

**The crossing is root-mediated, so nothing on it is judged by path.** Submit
runs as factoryd and reads a tree the producer can rewrite at any moment —
including after a clean exit, by a child it left behind. Every read on the
crossing is therefore `openat` on the directory in hand with `O_NOFOLLOW`, judged
on the descriptor actually opened: a control file must be a regular file (a
symlink to a credential, inside or outside the tree, is refused unread; a fifo is
refused); every file copied is opened the same way, copied from that descriptor,
and a directory is listed through its own descriptor. A file swapped for a link
between listing and open fails the open and refuses the submit. `os.Root` is not
used for this: it resolves an in-tree symlink itself before the final open, so
`O_NOFOLLOW` never sees the link. A turn whose leader exited while processes in
its group still ran is **not clean**: they are killed, nothing follows the turn,
and it counts as a failure — and because a detached child escapes the group,
the crossing does not rely on that check. **The producer receives no credential**
is proved by `doctor` as the producer: neither credential file may be readable
by it, with the control that its own workdir is.

**The producer's sandbox is the supervisor's, not the agent's.**
`roles.producer.sandbox.no_network` starts the turn in a new, empty network
namespace, created by factoryd as root at clone before the identity switch; a
factoryd that cannot create it refuses to start the turn rather than starting it
connected. `doctor` proves it from inside: a listener doctor opens must be
unreachable from the sandboxed probe and reachable from the unsandboxed one.
**It is for producers whose network is not their own** — a scripted turn, or
an agent whose tooling sandboxes its shell while the agent process itself talks
to a hosted model. `no_network` takes the network from the *whole* turn, the
agent process included; a producer that is itself a hosted-model CLI (codex,
claude) must reach its API and **must not** set it — its own tool sandbox is what
keeps the *shell* offline, and §4.4's two-directory boundary is what keeps git
out of its hands either way. SUBMIT is the producer supervisor's **after-turn
step**: after a turn that
exited clean and declared intent, the supervisor runs submit outside the sandbox
as itself; a submit that fails on configuration or identity counts on the fail
streak like a turn that exited non-zero.

**The producer credential governs the git transport, not only the API.** The
contract and its verification oracle are in §5.4; this is why it exists, and it
is stated separately because it is the half an implementer will miss: `fetch` and
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

Therefore every git operation, `fetch` as much as `push`, runs through a
`GitTransport` (§5.4): inherited credential sources suppressed, the producer
credential supplied explicitly, and never through argv.

**The gate declares the paths it needs; they are not discovered.**

```json
"gate": {
  "command": ["bash", "-c", "go build ./... && go test ./..."],
  "env": {
    "PATH": "/usr/local/bin:/usr/bin:/bin",
    "HOME": "/var/lib/factoryd/widgets",
    "GOCACHE": "/var/cache/factoryd/widgets/go",
    "GOFLAGS": "-count=1"
  },
  "required_writable_paths": ["${GOCACHE}", "build/artifacts"],
  "timeout_seconds": 900
}
```

**The gate cannot read `.git`, by design, so the gate must not walk it.** The
submit repository's `.git` is `0700` factoryd-owned precisely so that
producer-authored build code cannot plant a hook; a gate that walks the whole
tree (`gofmt -l .`) therefore dies on it, and submit reports that as a red gate
(canary issue #21). A gate prunes it — `find . -path ./.git -prune -o -name
'*.go' -print0 | xargs -0 gofmt -l` — and the shipped examples do; the Go tools
themselves skip dot-directories. A gate must also be **able to go red**: a gate
that is already red before the producer touches anything gates nothing, and a
gate of `true` gates nothing either. Prove a new gate three ways: green at
baseline, red on a planted defect, green again.

Declared, because a gate command is an opaque argv — often a shell string — and
the paths a build writes to are set by environment variables, tool defaults and
the build system itself. Trying to recover them by parsing the command would be a
guess presented as a check: it would miss paths that matter and invent ones that
do not, and a check that is sometimes wrong about what it verified is worse than
no check, because it is believed.

**The gate's environment is declared in full, and nothing is inherited.** The
gate environment is exactly the `FACTORYD_*` variables submit sets, plus
`gate.env`. There is no allowlist and no pass-through. `PATH` is required in
`gate.env`; a gate with no `PATH` cannot run anything, and inheriting one would
reintroduce the whole problem.

An earlier draft allowed an `inherit_env` allowlist, on the reasoning that
naming the variables made them accountable. It does not. `doctor` is run from a
terminal and `submit` runs as a service; even with identical *names* the
**values** differ — a different `PATH`, a different `HOME`, a proxy set in one
and not the other. `doctor` would go green having resolved `${GOCACHE}` against
an environment `submit` never sees. Naming a variable does not make its value
the same in two processes, and no check inside `doctor` can establish that it is.

This is the "same environment" corollary of §5.4: the environment has to be
*owned*, not agreed upon. Declared in full, both commands compute the identical
environment from the identical file, and the question of whose process it came
from does not arise.

Submit records the resolved paths — after `${VAR}` expansion — in the state
document, so what was checked and what actually ran can be compared afterwards
rather than assumed equal.

Resolution rules, so the field means one thing:

- A relative path resolves against the gate's working directory.
- `${VAR}` expands from **that** environment. **An unset variable is an error,
  not an empty expansion** — expanding `${GOCACHE}/x` to `/x` would check a path
  the gate will never touch and pass.
- No shell, no globbing, no `~`. Anything needing those is a path the operator
  should write out.

**A declared path is a directory, and submit creates it if it is absent.**
Detecting the missing cache directory would be an improvement on reporting it as
a red gate; creating it removes the failure entirely. So:

- `doctor` requires each path to be a writable directory, **or** absent with a
  writable nearest existing ancestor — creatable counts as satisfied, and is
  reported as such rather than silently.
- `submit` creates any absent path before running the gate.
- A path that exists as a non-directory, or cannot be created, is exit **3**.

The residual risk is a typo creating a directory nothing uses. That is visible
and harmless, and much cheaper than a false red gate that teaches the operator
to disbelieve red.

An empty list is legal and means the gate needs nothing beyond its workdir. It is
a claim the operator makes, and a wrong one shows up as an exit 3 rather than a
mystery.

**Git never runs in a repository the producer can write.** There are two
directories, and the separation is the boundary:

| | `paths.producer_workdir` | `paths.submit_repo` |
|---|---|---|
| written by | the producer | factoryd only |
| contains | source the producer edits | the clone every git command runs in |
| its `.git` | ignored entirely; may not exist | factoryd's, and the only one git reads |

Submit copies the producer's **source tree** into the submit repository and does
all of its git work there. The copy excludes git control data — any path named
`.git`, at any depth, file or directory — and refuses a symlink that resolves
outside the tree. Nothing the producer wrote can reach the configuration git
reads, because it is not copied and the destination is not writable by the
producer.

This replaces an earlier design in which the producer's own clone was validated
before each operation. That was a time-of-check-to-time-of-use race and could not
be fixed by checking harder: submit read `.git/config`, approved it, then started
a git process that read the file again, and the producer could replace it
atomically in between. "Factoryd created" is provenance at one instant, not
ownership over time, and a cooperative lock between a factory and the agent it
is sandboxing is not a boundary. The allowlist of §5.4 still applies to the
submit repository, but as defence in depth over a file the producer cannot
reach — not as the mechanism.

**The boundary is verified by probe, not by assertion.** `doctor` runs a write
probe **through exactly the mechanism the producer turn runs under** — the same
sandbox wrapper, the same user — and requires it to fail against
`paths.submit_repo`. The same probe must **succeed** against
`paths.producer_workdir`: without that control, "the producer cannot write it"
is indistinguishable from a probe that cannot write anything.

If the producer turn runs with the same authority as submit, there is no
boundary to verify and `doctor` fails, saying so. An unenforced separation
recorded as enforced is worse than none: it is the two-party split on paper with
nothing holding it up.

**The gate is a third principal.** It runs producer-authored build and test
code inside the factoryd-owned repository, and factoryd itself holds root in
order to switch identities. Run as factoryd, that code reads every credential
on the host and can rewrite the submit repository's hooks; run as the producer,
it cannot write the build outputs it exists to produce. So `gate.run_as` names
an unprivileged user distinct from both, and `doctor` probes it the same way it
probes the producer:

- it **can** write each path in `gate.required_writable_paths` — the control,
  so that "cannot" below is not a gate that cannot do anything;
- it **cannot** write `submit_repo/.git` — producer-authored code run by the
  gate could otherwise plant a hook that runs during `git push`, after the
  guard, with the credential helper active;
- it **cannot read** either credential file.

The same reasoning governs what crosses into the submit tree. A symlink is
judged by where it will point **after** it is recreated in the destination, not
by where it points in the source: a link `hooks -> .git/hooks` is "inside" the
producer's tree — whose `.git` is skipped, so it may not even resolve there —
yet recreated verbatim it points at the factory-owned hooks directory. Absolute
targets, targets that escape via `..`, and targets passing through any `.git`
component are refused.

**Every turn's environment is constructed, like the gate's.** `roles.<role>.env`
is the whole environment beyond `FACTORYD_*`; nothing is inherited. Passing the
supervisor's environment through would hand the producer every variable the
supervisor holds — including a reviewer credential named by
`credentials.reviewer.env` — and the two-party model would be one `getenv` away
from gone. The reviewer's credential variable is delivered to the reviewer's
turn by name; the producer receives no credential of either role, because it
never runs git and never calls the API. Commands are resolved against the
declared `PATH`, not the process's: `doctor`'s shell can find `go` where the
gate's environment cannot, and a green `doctor` would then vouch for a gate that
fails on its first run.

**The remote is pinned by authority, not only by path.**
`https://evil.example/acme/widgets.git` names the right project on the wrong
host, and every downstream check — the guard, the identity oracle — would then
faithfully verify and push the producer's credential to it. The expected host is
derived from the provider block: `github.com` for the public API, the
`base_url`'s authority for GitHub Enterprise and for every GitLab.

**The submit repository must be a clone, not a git worktree.** A worktree keeps
its refs in the parent repository, so the refs submit writes would land outside
the directory whose ownership was just established.

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

    // GitCredential is what this provider expects over HTTPS. See §5.4: the
    // two do not agree on the username half, so it cannot be derived from the
    // token alone.
    GitCredential(secret string) GitCredential
}
```

Both implementations must pass one shared conformance suite against recorded fixtures.
v1's two drivers diverged: five verbs the producer half needed existed only in the
GitHub driver, which silently made the producer daemon GitHub-only. A conformance
suite makes that a build failure.

### 5.4 Git transport

The provider driver speaks the API. Nothing in it touches git, and the two
authenticate by different mechanisms — which is exactly how a producer came to
push under the reviewer's credential while every API check passed (§1, item 11).
So the git side gets its own contract, and its own identity question:

```go
// GitIdentity is who a transport authenticates as at the remote.
type GitIdentity struct {
    Login string // the account the remote sees
    // Source names how it was resolved: "credential-helper" or "ssh".
    Source string
}

// GitTransport performs submit's git operations under an identity it can state.
//
// Identity must resolve using the SAME isolation and credential source that
// Fetch and Push use. A check performed under different conditions than the
// operation it vouches for proves nothing about that operation.
type GitTransport interface {
    // Identity resolves who this transport would present to the remote. It
    // returns an error rather than a guess when it cannot: an unresolved
    // identity is undecided, not satisfied.
    Identity(ctx context.Context) (GitIdentity, error)

    Fetch(ctx context.Context, refspec string) error
    Push(ctx context.Context, refspec string) error
}
```

**The credential has two halves, and only the provider knows the first.** Git
authenticates over HTTPS with a username *and* a password, and the conventions
differ: the token alone is not enough to answer `git credential fill`.

```go
// GitCredential is what git is handed for an HTTPS remote.
type GitCredential struct {
    Username string
    Secret   string
}
```

The username is provider-owned, supplied by `Driver.GitCredential` (§5.3), so
neither the transport nor the operator has to know the convention. It is not
cosmetic: the host in the incident behind §1 item 11 had entries under **two**
different usernames for the same instance, and which one git picked decided
which identity pushed.

**Configuration.** The remote is declared, not discovered. A transport that
inferred its remote from the workdir would verify one thing and push to another:

```json
"git": {
  "remote": "https://gitlab.example.com/acme/widgets.git",
  "transport": "https",
  "ssh_key_file": "/etc/factoryd/widgets/producer_ed25519",
  "known_hosts_file": "/etc/factoryd/widgets/known_hosts",
  "proxy": "",
  "ca_file": ""
}
```

`transport` is `https` or `ssh`. For `https` the credential is
`credentials.producer` — the same one the API uses, and no other. For `ssh`,
`ssh_key_file` and `known_hosts_file` are required and ignored otherwise.
`proxy` and `ca_file` are the **only** way a proxy or an alternative trust store
reaches the transport: empty means direct connection and the system trust store,
and nothing ambient can widen either (see the environment rule below).

**Isolation is part of the contract, not a precaution.** Every git invocation
must run with inherited credential sources suppressed:

- **https:** no ambient `credential.helper` may apply. Submit supplies its own,
  configured explicitly for this invocation, reading the producer credential from
  a file the process owns at mode `0600` and removes, or from stdin. **Never a
  remote URL with the credential embedded, and never argv** —
  `/proc/<pid>/cmdline` is world-readable, so a token on a command line is
  readable by anything on the host.
- **ssh:** `-F /dev/null` first — without it, `~/.ssh/config` and the system
  ssh config still apply, and a `ProxyCommand` or `ProxyJump` there routes the
  connection through a host factoryd never chose, with the effective URL
  unchanged. Then, on that clean base: `IdentitiesOnly=yes`,
  `IdentityAgent=none`, an explicit `IdentityFile`, and the host-key set pinned
  from **both** directions: `UserKnownHostsFile` set to `git.known_hosts_file`,
  `GlobalKnownHostsFile=/dev/null`, `StrictHostKeyChecking=yes`, and
  `UpdateHostKeys=no`.

  Each of those is there because the one before it is not enough. `-F /dev/null`
  discards the config files but not OpenSSH's built-in defaults, and one of
  those defaults is `GlobalKnownHostsFile /etc/ssh/ssh_known_hosts
  /etc/ssh/ssh_known_hosts2` — verifiable with `ssh -G`, which still lists them
  after `-F /dev/null` and an explicit user file. So a key absent from
  `git.known_hosts_file` is still accepted if the host's system file carries it:
  the pinned set was a floor, not a set. `GlobalKnownHostsFile=/dev/null` makes
  the user file the whole of it. `UpdateHostKeys=no` then stops the remote
  extending that set itself — with it on, a server can hand over additional
  keys that ssh records, and the trust set factoryd pinned grows without
  factoryd's involvement.

  Trust-on-first-use inside an unowned home directory is one more ambient
  input; so is a system known-hosts file nobody in this factory maintains. An
  agent holding the reviewer's key is the same failure as a credential helper
  holding the reviewer's token; a ProxyCommand in a dotfile is the same failure
  as `http_proxy` in the environment.

**The invariant.** *A verification is valid only for an operation it shares an
environment with, and only while nothing can change that environment in
between.*

Everything below is an instance of it, and so was every gap found reviewing this
section: the credential was pinned but not the target, the token was pinned but
not the username, the path was pinned but not the environment it resolved in.
Each time, the check verified the thing next to the thing that actually decided.
Two corollaries do the work:

- **Same environment.** The environment must be fully owned by factoryd, so that
  "the environment `doctor` saw" and "the environment `submit` used" are the
  same object rather than two processes' ambient state that happen to agree.
- **No window.** A check that ran earlier vouches for nothing if what it checked
  can change afterwards. Where the gap cannot be closed, the check moves next to
  the operation.

**Pinning the credential is not enough; the target must be pinned too.** A
correct `remote` proves nothing on its own. `url.<base>.insteadOf` and
`pushInsteadOf` rewrite it, `core.sshCommand` and `GIT_SSH_COMMAND` replace the
transport, and `GIT_DIR`, `GIT_ASKPASS` and the system and global config files
all reach in from outside. Every one of those can send a correctly-configured
remote somewhere else, and the push still looks like it worked.

So the git environment is isolated to the same standard as the credential:

- `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` point at `/dev/null`; only
  configuration submit sets explicitly applies.
- **The repository's own `.git/config` is owned outright, by allowlist.** It is
  not covered by the `/dev/null` above, and it is the one file the producer can
  actually reach: it has write access to its workdir by definition, so it can
  configure the very repository submit is about to push from.

  A denylist cannot close this. Refusing `credential.helper` and
  `url.*.insteadOf` leaves `http.<url>.proxy` sending the connection through an
  interceptor, `http.sslVerify=false` accepting any certificate, and
  `http.sslCAInfo` swapping the trust store — **none of which change the
  effective URL**, so the target check passes while the credential is handed to
  someone else. Nor is that list complete either; it is the next three anyone
  thought of. You cannot enumerate what you do not own, and every future Git
  release makes a denylist more wrong.

  So the local configuration is **default-deny**:

  - Git runs only in `paths.submit_repo`, which the producer cannot write
    (§4.4). That is the boundary. The allowlist below is defence in depth over a
    file the producer cannot reach — checking a file the producer *could* write
    would be a time-of-check-to-time-of-use race, since git rereads it after the
    check.
  - Before every fetch and every push, submit reads the repository's effective
    local configuration and **refuses if any key outside this allowlist is
    present**:

    ```
    core.repositoryformatversion   core.filemode      core.bare
    core.logallrefupdates          core.symlinks      core.ignorecase
    core.precomposeunicode         extensions.objectformat
    remote.<name>.url              remote.<name>.fetch
    branch.<name>.merge            branch.<name>.remote
    ```

    That is what a clone needs to be a clone. Everything else — every `http.*`,
    every `url.*`, `credential.*`, `core.sshCommand`, `core.worktree`,
    `include.path`, `includeIf.*` — is absent or submit does not run. A key Git
    adds next year is refused by default rather than admitted by omission.
  - `remote.<name>.url` is allowlisted because a clone has one, **and submit
    does not use it**: pushes and fetches name the URL explicitly, and the
    effective-URL check below still runs, because `insteadOf` rewrites an
    explicit URL too.

  This is the "fully owned" corollary applied properly. The earlier draft of
  this section policed the local config with a denylist, which satisfies the
  letter of the invariant and not the substance: the verification and the
  operation still shared configuration that factoryd did not own.
- **The git process environment is constructed in full, exactly as the gate's
  is (§4.4).** An earlier draft dropped inherited `GIT_*` variables, which is a
  denylist wearing an environment's clothes: `http_proxy`, `https_proxy`,
  `all_proxy`, `NO_PROXY`, `SSL_CERT_FILE`, `SSL_CERT_DIR` and `CURL_CA_BUNDLE`
  all steer git's HTTPS transport and none of them starts with `GIT_`. A filter
  keyed on a prefix owns the prefix, not the environment — and a `doctor` run
  from a terminal and a `submit` run as a service would still transport through
  different proxies while both reported the same green.

  So every git and ssh process starts from an environment factoryd builds:
  `PATH` and `HOME` from configuration, the `GIT_CONFIG_*`/`GIT_TERMINAL_PROMPT`
  and inert-`GIT_ASKPASS` settings above, and nothing else. A proxy or TLS
  override reaches the transport only through a typed factory config field
  (`git.proxy`, `git.ca_file`) that declares it — never through whatever the
  parent process was started with. `doctor` and `submit` build this environment
  from the same configuration, so "which proxy" stops being a property of who
  launched the process.
- The transport is set explicitly, never inherited.

And the effective target is checked, not the configured one — **inside the
transport, immediately before every fetch and every push**, not once as a
`doctor` preflight. This is the "no window" corollary: `doctor` runs before the
producer's turn, and the producer can edit `.git/config` during it. A preflight
answer about the target is stale by the time the push happens.

- **the URL git will actually use** — `git remote get-url` and
  `git remote get-url --push`, both after rewriting — must equal the configured
  `git.remote`;
- **`git.remote` must address the same project** the provider block names
  (`github.owner`/`repo`, or `gitlab.project`). A remote pointing at a different
  repository than the one being reviewed is a misconfiguration no identity check
  would catch: the right account pushing to the wrong place.

**The verification oracle**, so `Identity` is implementable rather than aspirational:

- **https:** run `git credential fill` for the configured remote, under the
  isolation above. Whatever it returns is what git will use. Resolve that
  credential through the provider API (`Driver.Whoami`) and report the identity
  it names. A helper that returns nothing is an error, not an empty identity.
- **ssh:** run the provider's authentication probe (`ssh -T`) under the isolation
  above and read the account out of the greeting — GitHub answers `Hi <login>!`,
  GitLab `Welcome to GitLab, @<login>!`. An unparseable greeting is an error.

Both are read-only and safe to run from `doctor`. Neither infers the answer from
configuration: they ask the mechanism that will actually be used.

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

Both directories are **the producer's to write**: it consumes its brief and
records progress in the inbox, and consumes the verdicts and answers that arrive
in the outbox. `doctor` probes both as the producer identity, because factoryd
being able to write them says nothing — found in the acceptance run, where a
root-owned inbox made every clean producer turn read as "no progress" and a
root-owned outbox left every verdict unconsumed.

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

**The merge gate is `factoryd signal <id> merged`**, and the policy it enforces is
the `scope` block of the config (§2 "scope policy as data"), in Go RE2 syntax,
compiled at load — a pattern that does not compile refuses the config, because a
policy with a silently dropped rule is a policy with a hole where the operator
believes there is a rule. A config with no scope block is refused; an empty policy
must be declared, not arrived at by omission.

The gate, in order, on the diff of the head the verdict names:

1. **Deny.** A changed path (both names of a rename; a deletion counts) matching a
   `deny_regexes` pattern and no `allow_regexes` pattern is *operator-only*.
2. **Hold.** An **added** diff line — not a removed one, not a `+++` header —
   matching a `hold_diff_regexes` pattern is *operator-only*, whatever its path.
3. **Escalate.** A changed path matching an `escalate_regexes` pattern requires a
   recorded audit **on the exact head**: at least one `CLEARED` with attempts, and
   no `BROKEN`. An audit on an earlier head is an audit of something else.

The strictest class wins and every reason is kept and shown. **An operator-only
result refuses the `merged` signal; it does not downgrade it.** The reviewer then
signals `operator-gated` themselves, so the recorded verdict is always the one the
reviewer chose, never one the gate substituted behind their back. The `sha` argument
must be the change's current head (`auto` means "the head now"); a verdict on a
commit that has since been replaced is a verdict on something the producer no
longer has, and is refused as *head moved*.

**Two-party trust is decided at the gate, on stable ids.** The reviewer signalling
and the change's author are compared on the provider's stable id — never the
login, never a `doctor` result that may be stale or a credential that may have
been swapped since — and equality refuses every verdict, not only `merged`. An
author the provider did not name is not known to be distinct, and refuses.

**An audit's author is the provider's word.** The driver sets it from the
authenticated author of the comment that carries the audit, overriding anything
the body claims — a body is written by whoever posts it, and an audit that could
name its own author could be forged by the producer for its own head. The gate
counts an audit only when its author is the reviewer signalling now. Audits by
anyone else — the change's author above all — are **ignored and named, never a
veto**: a producer's comment must not decide a merge in either direction. (Found
live: a first cut refused on a forged audit even with the reviewer's genuine one
beside it, which let the producer block its own merge.) A `BROKEN` audit *by the
reviewer* refuses; an unattributed audit is ignored with its own reason.

**The gate does not mark a draft ready.** No provider offers a ready mutation
conditional on the head, so a producer pushing between the check and the
mutation would have its unreviewed head marked ready by the gate — the
head-conditional merge would then refuse, but the ready state would stand.
Marking ready is the reviewer's own explicit act after review
(`factoryd scm set-draft <id> false`); a `merged` signal on a draft is refused
and says so.

**Content policy is evaluated on delivered content only.** GitLab collapses large
diffs and withholds ones that are too large; GitHub omits the patch of a large or
binary file. A file whose content was not delivered is marked *incomplete* by the
driver, and when any `hold_diff_regexes` are configured the `merged` signal is
refused rather than evaluated on an empty patch — which would pass exactly the
file most worth looking at. A rename with a blank patch is where that hides most:
it is incomplete unless the driver *proves* it path-only — GitHub by zero
additions and deletions, GitLab (which gives no per-file counts) by equal blob
ids at the old path on the base and the new path on the head. Then: merge with the expected head, verify by
ancestry (§5.1).
Every verdict is written to `outbox/<id>.json` first — that is the handoff — then
recorded in the state document, then posted as a comment; a failed comment is
said, not fatal. Exit **0** done · **3** config/identity · **5** refused (scope,
audit, head moved, provider) · **6** the provider claimed a merge the repository
does not show.

`factoryd audit post <id> <sha> --lens <l> --verdict CLEARED|BROKEN --file <f>`
records the pass, as the reviewer, pinned to the head that exists; the file lists
the attempts, and no attempts is refused before any request is made.

---

## 7. Health, alerts, resources

The health subsystem detects; an **alert transport** delivers. v1 had the first and
not the second, and a stalled factory was recorded correctly every 15 minutes into a
journal nobody read.

- Configurable transports: command, webhook, file, syslog. Multiple, best-effort,
  independent — one failing must not suppress the others. **This build delivers
  through `file` and `command`**; a config naming `webhook` or `syslog` is refused
  at load, because a transport that is accepted and never delivers is the failure
  this section exists to prevent. They arrive when they can be delivered.
- **An unconfigured transport is a startup error, not a silent default.** If nothing
  can receive an alert, say so loudly at boot. `doctor` goes further: it delivers
  a probe alert through every transport and fails on any that does not arrive,
  because "the path looks writable" is not "the alert landed".
- Escalating cadence: alert after N intervals, repeat at a longer interval so it nags
  without becoming noise. One recovery when a condition clears, none for a condition
  that was never announced. The cadence record lives in `state.json`, under the same
  lock as everything else, and is written **before** the alert goes out: a crash
  between the two loses one alert, which the repeat resends; the reverse order
  would send the same alert on every tick.
- `factoryd health` is the tick: one observation, or `--loop`. It writes
  `<root>/health.json` atomically and exits **0** healthy, **1** findings, **3**
  could not look. A tick that could not observe something (a volume it cannot
  stat, a provider it cannot reach) is **unhealthy**, not quiet: a tick that
  cannot look is not a tick that saw nothing.
- Conditions: supervisor registered and not alive and not halted; liveness that
  cannot be determined; a halted role (until an operator clears it); a turn past
  its timeout **plus grace** (the supervisor should have ended it, so the
  supervisor is stuck); a trigger unconsumed past the stale threshold; an open
  change untouched past the unreviewed threshold — or with no timestamp at all,
  which is an unknown age, not a recent one; no state document at all.
- **Resource guards** are part of health, not an afterthought: disk headroom on every
  volume the factory writes to, agent-turn duration, and cache growth. The predecessor
  filled a 232 GB volume to 96% with no signal; the first symptom would have been a
  confusing build failure.
- The factory must bound its own caches (per-turn build caches, clones, worktrees) and
  report what it reclaimed. Growth without a bound is a defect, not a tuning issue.
  Each declared cache is reclaimed oldest entry first (by newest *file* mtime — a
  directory's mtime is bumped by every walk) until under its bound. **The newest
  entry is never removed**: a single entry larger than the bound would otherwise
  be deleted and rebuilt on every tick; left over the bound, it is a finding.
  **Reclamation deletes, so where it may delete is the first question.** Every
  cache lies inside `paths.cache_root`, a dedicated factory-owned directory that
  may not be `/` and may not overlap the factory root, either repository, a
  credential, or an alert file — refused at load, lexically. At the moment of
  use, **deletion is bound to an opened, verified directory handle, not to a
  path.** The tick opens the cache root without following a final symlink,
  verifies on the handle that it is a directory owned by the factory user and
  writable by nobody else, binds an `os.Root` to it and checks that the bound
  root is the same inode it verified; every listing, sizing and removal is then
  relative to that handle and refuses to follow a symlink out of it. Nothing an
  untrusted writer does to the path afterwards — a symlink at the cache, at the
  root, or at the root's renamed parent — changes what is deleted, because the
  path is not consulted again. A resolved-path comparison would not give this:
  a root swapped for a symlink resolves *consistently* under the victim, and
  containment against a moved root passes. A symlink entry is removed as a
  link, never followed. `doctor` performs the same open-and-verify; it
  establishes doctor's environment, and the tick repeats it at every deletion.
- **"Could not look" is exit 3, not a finding.** `Tick` returns a typed
  observation error alongside the report it still wrote; the CLI maps it to 3.
  An unhealthy factory and a blind tick have different remedies.
- **A corrupt or unwritable state document must not silence the tick.** The
  cadence record lives there; when it is unavailable a fail-safe alert goes out
  that depends on nothing but the transports, carrying every finding, bounded by
  the previous `health.json`'s record of when it last did so — and
  unconditionally when that too is unreadable. Alerting on every tick is the
  bounded failure; alerting never is the one v1 had.
- Disk headroom is checked **once per volume**, over every path the factory
  writes: root, both repositories, the gate's declared paths, the caches, the
  alert files. A path that does not exist yet is checked through its nearest
  existing ancestor, because that is the volume it will land on.

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

Implementation notes (step 7):

- **"Needs me" and "working" are separate questions.** A gated change or a
  waiting question is the factory waiting on the operator *by design*; it is
  working, and it needs you. A provider that status cannot reach says nothing
  about the factory; the open changes are shown as *unknown*, with the last good
  list and its time beside the error, never blanked — "the provider is down" must
  not read as "nothing is open". A dead or halted supervisor, a stop sentinel, a
  missing or stale health document, or a state document status cannot read: not
  working.
- **What status could not read is shown, not hidden.** A page for a factory whose
  state document is unreadable must not look like a page for an idle factory.
- **Read-only, and shown to be.** Every file under the factory root is byte- and
  mtime-identical after collecting and after serving both endpoints; the provider
  sees nothing but a listing; any method but GET/HEAD is refused. There is no
  control surface to mistake for one.
- **The provider is asked once per health interval, not once per page load.** A
  reload is not a reason for an API call. `--provider=false` runs status with no
  credential at all, and says so on the page.
- **Liveness is the process, not the record.** The supervisor's handle is checked
  against the live process (start token), and the real process tree under it is
  shown from `/proc`, so "supervising" and "supervising a turn that is actually
  running `claude`" are different lines.
- One-shot `factoryd status --config f` prints the same document as text and
  exits 0 working / 1 not; `--json` prints the endpoint's document; `--serve`
  hosts both. Several `--config` flags make one page.
- **The page is unauthenticated, so nothing that reaches it may carry anything a
  process said about itself.** argv routinely holds tokens; `/proc/<pid>/comm` is
  writable by the process (`prctl(PR_SET_NAME)`); the executable path is a copy
  of a binary named anything. A child holding a credential can encode it through
  any of them, and the page would publish it. The tree therefore shows **pid and
  structure only**, labelled from what factoryd itself recorded: `supervisor` for
  the pid in the state document, `turn` for the pid the supervisor recorded for
  the running turn, `child` for everything else. The supervisor is shown as pid
  and start time, never as the `proc.Ref` whose descriptive `Command` is its own
  argv. `--serve` warns when the address actually **bound** — not the flag, which
  may be a hostname — is not loopback.
- **Absent and unreadable are different answers.** No health document means the
  tick never ran; a health document that exists and cannot be read or parsed is
  named as such, is an error the page shows, and means *not working* — it must
  not read as "the tick never ran here".
- **The provider throttle keys on the last attempt, not the last good answer.**
  After a failed refresh the last good list's time is already older than the
  TTL; a throttle keyed on it would ask the provider on every reload — precisely
  while the provider is down.

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

### 9.1 What must be shown to fail

A requirement with no demonstrated failure is a wish. Each row below is a
condition an implementation must be able to *produce*, and a check that must go
red when it does. Nothing here is satisfied by a passing suite alone.

| requirement | must be shown to fail when | positive control |
|---|---|---|
| §4.4 git identity (https) | an ambient `credential.helper` resolves the remote to a non-producer account | the same check passes with the producer credential configured |
| §4.4 git identity (ssh) | an agent, default key, or `~/.ssh/config` `IdentityFile` authenticates as a non-producer account | passes with `-F /dev/null`, `IdentitiesOnly` and the producer key |
| §4.4 identity is undecidable | the helper returns nothing, or the greeting cannot be parsed | a resolvable transport reports a login |
| §4.4 credential never in argv | the credential appears in the spawned git process's `/proc/<pid>/cmdline` | **the credential is present in the channel it should use** — otherwise "absent from argv" passes for a submit that never supplied it at all |
| §4.4 `fetch` is covered too | an ambient credential is used for fetch, not only push | fetch succeeds under the producer credential |
| §5.4 URL rewrite | `url.<base>.insteadOf` redirects the configured remote → the effective URL no longer matches, refuse | the same check passes with no rewrite in force |
| §5.4 push rewrite | `pushInsteadOf` redirects only the push URL | `get-url --push` matches when unrewritten |
| §5.4 config isolation | a global or system git config that would redirect or re-credential is **not** in force during any git operation | the explicit configuration submit sets *is* in force |
| §5.4 remote vs project | `git.remote` addresses a different repository than the provider block names → refuse | matching remote and provider block passes |
| §5.3 `GitCredential` | either driver returns a username its provider will not accept | **both** drivers exercised, each asserting its own convention |
| §4.4 gate env is owned | any variable reaches the gate that `gate.env` did not declare, or `gate.env` omits `PATH` | the declared environment reaches the gate intact |
| §4.4 gate env agreement | `doctor` and `submit` resolve a `${VAR}` path to different values — including when run from **different processes with different ambient environments**, which is the case that made an allowlist unworkable | both resolve it to the same path from the same config |
| §5.4 local config allowlist | **any** key outside the allowlist is present in the workdir's local config → refuse | a clone carrying only allowlisted keys pushes normally |
| §5.4 the URL check is not enough | `http.<url>.proxy`, `http.sslVerify=false` or `http.sslCAInfo` is set — the effective URL and project still match, and submit must still refuse | the same push succeeds once the setting is removed |
| §5.4 ambient proxy env | `https_proxy`/`all_proxy` set in the parent environment routes nothing: identity, target, fetch and push are all unaffected, because the git process never receives them | `git.proxy` declared in config **does** take effect — proving the transport honours a proxy when one is actually declared, so "unaffected" is not a transport that ignores proxies entirely |
| §5.4 ambient ssh config | a `ProxyCommand` in `~/.ssh/config` (or the system ssh config) is never executed by any factoryd ssh invocation | the same `ProxyCommand` in a config passed without `-F /dev/null` executes — proving the probe can detect execution at all |
| §5.4 host-key policy | a remote presenting a key absent from `git.known_hosts_file` is refused, not learned — **including when that key is present in `/etc/ssh/ssh_known_hosts`**, which is the case `-F /dev/null` alone does not cover | the pinned key connects |
| §5.4 host-key set is fixed | the remote offers additional host keys during the session and ssh records none of them | `ssh -G` on the factoryd invocation reports `updatehostkeys no` and `globalknownhostsfile /dev/null`, so the assertion is about the options actually in force, not the ones intended |
| §4.4 the boundary holds | the producer principal **can** write `paths.submit_repo` → `doctor` fails | the same probe succeeds against `paths.producer_workdir`, so "cannot write" is not a broken probe |
| §4.4 no boundary at all | the producer runs with submit's own authority → `doctor` fails rather than reporting a separation it cannot enforce | a genuinely separated pair passes |
| §4.4 the race is gone | `.git/config` in the **producer workdir** is replaced — with a proxy, a rewrite, anything — at any moment, including after every check and while the push is running: the push is unaffected, because git never reads that file | the identical setting placed in `paths.submit_repo`'s config **does** refuse, proving the test can detect it at all |
| §4.4 control data never crosses | a `.git` anywhere in the producer tree, at any depth, is copied into the submit repository | an ordinary source tree copies intact |
| §4.4 symlink escape | a symlink in the producer tree resolving outside it is followed | a symlink within the tree is preserved |
| §4.4 symlink into `.git` | a link whose recreated target passes through `.git` (`hooks -> .git/hooks`, `x -> ../.git/config`) is copied | an in-tree link to a plain file is preserved |
| §4.4 gate identity | the gate user can write `submit_repo/.git`, or can read either credential file → refuse | the gate user can write each declared path, so "cannot" is not a gate that cannot do anything |
| §4.4 check-to-push race | a draft is marked ready **while the gate runs** → no push at all, nothing opened, nothing closed | the identical run with no flip pushes exactly once, non-force, to the content-derived branch |
| §4.4 unknown owner is not ours | a change in the family with **no author**, or a producer with **no login** → refuse before the gate | the same change carrying the producer's login is accepted and updated |
| §4.4 supersession never writes | the earlier draft goes ready the instant **after submit's last read of it** → `Close` is never invoked; the new content still opens as its own draft naming the old one | the old draft *is* named in the new body and the result — so "never closed" is not a supersession that never happened |
| §7 dead supervisor | the supervisor handle no longer refers to a live process and the role did not halt → finding | the same handle alive → no finding; a halted role is `halted`, not `supervisor_dead` |
| §7 liveness unknown | `/proc` cannot answer → finding, not "alive" | — the positive control is the alive case above |
| §7 disk headroom | a volume below the headroom → one finding **per volume**, not per path on it | the same paths on two volumes → two observations, one finding |
| §7 a volume the tick cannot see | `statfs` fails → the tick is **unhealthy** with a tick error | the same volume readable → healthy |
| §7 bounded cache | a cache over its bound → the oldest entry is removed and **reported**; the newest is never removed and a cache still over bound is a finding | a cache under its bound after reclamation is not a finding |
| §7 cadence | alert only after `alert_after` ticks; silence until `repeat_seconds`; one recovery; none for a condition never announced | the sequence of alert lines is alert, repeat, recovered — exactly three |
| §7 delivery probe | `doctor` fails when the alert file cannot be appended, or the alert command exits non-zero — even when a second transport succeeds, which is named | a healthy run leaves the probe line **in the file** |
| §7 fan-out | one transport failing does not suppress the next, and the failure is named | every transport failing is an error that names each |
| §7 reclamation scope | a cache root of `/`, one overlapping the factory root, a repository, a credential or an alert file, or a cache outside the root → refused at load; a cache path or entry that **resolves** outside the root at reclamation time → nothing deleted, `cache_unsafe` | a sibling that merely shares a string prefix is accepted; a cache inside the root is reclaimed |
| §7 deletion follows the handle | the cache root is replaced by a symlink to a victim **after** it was opened and verified, before anything is deleted → the real root is reclaimed and the victim is intact, links included; the root swapped **between** the no-follow open and the bind → refused | the same root untouched reclaims exactly the oldest entry |
| §8 read-only | every file under the factory root is byte- and mtime-identical after collecting and serving, with triggers, a question and a stop sentinel present to tempt consumption; POST is refused | the page and the JSON rendered, the provider was asked once |
| §8 needs me | a dead supervisor, a halt, a stop sentinel, never supervised, an operator-gated verdict, a waiting question, no / stale / unhealthy health, an unreachable provider, an unreadable state document → each named | a healthy factory has an empty list and reads *working* |
| §8 provider down | a failed refresh keeps the last good list visible with its own time beside the error | a refresh after the TTL is made; a reload inside it is not |
| §8 throttle after failure | a failed refresh followed by reloads inside the TTL → the provider is asked **no** further time; after the TTL, once | — the positive control is the refresh after the TTL |
| §8 unreadable health | a health document that is not JSON, or cannot be read → named, an error, *not working*, and not "the tick never ran" | an absent document reads as absent |
| §8 no process-supplied label | a secret planted in the supervisor's recorded argv, in a live process's arguments, in its **comm** (rewritten by the process) and in its **executable path** (a copy named after the secret) appears in none of HTML, JSON or text | the processes **are** shown, by pid, labelled `turn` and `child` from factoryd's own records |
| §4.4 no-follow crossing | a control file symlinked to a credential, outside or **inside** the tree → refused unread: no commit, no draft, no push, no secret in any log or error; a fifo control file → refused; a detached child that swaps a source file for a credential link after a clean exit → submit refuses, nothing committed or pushed, nothing copied | an ordinary tree copies and submits |
| §4.4 leftover turn | a leader exits 0 with a child still running — holding its stdio, or silent — → the child is killed, the turn is reported leftover, nothing follows it, it counts as a failure | a quiescent turn is not leftover |
| §4.4 producer holds no credential | the producer identity can read either credential file → `doctor` fails; a probe that cannot read even the producer's own workdir → `doctor` fails as an unproved boundary | the producer can read its workdir and neither token |
| §6.4 the gate refuses | a deny path (either name of a rename), held content on an added line, an escalate path with no audit / a `BROKEN` audit / an audit on another head / an audit that tried nothing, a moved head, a closed change, an empty diff, a provider refusal → `merged` is refused **before** any merge call (the provider's own refusal excepted), no verdict file is written | a mergeable change is readied, merged with the expected head, verified, and recorded in file, state and comment |
| §6.4 refuse, not downgrade | an operator-only result refuses; the recorded verdict is never one the gate substituted | the reviewer's own `operator-gated` signal records without merging |
| §6.4 hold is on additions | a removed key and a `+++` header do not hold; an added key does | — |
| §6.4 policy compiles | a pattern that does not compile, or is empty, or a config with no scope block → refused at load | the example policy loads |
| §6.4 no self-merge | the reviewer's stable id equals the author's, or the author's id is unknown → every verdict refused; compared on ids, so a login collision does not pass | distinct ids record and merge |
| §6.4 audit authorship | an audit posted by the change's author, by a third party, or with no authenticated author → ignored and named, neither clearing nor vetoing; a body that claims `posted_by` yields no author at all, and both drivers set it from the provider's comment author | the reviewer's own audit on the exact head counts beside a producer's forged ones; the reviewer's own `BROKEN` refuses |
| §6.4 no auto-ready | a `merged` signal on a draft is refused with the explicit step; the gate never calls `SetDraft` | a change already ready merges |
| §6.4 incomplete diff | a collapsed / too-large / patch-less changed file with hold rules configured → refused, not read as "nothing added"; both drivers flag it. A **rename with a blank patch** is incomplete unless proved path-only: GitHub by zero additions and deletions, GitLab by equal blob ids at the old path on the base and the new path on the head — a differing blob, a failed lookup, or missing diff refs is content not delivered | with no hold rules, path policy still decides; the recorded pure rename on both providers is reported complete **by proof** |
| §7 root replaced after doctor | the root itself, or its renamed parent, replaced by a symlink after `doctor` passed → `cache_unsafe`, nothing deleted | — |
| §7 symlink entry | a symlink entry is removed as a link; its target is intact | the link itself is gone and counted |
| §7 could not look | a volume that will not stat, a provider that will not answer, a corrupt state document → `factoryd health` exits **3** | findings alone exit 1; a live supervisor exits 0 |
| §7 state unavailable | a corrupt `state.json` with a working transport → a `state_unavailable` alert on the first tick, none inside `repeat_seconds`, one after it, and one immediately when no previous `health.json` exists | a readable document alerts on the ordinary cadence |
| §4.4 gate is its own user | `gate.run_as` is empty, or names the producer's user → refuse at load | distinct users pass |
| §4.4 turn env is owned | a reviewer credential present in the supervisor's environment reaches the producer's turn | the reviewer's turn receives that same variable, by name |
| §4.4 declared `PATH` | the gate or turn command is found on doctor's own `PATH` but not on the declared one → doctor refuses; the turn refuses to start | found on the declared `PATH` passes |
| §5.4 remote authority | `git.remote` names the right project on a host other than the provider's (or on another port) → refuse at load | the provider's host passes; a GHE `base_url` accepts the GHE host and refuses `github.com` |
| §4.4 gate paths | a declared path exists as a non-directory, or cannot be created → exit **3** | a genuinely red gate still exits **5**, and an absent-but-creatable path is created and passes |
| §4.4 unset `${VAR}` | a referenced variable is unset → refuse | a set variable resolves and passes |
| §4.1 doctor covers the above | each condition above, reached through `doctor` rather than only through unit tests | a healthy factory passes every check |

The fourth row is the one most likely to be got wrong. "No token in argv" is
trivially true of an implementation that never passes the token anywhere, so the
test has to prove the credential reached the intended channel in the same run.

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
