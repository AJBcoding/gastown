#!/usr/bin/env bash
# rebuild-gt/run.sh — Rebuild gt binary from gastown source if stale.
#
# SAFETY: Only rebuilds forward (binary is ancestor of HEAD) and only
# from main branch. A bad rebuild caused a crash loop (every session's
# startup hook failed, witness respawned, loop repeated every 1-2 min).

set -euo pipefail

log() { echo "[rebuild-gt] $*"; }

# resolve_town_root: locate the Gas Town root (the dir holding mayor/town.json).
#
# Prefers GT_TOWN_ROOT (normally exported to plugin runs) after validating it
# actually points at a town. Falls back to walking up from $PWD for the
# mayor/town.json marker, mirroring internal/workspace.Find in the gt binary.
# The old `$(gt town root)` fallback is DEAD — that subcommand was removed and
# now prints help text to stdout, silently poisoning TOWN_ROOT with garbage.
resolve_town_root() {
  if [ -n "${GT_TOWN_ROOT:-}" ] && [ -f "${GT_TOWN_ROOT}/mayor/town.json" ]; then
    printf '%s\n' "$GT_TOWN_ROOT"
    return 0
  fi
  local dir
  dir=$(pwd)
  while [ -n "$dir" ] && [ "$dir" != "/" ]; do
    if [ -f "$dir/mayor/town.json" ]; then
      printf '%s\n' "$dir"
      return 0
    fi
    dir=$(dirname "$dir")
  done
  return 1
}

# --- Sync --------------------------------------------------------------------

# sync_rig_main: fast-forward the rig's LOCAL main to origin/main so the
# staleness check (which compares the binary to LOCAL main) can see fixes the
# refinery merged to origin/main. Without this, merged fixes sit live on
# origin but the binary never rebuilds, because the binary already matches the
# stale local main (gt-4tb).
#
# Best-effort and conservative: only fast-forwards when the repo is clean, on
# main, and origin/main is a strict descendant of local main. Never forces,
# never creates a merge commit, never touches a diverged or dirty tree. Always
# returns 0 — a failed sync must not abort the rebuild flow.
#
# Args: $1 = rig root directory.
sync_rig_main() {
  local rig_root="$1"

  if [ ! -d "$rig_root" ]; then
    log "sync: rig root $rig_root does not exist, skipping main fast-forward."
    return 0
  fi

  # Only sync a clean tree — never risk clobbering uncommitted local work.
  if [ -n "$(git -C "$rig_root" status --porcelain 2>/dev/null)" ]; then
    log "sync: repo dirty, skipping main fast-forward."
    return 0
  fi

  # Only sync when actually on main (the build source branch).
  local branch
  branch=$(git -C "$rig_root" branch --show-current 2>/dev/null || echo "")
  if [ "$branch" != "main" ]; then
    log "sync: not on main (on '$branch'), skipping main fast-forward."
    return 0
  fi

  # Fetch latest origin/main. Offline / fetch failure is fine — just skip.
  if ! git -C "$rig_root" fetch origin main --quiet 2>/dev/null; then
    log "sync: git fetch failed, skipping main fast-forward."
    return 0
  fi

  local local_main remote_main
  local_main=$(git -C "$rig_root" rev-parse main 2>/dev/null || echo "")
  remote_main=$(git -C "$rig_root" rev-parse origin/main 2>/dev/null || echo "")

  if [ -z "$local_main" ] || [ -z "$remote_main" ]; then
    log "sync: could not resolve main/origin/main, skipping."
    return 0
  fi

  if [ "$local_main" = "$remote_main" ]; then
    log "sync: local main already matches origin/main."
    return 0
  fi

  # Strict fast-forward only: local main must be an ancestor of origin/main.
  # This guards against diverged history — never force, never merge-commit.
  if ! git -C "$rig_root" merge-base --is-ancestor "$local_main" "$remote_main" 2>/dev/null; then
    log "sync: local main diverged from origin/main (not a fast-forward), skipping."
    return 0
  fi

  if git -C "$rig_root" merge --ff-only origin/main --quiet 2>/dev/null; then
    log "sync: fast-forwarded local main $local_main -> $remote_main"
  else
    log "sync: ff-only merge failed unexpectedly, skipping."
  fi
  return 0
}

# --- Main --------------------------------------------------------------------

main() {
  TOWN_ROOT=$(resolve_town_root) || {
    log "FATAL: cannot resolve town root — GT_TOWN_ROOT unset/invalid and no mayor/town.json found walking up from $(pwd)"
    return 1
  }
  RIG_ROOT="${TOWN_ROOT}/gastown/mayor/rig"

  # Bring local main up to date with merged fixes BEFORE the staleness check,
  # otherwise gt stale compares the binary to a stale local main and reports
  # "fresh" even when origin/main has newer fixes (gt-4tb).
  sync_rig_main "$RIG_ROOT"

  # --- Detection -------------------------------------------------------------

  log "Checking binary staleness..."
  local STALE_JSON
  STALE_JSON=$(gt stale --json 2>/dev/null) || {
    log "gt stale --json failed, skipping"
    return 0
  }

  local IS_STALE SAFE
  IS_STALE=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('stale', False))" 2>/dev/null || echo "False")
  SAFE=$(echo "$STALE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('safe_to_rebuild', False))" 2>/dev/null || echo "False")

  if [ "$IS_STALE" != "True" ]; then
    log "Binary is fresh. Nothing to do."
    bd create "rebuild-gt: binary is fresh" -t chore --ephemeral \
      -l type:plugin-run,plugin:rebuild-gt,rig:gastown,result:success \
      --silent 2>/dev/null || true
    return 0
  fi

  if [ "$SAFE" != "True" ]; then
    log "Not safe to rebuild (not on main or would be a downgrade). Skipping."
    bd create "Plugin: rebuild-gt [skipped]" -t chore --ephemeral \
      -l type:plugin-run,plugin:rebuild-gt,rig:gastown,result:skipped \
      -d "Skipped: not safe to rebuild" --silent 2>/dev/null || true
    return 0
  fi

  # --- Pre-flight checks -----------------------------------------------------

  log "Pre-flight checks..."

  if [ ! -d "$RIG_ROOT" ]; then
    log "Rig root $RIG_ROOT does not exist. Skipping."
    return 0
  fi

  local DIRTY
  DIRTY=$(git -C "$RIG_ROOT" status --porcelain 2>/dev/null)
  if [ -n "$DIRTY" ]; then
    log "Repo is dirty, skipping rebuild."
    bd create "Plugin: rebuild-gt [skipped]" -t chore --ephemeral \
      -l type:plugin-run,plugin:rebuild-gt,rig:gastown,result:skipped \
      -d "Skipped: repo has uncommitted changes" --silent 2>/dev/null || true
    return 0
  fi

  local BRANCH
  BRANCH=$(git -C "$RIG_ROOT" branch --show-current 2>/dev/null)
  if [ "$BRANCH" != "main" ]; then
    log "Not on main branch (on $BRANCH), skipping rebuild."
    bd create "Plugin: rebuild-gt [skipped]" -t chore --ephemeral \
      -l type:plugin-run,plugin:rebuild-gt,rig:gastown,result:skipped \
      -d "Skipped: not on main branch (on $BRANCH)" --silent 2>/dev/null || true
    return 0
  fi

  # --- Build -----------------------------------------------------------------

  local OLD_VER NEW_VER ERROR
  OLD_VER=$(gt version 2>/dev/null | head -1 || echo "unknown")
  log "Rebuilding gt from $RIG_ROOT..."

  if (cd "$RIG_ROOT" && make build && make safe-install) 2>&1; then
    NEW_VER=$(gt version 2>/dev/null | head -1 || echo "unknown")
    log "Rebuilt: $OLD_VER -> $NEW_VER"
    bd create "rebuild-gt: $OLD_VER -> $NEW_VER" -t chore --ephemeral \
      -l type:plugin-run,plugin:rebuild-gt,rig:gastown,result:success \
      --silent 2>/dev/null || true
  else
    ERROR="make build/safe-install failed"
    log "FAILED: $ERROR"
    bd create "Plugin: rebuild-gt [failure]" -t chore --ephemeral \
      -l type:plugin-run,plugin:rebuild-gt,rig:gastown,result:failure \
      -d "Build failed: $ERROR" --silent 2>/dev/null || true
    gt escalate "Plugin FAILED: rebuild-gt" -s medium 2>/dev/null || true
    return 1
  fi
}

# Only run when executed directly; allow tests to source the functions above.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
