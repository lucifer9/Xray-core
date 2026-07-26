---
name: upstream-sync
description: Synchronize a long-lived local Git patch stack with its upstream branch while preserving local features, detecting exact or partial upstream overlap, shrinking absorbed patches, and validating the residual delta. Use when asked to fetch, rebase, merge, compare, or sync upstream changes into this repository without opening a PR.
compatibility: Requires git and a repository with an upstream remote. This repository uses upstream/main, stacked local/* topic branches, dev as the integration branch, and docs/local-patches/tun-network-path-safety.md as the patch ledger.
---

# Upstream Sync

Maintain the smallest behaviorally necessary local delta over the current upstream. Do not preserve a local commit merely because it existed before the sync.

## Required repository model

For this repository, use this dependency order:

```text
upstream/main
└── main
    └── local/dial-controls
        └── local/tun-carrier-policy
            └── local/route-exclusions
                └── local/direct-echo
                    └── dev
```

`main` is a clean upstream mirror. Topic branches form a stacked patch queue. `dev` is the validated integration branch.

Read `docs/local-patches/tun-network-path-safety.md` before changing refs. Treat its behavior units and upstream statuses as the source of truth for what local functionality must remain.

## Safety boundaries

- Read repository instructions, `git status`, tracked diffs, remotes, branch tracking, and the patch ledger before changing refs.
- Stop if tracked worktree or index changes are present unless the user explicitly says how to preserve them.
- Never delete, reset, clean, overwrite, or add unrelated untracked files.
- Never sync directly on the current stable `dev`; use a backup ref and a temporary `sync/<date>` branch or separate worktree.
- Never push, force-push, delete remote branches, or update a deployed/shared branch without explicit approval.
- Keep `main` free of local patches and update it only by fast-forward from `upstream/main`.
- Prefer rebase for this private patch queue. Do not merge upstream into every topic branch.
- Do not use patch-id equivalence as proof of behavioral equivalence.
- Do not keep two implementations, hidden fallbacks, or compatibility bypasses when upstream partially absorbs a feature. Rewrite the local patch to the residual behavior only.

## Phase 1: Inspect and freeze the old state

Run the read-only preflight helper:

```bash
.pi/skills/upstream-sync/scripts/preflight.sh upstream/main dev
```

Record:

- old upstream base;
- old integration tip;
- topic branch tips;
- current tree hash;
- tracked worktree/index status;
- current ledger statuses.

Create a timestamped backup branch before rewriting history:

```bash
backup="backup/dev-before-upstream-sync-$(date +%Y%m%d-%H%M%S)"
git branch "$backup" dev
```

Use a temporary branch or worktree for all integration work:

```bash
sync_branch="sync/upstream-$(date +%Y%m%d-%H%M%S)"
git switch -c "$sync_branch" dev
```

If `dev` is shared or deployed, keep it unchanged until the complete sync branch passes validation.

## Phase 2: Fetch and inspect upstream

Fetch without changing local branches:

```bash
git fetch upstream
```

Inspect upstream changes since the ledger's `Checked against` commit, prioritizing paths touched by local patches:

```bash
git log --oneline <old-base>..upstream/main -- \
  transport/internet \
  features/dns/localdns \
  proxy/tun \
  infra/conf/tun.go
```

Detect exact or near-exact patch candidates:

```bash
git cherry upstream/main dev
git log --left-right --cherry-mark --oneline upstream/main...dev
```

Interpret these only as candidate signals. Different patches may implement the same behavior, and similar patches may omit required lifecycle, error, concurrency, or platform semantics.

## Phase 3: Classify overlap by behavior unit

Use the ledger's issue-level behavior units. For each relevant upstream commit, assign one state:

| State | Meaning | Local action |
|---|---|---|
| `local-only` | Upstream lacks the behavior | Retain the local patch |
| `candidate` | Possible overlap not yet proven | Inspect code and run contract tests |
| `partial` | Upstream implements only part | Remove duplicated hunks and retain a minimal residual patch |
| `absorbed` | Upstream contains the same patch/behavior | Drop the local patch |
| `superseded` | Different upstream design satisfies the full contract | Drop the local implementation |
| `diverged` | Upstream behavior conflicts with required local behavior | Keep an explicit minimal delta and document why |

A feature is absorbed only when its behavior tests and invariants pass without the local implementation. File overlap, API-name similarity, or successful conflict resolution is not enough.

When upstream partially absorbs a commit:

1. split the local commit if it contains multiple behavior units;
2. remove the upstream-provided portion;
3. keep only missing behavior;
4. update tests so they assert the residual contract at the highest stable seam;
5. avoid introducing a second source of truth.

## Phase 4: Rebase the patch stack in dependency order

Rebase or rebuild topics in this order:

1. `local/dial-controls`
2. `local/tun-carrier-policy`
3. `local/route-exclusions`
4. `local/direct-echo`

The first topic rebases onto `upstream/main`; every later topic rebases onto the newly validated parent topic. Use temporary topic refs while working when existing topic branches must remain stable.

For each topic:

1. rebase onto its new parent;
2. review every skipped or conflict-resolved commit;
3. drop fully absorbed behavior;
4. rewrite partially absorbed commits into residual deltas;
5. run that topic's targeted tests before proceeding;
6. update its ledger entries.

Enable recorded conflict reuse, but review every reused resolution:

```bash
git config rerere.enabled true
git config rerere.autoupdate false
```

Never accept a rerere result solely because it applies cleanly.

## Phase 5: Validate behavior and history

Run validation in increasing scope:

1. tests for each changed behavior unit;
2. race tests for controller/carrier concurrency;
3. affected package tests;
4. Linux, Darwin, Windows, Android, iOS, and FreeBSD compile checks where applicable;
5. a minimal smoke test when platform privileges are available.

For this patch stack, the minimum targeted checks are:

```bash
go test ./transport/internet ./features/dns/localdns ./proxy/tun
go test ./infra/conf -run '^TestTunConfig'
go test -race ./transport/internet ./features/dns/localdns ./proxy/tun
```

Compare the old and new patch queues:

```bash
git range-diff <old-base>..<old-tip> upstream/main..<new-tip>
```

Review the final delta:

```bash
git diff --stat upstream/main..<new-tip>
git diff --check upstream/main..<new-tip>
```

Check specifically for duplicated logic, stale compatibility branches, swallowed errors, weakened fail-closed behavior, generated-file drift, and unmentioned public configuration changes.

## Phase 6: Update the ledger and promote

For every behavior unit touched by the sync, update:

- `Upstream status`;
- `Upstream commits`;
- `Checked against`;
- `Residual delta`;
- `Verification`.

Do not store the only mapping in commit messages or Git notes. Commit hashes and patch IDs are secondary identifiers; the behavior unit and its tests are primary.

Promote the validated topic tips and integration branch only after all checks pass. Preserve the backup ref until the user accepts the sync result. Do not push unless explicitly requested.

## Final report

Report:

- old and new upstream bases;
- old backup ref;
- new topic and integration tips;
- absorbed, partial, superseded, and retained behavior units;
- tests and compile checks run;
- unresolved risks or platform checks not run;
- whether any push or remote mutation occurred.
