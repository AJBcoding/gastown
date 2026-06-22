#!/usr/bin/env bash
# Tests for rebuild-gt/run.sh sync_rig_main() (gt-4tb).
#
# Verifies the local-main fast-forward is safe: it advances when origin/main is
# strictly ahead, and refuses to touch a dirty, off-main, or diverged tree.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0

# Source run.sh for its functions; the BASH_SOURCE guard keeps main() from running.
# shellcheck source=run.sh
source "$SCRIPT_DIR/run.sh"

# Quiet the log() output from run.sh during tests.
log() { :; }

TMP_ROOT=$(mktemp -d)
cleanup() { rm -rf "$TMP_ROOT"; }
trap cleanup EXIT

git_quiet() { git -C "$1" "${@:2}" >/dev/null 2>&1; }

# make_clone NAME -> creates an origin repo + a clone, prints the clone path.
# The clone has one shared commit on main, checked out and tracking origin.
make_clone() {
  local name="$1"
  local origin="$TMP_ROOT/$name-origin"
  local clone="$TMP_ROOT/$name"

  git init --quiet --bare "$origin"

  git init --quiet "$clone"
  git_quiet "$clone" config user.email "test@gastown.test"
  git_quiet "$clone" config user.name "test"
  git_quiet "$clone" remote add origin "$origin"
  git_quiet "$clone" checkout -b main
  echo "v1" > "$clone/file.txt"
  git_quiet "$clone" add file.txt
  git_quiet "$clone" commit -m "c1"
  git_quiet "$clone" push -u origin main

  echo "$clone"
}

# advance_origin CLONE -> pushes a new commit to origin/main from a scratch
# checkout, leaving CLONE's local main behind (simulating a refinery merge).
advance_origin() {
  local clone="$1"
  local scratch="$TMP_ROOT/scratch-$$-$RANDOM"
  git clone --quiet "$clone/../$(basename "$clone")-origin" "$scratch"
  git_quiet "$scratch" config user.email "test@gastown.test"
  git_quiet "$scratch" config user.name "test"
  git_quiet "$scratch" checkout main
  echo "v2" > "$scratch/file.txt"
  git_quiet "$scratch" commit -am "c2"
  git_quiet "$scratch" push origin main
  rm -rf "$scratch"
  # Refresh CLONE's remote-tracking ref without moving local main.
  git_quiet "$clone" fetch origin
}

rev() { git -C "$1" rev-parse "$2" 2>/dev/null; }

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

echo "=== sync_rig_main tests ==="

# 1. Happy path: origin ahead, clean, on main -> local main fast-forwards.
clone=$(make_clone ff)
advance_origin "$clone"
before=$(rev "$clone" main)
sync_rig_main "$clone"
after=$(rev "$clone" main)
remote=$(rev "$clone" origin/main)
if [ "$after" = "$remote" ] && [ "$after" != "$before" ]; then
  pass "fast-forwards local main to origin/main when strictly ahead"
else
  fail "expected ff to origin/main ($remote), got $after (was $before)"
fi

# 2. Already up to date: no error, main unchanged.
clone=$(make_clone uptodate)
before=$(rev "$clone" main)
sync_rig_main "$clone"
after=$(rev "$clone" main)
if [ "$after" = "$before" ]; then
  pass "no-op when local main already matches origin/main"
else
  fail "expected main unchanged ($before), got $after"
fi

# 3. Dirty tree: refuses to sync even though origin is ahead.
clone=$(make_clone dirty)
advance_origin "$clone"
echo "uncommitted" > "$clone/file.txt"
before=$(rev "$clone" main)
sync_rig_main "$clone"
after=$(rev "$clone" main)
if [ "$after" = "$before" ]; then
  pass "refuses to fast-forward a dirty working tree"
else
  fail "expected main unchanged on dirty tree ($before), got $after"
fi

# 4. Off main: on a feature branch, never touches it.
clone=$(make_clone offmain)
advance_origin "$clone"
git_quiet "$clone" checkout -b feature
before=$(rev "$clone" feature)
sync_rig_main "$clone"
after=$(rev "$clone" feature)
mainrev=$(rev "$clone" main)
if [ "$after" = "$before" ] && [ "$mainrev" != "$(rev "$clone" origin/main)" ]; then
  pass "refuses to sync when not on main"
else
  fail "expected no change off-main; feature $before->$after, main=$mainrev"
fi

# 5. Diverged: local main has a commit origin doesn't -> not a fast-forward, skip.
clone=$(make_clone diverged)
advance_origin "$clone"
echo "local-only" > "$clone/local.txt"
git_quiet "$clone" add local.txt
git_quiet "$clone" commit -m "local-divergent"
before=$(rev "$clone" main)
sync_rig_main "$clone"
after=$(rev "$clone" main)
if [ "$after" = "$before" ]; then
  pass "refuses to fast-forward a diverged local main"
else
  fail "expected diverged main unchanged ($before), got $after"
fi

# 6. Missing rig root: returns cleanly without error.
if sync_rig_main "$TMP_ROOT/does-not-exist" >/dev/null 2>&1; then
  pass "returns 0 when rig root is missing"
else
  fail "expected clean return for missing rig root"
fi

echo ""
if [ "$FAILURES" -gt 0 ]; then
  echo "FAILED: $FAILURES test(s) failed"
  exit 1
else
  echo "PASSED: all tests passed"
fi
