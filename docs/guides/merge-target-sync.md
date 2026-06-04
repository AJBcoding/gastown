# Merge-target sync hygiene

> **TL;DR** — Any commit that lands on `main` without going through the merge
> queue (a cherry-pick, a hotfix, an incident salvage) **must** be propagated to
> `merge-target`. Run `gt sync merge-target` to do it safely.

## Why this matters

The refinery's merge queue is keyed off polecat branches:

- Polecats branch off **`main`**.
- The refinery rebases each MR onto **`merge-target`** before merging.

When `main` and `merge-target` agree, that rebase is a no-op and merges fly
through. When they **diverge** — which happens the moment a commit lands on
`main` without also reaching `merge-target` — every MR is built on one parent
state and rebased onto another. Result: **rebase conflicts on every MR**, and
the queue silently stops draining.

This failure is deceptive. It presents as "queue not draining" or "claim logic
dead," but the real cause is rebase-conflict-on-every-MR from divergence.

### How it bit us (2026-05-03)

An incident chain salvaged work via manual cherry-picks to `main`. Six commits
were cherry-picked; **none propagated to `merge-target`**. Later polecats
branched from `main` (with the cherry-picks) while the refinery rebased onto
`merge-target` (without them). Every MR conflicted on the cherry-picked content.

The one-shot fix was a force resync of `merge-target` to `main`. After it, the
refinery drained all five queued MRs immediately.

## Process discipline (do this every time)

**Whenever you put a commit on `main` outside the merge queue**, immediately
propagate it:

```bash
gt sync merge-target
```

This is cheap, idempotent, and safe to run anytime — after a cherry-pick, on a
schedule, or as a periodic operator check.

## What `gt sync merge-target` does

It fast-forwards `merge-target` up to `main` when (and only when) that is safe:

| Situation | Behavior |
|-----------|----------|
| `merge-target` does not exist | Nothing to sync — integration-branch refinery is off. Succeeds silently. |
| `merge-target` already equals `main` | In sync. Succeeds silently. |
| `merge-target` is strictly behind `main` (every merge-target commit is also on main) | **Fast-forwards** `merge-target` to `main` via a refspec push. The working tree is never touched. |
| `merge-target` has commits **not** on `main` | **Real divergence.** Prints a diagnostic and exits non-zero. Will not discard work automatically. |

The fast-forward is a refspec push (`main:refs/heads/merge-target`), so it
updates the remote branch without checking anything out — no working-tree
mutation, no town-root guard tripped. The push is non-force, so git itself
rejects anything that is not a true fast-forward as a final safety net.

### Useful flags

```bash
gt sync merge-target --dry-run     # Show what would change, push nothing
gt sync merge-target --escalate    # File a HIGH escalation on real divergence
gt sync merge-target --source master  # Non-default source branch
gt sync merge-target --target staging # Non-default merge-target branch name
gt sync merge-target --remote upstream
```

## When divergence is real

If the command reports divergence, **a human must decide**. Inspect what is on
`merge-target` but not on `main`:

```bash
git log origin/merge-target ^origin/main --oneline
```

If those commits are genuinely unwanted (e.g. salvage that already reached
`main` another way), resync after tagging a backup so the old state is
recoverable:

```bash
git tag merge-target-pre-resync origin/merge-target
git push origin origin/main:refs/heads/merge-target --force-with-lease
```

If the commits are real work that belongs on `main`, get them onto `main`
through the normal flow first, then re-run `gt sync merge-target`.
