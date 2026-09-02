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
- every name in `gate.inherit_env` is set in the environment `doctor` is running
  in (§4.4);
- the target repo is reachable;
- the workdir is a clone, not a worktree (§4.4).

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
3. re-check `gate.required_writable_paths` (below). A missing or unwritable one
   exits **3**, not 5: a misconfigured host is not a red branch (§9.6).
4. run the configured gate command (build/vet/test). If red: write a question
   (§6.2), signal, **do not push**
5. push, create-or-update the Draft PR/MR
6. signal the reviewer

Exit codes are part of the contract: `0` submitted · `4` nothing to submit ·
`5` gate failed · `3` configuration/identity failure.

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
  "env": {"GOCACHE": "/var/cache/factoryd/widgets/go", "GOFLAGS": "-count=1"},
  "inherit_env": ["PATH", "HOME"],
  "required_writable_paths": ["${GOCACHE}", "build/artifacts"],
  "timeout_seconds": 900
}
```

Declared, because a gate command is an opaque argv — often a shell string — and
the paths a build writes to are set by environment variables, tool defaults and
the build system itself. Trying to recover them by parsing the command would be a
guess presented as a check: it would miss paths that matter and invent ones that
do not, and a check that is sometimes wrong about what it verified is worse than
no check, because it is believed.

**The gate's environment is constructed, not inherited.** It has to be, or
`${GOCACHE}` means one thing when `doctor` reads it and another when `submit`
runs — two checks of two different paths, both reporting on the third. The gate
environment is, in order:

1. the `FACTORYD_*` variables submit sets;
2. the variables named in `gate.inherit_env`, taken from the supervisor's
   environment — **an allowlist, so nothing arrives unnoticed**; a name in the
   list that is unset in the supervisor's environment is an error, not an
   omission;
3. `gate.env`, which wins over both.

Nothing else is passed through. Anything the gate needs is named by the
operator, which is what makes the environment the same in `doctor` and in
`submit`: both compute it from the same configuration rather than reading
whatever the process happened to be started with.

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
  "ssh_key_file": "/etc/factoryd/widgets/producer_ed25519"
}
```

`transport` is `https` or `ssh`. For `https` the credential is
`credentials.producer` — the same one the API uses, and no other. For `ssh` it is
`ssh_key_file`, which is required for that transport and ignored otherwise.

**Isolation is part of the contract, not a precaution.** Every git invocation
must run with inherited credential sources suppressed:

- **https:** no ambient `credential.helper` may apply. Submit supplies its own,
  configured explicitly for this invocation, reading the producer credential from
  a file the process owns at mode `0600` and removes, or from stdin. **Never a
  remote URL with the credential embedded, and never argv** —
  `/proc/<pid>/cmdline` is world-readable, so a token on a command line is
  readable by anything on the host.
- **ssh:** `IdentitiesOnly=yes`, `IdentityAgent=none`, and an explicit
  `IdentityFile`. An agent holding the reviewer's key is the same failure as a
  credential helper holding the reviewer's token.

**Pinning the credential is not enough; the target must be pinned too.** A
correct `remote` proves nothing on its own. `url.<base>.insteadOf` and
`pushInsteadOf` rewrite it, `core.sshCommand` and `GIT_SSH_COMMAND` replace the
transport, and `GIT_DIR`, `GIT_ASKPASS` and the system and global config files
all reach in from outside. Every one of those can send a correctly-configured
remote somewhere else, and the push still looks like it worked.

So the git environment is isolated to the same standard as the credential:

- `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` point at `/dev/null`; only
  configuration submit sets explicitly applies.
- Inherited `GIT_*` variables are dropped, not merged. `GIT_TERMINAL_PROMPT=0`
  and an inert `GIT_ASKPASS`, so a missing credential fails rather than waiting
  for a human who is not there.
- The transport is set explicitly, never inherited.

And the effective target is checked, not the configured one:

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

### 9.1 What must be shown to fail

A requirement with no demonstrated failure is a wish. Each row below is a
condition an implementation must be able to *produce*, and a check that must go
red when it does. Nothing here is satisfied by a passing suite alone.

| requirement | must be shown to fail when | positive control |
|---|---|---|
| §4.4 git identity (https) | an ambient `credential.helper` resolves the remote to a non-producer account | the same check passes with the producer credential configured |
| §4.4 git identity (ssh) | an agent or default key authenticates as a non-producer account | passes with `IdentitiesOnly` and the producer key |
| §4.4 identity is undecidable | the helper returns nothing, or the greeting cannot be parsed | a resolvable transport reports a login |
| §4.4 credential never in argv | the credential appears in the spawned git process's `/proc/<pid>/cmdline` | **the credential is present in the channel it should use** — otherwise "absent from argv" passes for a submit that never supplied it at all |
| §4.4 `fetch` is covered too | an ambient credential is used for fetch, not only push | fetch succeeds under the producer credential |
| §5.4 URL rewrite | `url.<base>.insteadOf` redirects the configured remote → the effective URL no longer matches, refuse | the same check passes with no rewrite in force |
| §5.4 push rewrite | `pushInsteadOf` redirects only the push URL | `get-url --push` matches when unrewritten |
| §5.4 config isolation | a global or system git config that would redirect or re-credential is **not** in force during any git operation | the explicit configuration submit sets *is* in force |
| §5.4 remote vs project | `git.remote` addresses a different repository than the provider block names → refuse | matching remote and provider block passes |
| §5.3 `GitCredential` | either driver returns a username its provider will not accept | **both** drivers exercised, each asserting its own convention |
| §4.4 gate env | a name in `inherit_env` is unset in the supervisor's environment → refuse | a set name resolves and passes |
| §4.4 gate env agreement | `doctor` and `submit` resolve a `${VAR}` path to different values | both resolve it to the same path from the same config |
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
