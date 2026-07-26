#!/usr/bin/env bash
set -euo pipefail

upstream_ref=${1:-upstream/main}
local_branch=${2:-dev}

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "error: not inside a Git worktree" >&2
  exit 2
fi

if ! git rev-parse --verify "$upstream_ref^{commit}" >/dev/null 2>&1; then
  echo "error: upstream ref does not exist: $upstream_ref" >&2
  exit 2
fi

if ! git rev-parse --verify "$local_branch^{commit}" >/dev/null 2>&1; then
  echo "error: local branch/ref does not exist: $local_branch" >&2
  exit 2
fi

tracked_dirty=false
if ! git diff --quiet || ! git diff --cached --quiet; then
  tracked_dirty=true
fi

base=$(git merge-base "$upstream_ref" "$local_branch")
upstream_tip=$(git rev-parse "$upstream_ref")
local_tip=$(git rev-parse "$local_branch")
local_tree=$(git rev-parse "$local_branch^{tree}")

printf 'Repository: %s\n' "$(git rev-parse --show-toplevel)"
printf 'Current branch: %s\n' "$(git branch --show-current || true)"
printf 'Upstream ref: %s (%s)\n' "$upstream_ref" "${upstream_tip:0:12}"
printf 'Local ref: %s (%s)\n' "$local_branch" "${local_tip:0:12}"
printf 'Merge base: %s\n' "$base"
printf 'Local tree: %s\n' "$local_tree"
printf 'Tracked changes present: %s\n' "$tracked_dirty"

printf '\nRemotes:\n'
git remote -v

printf '\nTopic branches:\n'
for branch in \
  local/dial-controls \
  local/tun-carrier-policy \
  local/route-exclusions \
  local/direct-echo \
  dev \
  main; do
  if git show-ref --verify --quiet "refs/heads/$branch"; then
    printf '%-32s %s  %s\n' \
      "$branch" \
      "$(git rev-parse --short "$branch^{commit}")" \
      "$(git log -1 --format=%s "$branch" --)"
  else
    printf '%-32s missing\n' "$branch"
  fi
done

printf '\nWorking tree status (untracked files are reported but not treated as tracked dirt):\n'
git status --short --branch

printf '\nPatch-equivalence candidates:\n'
git log --left-right --cherry-mark --oneline "$upstream_ref...$local_branch" || true

printf '\nLocal patch paths since merge base:\n'
git diff --name-only "$base..$local_branch"

if [ "$tracked_dirty" = true ]; then
  echo >&2
  echo "error: tracked worktree or index changes are present; preserve or resolve them before synchronization" >&2
  exit 3
fi
