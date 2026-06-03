package dog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWorktreesStale covers the recurring-zombie root cause (gt-y9l): a dog
// whose recorded worktrees point outside the current town root or no longer
// exist on disk must be reported stale so dispatch can heal it before
// assigning work.
func TestWorktreesStale(t *testing.T) {
	t.Run("healthy worktree under town root", func(t *testing.T) {
		m, townRoot := testManager(t)
		// Create a real worktree directory under the dog's kennel dir.
		wt := filepath.Join(m.dogDir("alpha"), "gastown")
		if err := os.MkdirAll(wt, 0755); err != nil {
			t.Fatal(err)
		}
		setupDogWithState(t, m, "alpha", &DogState{
			Name:      "alpha",
			State:     StateIdle,
			Worktrees: map[string]string{"gastown": wt},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})

		stale, reason, err := m.WorktreesStale("alpha")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if stale {
			t.Errorf("expected healthy, got stale: %s", reason)
		}
		_ = townRoot
	})

	t.Run("worktree outside town root (the alpha bug)", func(t *testing.T) {
		m, _ := testManager(t)
		// Mirror the observed alpha state: a path under a town root that no
		// longer exists / differs from the current one.
		setupDogWithState(t, m, "alpha", &DogState{
			Name:      "alpha",
			State:     StateIdle,
			Worktrees: map[string]string{"python419": "/Users/someone/PycharmProjects/gt/deacon/dogs/alpha/python419"},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})

		stale, reason, err := m.WorktreesStale("alpha")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !stale {
			t.Errorf("expected stale (path outside town root), got healthy")
		}
		if reason == "" {
			t.Error("expected a non-empty reason")
		}
	})

	t.Run("worktree missing on disk", func(t *testing.T) {
		m, _ := testManager(t)
		// Path is under the town root but the directory was never created.
		missing := filepath.Join(m.dogDir("bravo"), "gastown")
		setupDogWithState(t, m, "bravo", &DogState{
			Name:      "bravo",
			State:     StateIdle,
			Worktrees: map[string]string{"gastown": missing},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})

		stale, reason, err := m.WorktreesStale("bravo")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !stale {
			t.Errorf("expected stale (missing on disk), got healthy")
		}
		if reason == "" {
			t.Error("expected a non-empty reason")
		}
	})

	t.Run("no recorded worktrees", func(t *testing.T) {
		m, _ := testManager(t)
		setupDogWithState(t, m, "charlie", &DogState{
			Name:      "charlie",
			State:     StateIdle,
			Worktrees: map[string]string{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		})

		stale, _, err := m.WorktreesStale("charlie")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !stale {
			t.Errorf("expected stale (no worktrees), got healthy")
		}
	})

	t.Run("nonexistent dog", func(t *testing.T) {
		m, _ := testManager(t)
		if _, _, err := m.WorktreesStale("ghost"); err != ErrDogNotFound {
			t.Errorf("expected ErrDogNotFound, got %v", err)
		}
	})
}
