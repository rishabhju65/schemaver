# Product Requirements

Scope is cut by one question: **does it write to a managed database?**

- **Phase 1 — See.** Connects with a read-only role. Introspects, diffs,
  visualizes, detects drift. Cannot damage anything.
- **Phase 2 — Change.** Authoring, planning, safety gating, approval, execution.
- **Phase 3 — Zero-downtime.** The multi-phase and data-movement work.

No priority tags inside a phase — the phase is the priority. Ordering within a
section is dependency order.

**Phase 1 is shippable when these eight work:** register a database ·
introspect · baseline import · declared-vs-live semantic diff · drift detection ·
fleet dashboard · schema explorer with ER diagram · rendered visual diff.

Of those, introspection is built. The next blocking piece is the semantic diff
engine (§12), which baseline import, drift and the review surface all depend on.

---

# Phase 1 — See

## UI

### 1. Onboarding and connection

- Register a database: host, port, database, role, TLS mode.
- Connection test that names the failure — unreachable, auth rejected, TLS
  refused, database absent — rather than one generic error.
- **Privilege pre-flight.** Verify the role can read `pg_catalog`,
  `pg_stat_activity`, and `pg_locks`, and report the exact missing `GRANT`.
  Phase 1 asks for nothing beyond read.
- **Baseline import.** Point at an existing database, introspect it, generate the
  declared schema, open it as a pull request. Nobody adopts this on an empty
  database; if this screen is bad, nothing else matters.
- Register environments and declare their order.
- Link a repo: URL, branch, schema directory path, credentials.

### 2. Fleet dashboard

- Every registered database: environment, current version, drift status, last
  seen, reachability.
- Status at a glance: in sync / drifted / unreachable.
- Filter and group by environment or tag.

### 3. Schema explorer

- Browse any version: tables, columns, types, indexes, constraints, sequences,
  enums.
- Table detail: columns with type, nullability, default; indexes; foreign keys in
  and out; on-disk size; row estimate.
- **ER diagram** from any version, rendering a *focused subgraph* — a chosen
  table plus neighbours to depth N. A 400-table diagram is decorative.
- Search across object names and types.
- **Blast radius.** Pick a column, see every dependent — foreign keys, indexes,
  constraints.

### 4. Diff and drift views

- **Semantic diff, rendered.** Typed change list grouped and classified:
  additive, destructive, rewriting, lock-heavy, metadata-only. Not a text patch.
- **The same diff overlaid on the ER diagram**, changes highlighted in place.
- Diff any two versions, in either direction: declared vs live, live vs live,
  version vs version.
- Drift list with the semantic diff of each divergence.
- **Adopt drift** — generate the migration that codifies reality into the repo
  and open it as a pull request. (*Revert* is Phase 2: it writes.)

### 5. History — built, verified against PostgreSQL 17

- **Fleet view**: every registered database with its current version, change
  count, open drift and read staleness.
- **Timeline**, fleet-wide or per database, newest first. Failed reads appear as
  entries rather than gaps — a read that failed is an observation.
- **Per-change object delta**: which tables, enums and sequences were added,
  removed or replaced across one transition. This is object-level, not semantic:
  it says *which* object differs, never *how*. That distinction is stated on the
  page rather than left for a reader to discover.
- **Drift view** with what each database was compared against.
- Every change is currently unattributed, because there is no executor — which is
  to say every change arrived from outside schemaver. When migrations can be
  applied, the same timeline gains attribution, and an unattributed entry is
  precisely the definition of drift.
- *Not built:* per-table history filtered across time, and inspecting a full past
  schema. Both are queries over data already stored.

### 5a. Interface

Server-rendered, embedded in the binary, no build step and nothing to install.

- This is a starting point, not the end state. D-002 commits to ER diagrams and
  an interactive semantic diff, and those need a client application. None of this
  is wasted: plain linkable pages stay useful alongside it.
- Staleness is rendered as an age ("3d ago"), never hidden. A view showing only
  "last read" without how long ago presents stale data as current.

### 6. Cross-cutting

- Every long operation is a job: job status and staleness indicators are
  first-class, not retrofits.
- Deep links to a table, a version, a diff. These get pasted into incident
  channels.
- Live updates for introspection and drift.

## Backend

Items marked **built** exist in the repository. **Verified** means exercised
against a live PostgreSQL 17, not merely unit-tested.

One correctness bug was found this way and is worth recording: `format_type`
renders a type as `st` or `myschema.st` depending on the caller's `search_path`,
so the same database read by two connections produced two different
fingerprints — the precise failure the canonical model exists to prevent.
Introspection now reads inside a REPEATABLE READ transaction with an empty
`search_path`, which fixes that and also makes all seven catalog queries observe
one instant.

### 7. Canonical schema model — built

- Engine-neutral representation of every supported object type.
- Deterministic normalization: identical logical schemas serialize to identical
  bytes. Everything downstream depends on this absolutely.
- Content-addressed version identity.

### 8. Introspection — built, verified against PostgreSQL 17

- PostgreSQL only, per D-003. The constraint is *asserted* — one constrained
  column on the instance record — rather than abstracted behind a dialect
  registry that has exactly one implementation.
- Full catalog read into the canonical model.
- Instance enumeration: list every database on a server, then read each
  individually. One unreadable database records its own error rather than
  discarding the whole instance's results.
- Degrade honestly under a restricted role — report what could not be read rather
  than omitting it and calling the result a schema.

### 9. DDL rendering — built, verified against PostgreSQL 17

- Canonical model back to executable DDL, ordered so it applies to an empty
  database top to bottom: types and sequences, then tables, then foreign keys
  once every table exists, then indexes, then comments.
- Foreign keys are emitted separately from `CREATE TABLE` so that circular
  references between tables stay executable.
- Serves both the explorer (show me the DDL behind this table) and baseline
  import (write the declared schema into the repo).

### 10. Keeping observed state current — built, verified against PostgreSQL 17

Two loops, on a job queue.

- **Discovery, per instance.** Re-enumerate databases: new ones appear
  *unmanaged*, because discovery must never imply consent to manage. Vanished
  ones are archived rather than deleted, so their history stays readable.
- **Observation, per managed database.** A cheap probe first: one round trip
  returning a single digest computed inside the engine. Unchanged digest means no
  full read happens at all.
- **A full read on a slower cadence regardless.** The probe only detects changes
  in fields it projects, so any field it fails to cover would otherwise be
  invisible forever. The backstop bounds that blind spot to one cycle. Build the
  backstop before trusting the optimization.
- **Snapshots are written only on change or failure.** An unchanged database
  writes nothing, however often it is polled, so steady-state growth is zero and
  no retention policy is needed. Liveness lives on the database row instead.
- **Staleness is a state, not an absence.** A failed read keeps the last known
  schema and records the error; the interface must show "last read three days
  ago" rather than presenting old data as current.
- Jobs are claimed under a lease with `FOR UPDATE SKIP LOCKED`, so a dead
  worker's job is reclaimed when its lease lapses instead of sitting in `running`
  forever. Failures back off exponentially; scheduling is jittered so databases
  registered together do not poll in lockstep.
- **Many instances are observed simultaneously** — that is the normal case, not
  an advanced one. Work runs across a pool of claimants, capped per instance so
  that a server hosting forty databases never receives forty simultaneous
  connections from us. The cap is evaluated inside the claim query rather than
  held in worker memory, so it holds across worker processes rather than only
  within one, and an instance at capacity is *skipped* rather than waited on — a
  slow or unreachable server never stalls progress on any other.

### 11. Declared schema, via shadow database — built, verified against PostgreSQL 17

- Apply the declared schema to a throwaway database and introspect it with §8.
- Shadow databases are created on schemaver's own instance, never on a managed
  target. Creation time is encoded in the name so a crash between create and drop
  cannot leak databases indefinitely — a sweep reclaims them.
- **No DDL parser is written.** Declared and live schemas normalize through
  identical code, so they cannot disagree because of a parser bug — a stronger
  guarantee than a parser could give, for a fraction of the work.
- Report DDL that fails to apply as a schema error, with the engine's message.

### 12. Semantic diff engine

- Typed change list, not a text patch.
- Classification per change: additive, destructive, rewriting, lock-heavy,
  metadata-only.
- Rename detection: heuristic proposal only. Confirmation is Phase 2, where it
  can be acted on.
- Dependency graph across objects, with topological ordering.
- Cycle detection — circular foreign keys need deferred constraints.

### 13. Drift detection — built, verified against PostgreSQL 17

Detection is fingerprint inequality, so it needed nothing from the diff engine.
*Explaining* a divergence does, and does not exist yet.

- **Two sources of expectation, behaving identically.** A repository's declared
  schema once one is connected, and *another live database* before that. Peer
  comparison is what makes the product useful with two connection strings and no
  repository at all — the second rung of the onboarding ladder — and it is a
  first-class path, not a degraded one.
- **"Nothing to compare against" is distinct from "no drift".** Both yield
  `Drifted false` and they mean opposite things; conflating them would report all
  clear for exactly the databases nobody is watching. Every comparison carries a
  `Comparable` flag and a reason.
- Drift is deduplicated state, not an event stream: a divergence persisting for a
  week is one row with a moving `last_seen`, not 168 rows. Coming back into
  agreement, or moving to a *different* divergence, closes the stale row.
- **Evaluated every cycle, not only on change.** An expectation can move while
  the database stands still — a new declared schema is imported, or the peer is
  migrated — and that is drift arriving with no observation of this database
  changing. The comparison is two stored fingerprints, so it is free.
- Losing the expectation does not close an open divergence: not knowing what
  something should be is not evidence it is fine.
- *Not built:* generating the adopt-migration, and classifying benign versus
  meaningful divergence. Both need the diff engine — until it exists, every
  divergence is reported as meaningful.

### 14. Repository integration

- Fetch and read declared schema and migrations at a ref.
- Open pull requests: baseline import, drift adoption.
- Deploy key and token management.

### 15. Platform

- **Metadata schema — built, verified.** Thirteen tables across two Postgres
  schemas: `schemaver` for derived state, `schemaver_audit` for the append-only
  record. The split makes D-004's two backup classes physical rather than
  aspirational.
- **Credential encryption — built.** AES-GCM with a key from the environment, so
  a leaked database dump yields ciphertext. Each ciphertext records which key
  sealed it, so rotation can find its remaining work.
- **Migration runner — built, verified.** Plain bootstrap for our own tables,
  refusing to proceed if an applied migration was edited afterwards. Becomes the
  first thing the real executor manages once Phase 2 exists.
- One API serving both UI and CLI — no second, divergent surface.
- Single container plus its own PostgreSQL, deployable inside a private network
  per D-005.
- Structured logs, health and readiness endpoints.

# Phase 2 — Change

## UI

### 14. Change authoring

- Edit the declared schema in-browser with validation, or pick up a change from
  an existing pull request.
- **Live plan preview** — the generated migration updates as the schema is
  edited.
- **Rename confirmation.** When the diff shows a drop plus an add resembling a
  rename, ask the author directly. This cannot be inferred reliably from state,
  and guessing wrong destroys a column of data. Mandatory, not a convenience.
- Manual override of the generated migration, permanently marked as hand-edited.
- Save opens a pull request carrying schema and migration together.

### 15. Review and approval

Review happens here, not in the version-control provider, per D-008.

- The Phase 1 diff view, plus the safety report: each finding with severity, the
  rule that fired, why it matters here, how to fix it.
- Generated DDL in execution order, with transaction boundaries visible — and
  visibly marked where a step cannot be transactional, so the reviewer sees where
  atomicity ends (D-006).
- Per-step impact estimate: lock type, expected duration, rows affected, whether
  it blocks reads or writes.
- Approve / request changes / block, with required approvers driven by computed
  risk.
- **Comment threads anchored to a semantic change identity**, not to a line of
  generated SQL. The anchor has to survive regeneration: D-007 regenerates a
  migration whenever an earlier change lands first, so any line-anchored comment
  would be orphaned. This requires the diff engine (§12) to emit stable change
  identities — a hard requirement on it, not a convenience.
- **Resolution driven by the diff, not only by a button.** When the change a
  thread objects to disappears from the diff, the thread can be marked resolved
  by that fact. A line-based reviewer cannot do this, because it has no notion of
  what a change is.
- **Findings and waivers are distinct from comments.** A finding is resolved by
  fixing it or by *waiving* it, and a waiver is a policy decision with
  consequences: who waived which rule, on which environment, and why. Waivers are
  written to the append-only audit record (D-004), never to a comment thread.
- Mentions and notification delivery. Accepted as our problem per D-008's scope
  cut; it is real surface area and is not schema tooling.
- The pull request receives a one-way summary comment with a deep link. No
  two-way synchronisation, ever.

### 16. Rollout and execution

Three gates stand between approval and production, per D-009 through D-012.

- **Shadow proof (D-009), automatic and unwaivable.** The migration is applied to
  a database built at its `from` fingerprint and the result must fingerprint as
  `to`. A mismatch means the migration is wrong, not risky, and blocks it
  outright. Proves the schema outcome only — it holds no data, so it says nothing
  about duration, locks, or rows that would violate a new constraint, and must
  never be presented as evidence of safety.
- **Lower-environment rehearsal (D-010), with expiring evidence.** A successful
  apply below production is required — and counts only while production's
  fingerprint still equals the `from` it was rehearsed at. If production has
  moved, the rehearsal was performed against a different database and is void.
- **Post-conditions (D-011).** Derived for schema changes: the `to` fingerprint,
  computed rather than authored. Hand-written assertions — a query and its
  expected result — are required for data migrations and available for custom
  invariants. No per-statement expected output for DDL; that produces files full
  of the word `ALTER TABLE` and no safety.
- Plan preview per environment before anything is triggered.
- **Live progress**: which task, which step, elapsed, and real percentage where
  the engine exposes it.
- **"What is blocking me."** During a stuck migration, show the blocking
  session — its query, age, and lock. The single most useful thing to hand a
  human at 2am, and almost nothing offers it.
- Cancel that terminates the backend, not just the job row.
- Streamed per-step logs; post-run summary; retry and resume.
- **Revert (D-012), pre-approved.** The revert is generated, shadow-verified and
  approved *with* the migration, so an incident executes an already-reviewed
  artifact rather than authoring one under pressure. Being an ordinary migration,
  its `from` is the original's `to` — so it is refused automatically if anything
  landed on top, which is correct, since reverting beneath a later change is
  unsafe.
- **Irreversibility surfaced at approval time.** Dropping a column or narrowing a
  type destroys data no generated DDL can restore. The review surface says so
  while a human can still choose differently, rather than leaving it to be
  discovered during the incident.
- **Recovery from partial application.** A step that cannot run in a transaction
  can leave the database at neither `from` nor `to`. Re-introspecting establishes
  where it actually is, and a repair path is computed from there — forward to
  `to`, or back to `from`.
- **Revert drift** — apply the migration that undoes an out-of-band change.

### 17. Policy and administration

- Rule catalogue: enable per environment, assign severity.
- **Backtest a ruleset** against the last N migrations before enabling it, so
  nobody turns on a rule that would have blocked everything they shipped.
- Approval policy per environment and risk level.
- Users, and per-environment permissions separating who may approve from who may
  execute.
- Audit log viewer, filterable and exportable.

## Backend

### 18. Migration planner

- Diff to ordered, executable steps.
- **Transaction boundary planning** — which steps may share a transaction and
  which must stand alone. Concurrent index builds cannot run inside one, so the
  planner must model this rather than discover it at runtime.
- Reversibility analysis: generate the down path, or mark the change irreversible
  *with the reason*. The down path is produced at generation time, not when it is
  needed (D-012).
- Online-DDL strategy selection, preferring non-blocking forms.

### 19. Safety policy engine

- Evaluate rules against the plan, not against raw text.
- **Statistics-aware.** Adding an index is free on ten thousand rows and an
  outage on two hundred million. Without live table statistics, rules are
  guessing.
- Severity, and blocking versus advisory, configurable per environment.

### 20. Executor

- Behind an interface per D-005; in-process implementation only.
- **Lock first, then reconcile (D-013) — built; the decision table is tested,
  the lock mechanics have never executed** (nothing has been migrated yet). A session-scoped
  advisory lock in the *target* database, taken without waiting, then live state
  is read and classified against the previous migration. The lock does not
  establish truth; it freezes truth long enough to act on it. Not a lock row — a
  lock table strands itself when its holder dies.
- **Reconciliation is authoritative for completion, the lock is not.** A worker
  that dies mid-statement has its lock released instantly while the database sits
  half migrated, so lock absence proves nothing. Six outcomes: three permit
  proceeding (including self-healing the case where a migration completed before
  we recorded it), three halt.
- **Partial application halts for a human, always.** The step chain identifies the
  exact stopping point, and rolling forward or back are both shadow-verifiable
  before either runs — an informed one-click choice, not archaeology.
- **A held lock names its holder**: pid, statement, running time, wait event, and
  which sessions are blocking it. Degrades to a partial answer when the role
  cannot see other sessions' query text, because a pid is still better than
  nothing.
- **A stuck holder is never killed automatically.** Termination is an explicit
  audited human action; the property preserved is *stuck rather than corrupt*.
- **Two connections per task**: one blocked on the synchronous DDL, one observing
  catalog views. The engine reports nothing to the executing connection, so
  progress must be observed externally.
- Mandatory `lock_timeout` and `statement_timeout`. An `ALTER` queued behind a
  long read blocks every later query on that table; failing fast is the only safe
  default.
- Idempotency keys, attempt counters, retries safe to repeat.
- Recovery for known partial states — a failed concurrent index build leaves an
  invalid index that must be dropped before retrying.
- Reconcile actual state by re-introspecting after an ambiguous failure.

### 21. Job orchestration

- Change Request → Plan → Rollout → Task.
- Durable task state machine; lease-based dead-worker detection so a task never
  sits "running" forever after its worker dies.
- Enforce environment ordering where policy demands it.
- Concurrency limits per database and instance.

### 22. Applied-state tracking

- **A tracking table inside every managed database.** The database must stay
  self-describing: if the control plane is lost, each target still knows its own
  version, and a restored backup reveals its true state.
- Applied version also in the control plane for the cross-database view, with
  explicit reconciliation when the two disagree.
- **Checksum verification** — detect a migration edited after it was applied and
  fail loudly rather than diverging silently.

### 23. Identity, authorization, audit

- Local users and sessions; role-based permissions scoped per environment.
- **Append-only audit log.** Authoritative and irreproducible per D-004 — it
  cannot be rebuilt from the repo or the database, so it is written once and
  never mutated.

### 24. Credentials

- Encrypted at rest, key supplied by the environment.
- Distinct credentials per environment, with no path by which a lower-environment
  credential reaches production.
- Read from an external secret manager rather than storing.

---

# Phase 3 — Zero-downtime

Each of these is a subsystem on the scale of the diff engine. They were
previously buried as sub-bullets, which badly understated them.

### 25. Expand/contract planning

Turn one logical change into the multi-phase sequence that survives a rolling
deploy: add nullable, backfill, constrain, switch, drop.

### 26. Data migrations and backfills

Batched, resumable, with a durable progress cursor. Not DDL, and a different
execution model. Includes hand-authored data migrations, which have no
schema-diff counterpart at all.

### 27. Fleet fan-out

One change applied across many databases or shards, with per-target progress and
partial-failure handling.

---

# Deliberately not building

Recorded so they do not get re-proposed.

- **Compatibility warning** — flagging a change as incompatible with the running
  application requires knowing which version is live. We have no source for that.
  Revisit only if one appears.
- **Drift attribution** — the engine largely cannot say who ran an out-of-band
  DDL. A promise we cannot keep.
- **Saved dashboard views, table annotations, change templates, dark mode,
  keyboard navigation** — things one could build, not requirements.
- **DDL event triggers for push-based change detection.** Technically the right
  answer — instant notification, no polling, and the engine can even name the
  session. Rejected because installing one *writes to the customer's database*,
  which destroys the read-only guarantee that makes Phase 1 adoptable, and
  because creating them requires superuser. Reconsider only as an opt-in Phase 2
  accelerator, never as the default.

---

# Open questions

- **How is the declared schema written?** Plain DDL is familiar and needs no new
  syntax, and the shadow-database approach in §9 assumes it. Confirm before §9.
- **Views, functions, and triggers.** Not yet in the canonical model. Planning
  changes to them is materially harder than for tables.
- **Multi-database change sets.** Does one change ever span two databases, and if
  so is there an atomicity claim? "No" is a legitimate and much simpler answer.
- **Break-glass.** Is there an audited path to apply a change that is not in the
  repo? D-004 currently forecloses it; that deserves its own decision rather than
  being settled as a side effect.
