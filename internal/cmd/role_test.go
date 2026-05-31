package cmd

import (
	"path/filepath"
	"testing"
)

func TestResolveCallerIdentity_Proxied_TrustsEnvOverCwd(t *testing.T) {
	// Simulate a proxied invocation: cwd is the proxy server's cwd (mayor's
	// home), but the cert-derived env vars say we're a remote crew member.
	// The fix (gt-c2v) is that env wins in proxied mode, so we resolve to
	// the crew identity — not whatever mayor-shaped cwd the server runs from.
	townRoot := t.TempDir()
	serverCwd := filepath.Join(townRoot, "mayor")

	t.Setenv("GT_PROXY_IDENTITY", "Indigo/securityspy")
	t.Setenv("GT_ROLE", "Indigo/crew/securityspy")
	t.Setenv("GT_RIG", "Indigo")
	t.Setenv("GT_CREW", "securityspy")
	t.Setenv("GT_POLECAT", "") // crew certs do not get GT_POLECAT

	ctx, err := resolveCallerIdentity(serverCwd, townRoot)
	if err != nil {
		t.Fatalf("resolveCallerIdentity: %v", err)
	}
	if ctx.Role != RoleCrew {
		t.Errorf("Role = %q, want %q", ctx.Role, RoleCrew)
	}
	if ctx.Rig != "Indigo" {
		t.Errorf("Rig = %q, want %q", ctx.Rig, "Indigo")
	}
	if ctx.Polecat != "securityspy" {
		t.Errorf("Polecat = %q, want %q", ctx.Polecat, "securityspy")
	}

	// And the assembled identity matches what gt prime + gt mail inbox report,
	// so gt hook / gt mol current stop disagreeing with the rest of the suite.
	if got := buildAgentIdentity(ctx); got != "Indigo/crew/securityspy" {
		t.Errorf("buildAgentIdentity = %q, want %q", got, "Indigo/crew/securityspy")
	}
}

func TestResolveCallerIdentity_LocalShellPrefersCwd(t *testing.T) {
	// Non-proxied (no GT_PROXY_IDENTITY): a user with a stale GT_ROLE in their
	// shell cd's into another agent's worktree. Cwd wins so they see the hook
	// for the directory they're actually in (gt-5d7eh).
	townRoot := t.TempDir()
	daveDir := filepath.Join(townRoot, "beads", "crew", "dave")

	t.Setenv("GT_PROXY_IDENTITY", "")
	t.Setenv("GT_ROLE", "gastown/crew/joe")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_CREW", "joe")

	ctx, err := resolveCallerIdentity(daveDir, townRoot)
	if err != nil {
		t.Fatalf("resolveCallerIdentity: %v", err)
	}
	if ctx.Role != RoleCrew {
		t.Errorf("Role = %q, want %q", ctx.Role, RoleCrew)
	}
	if ctx.Rig != "beads" {
		t.Errorf("Rig = %q, want %q (cwd should win)", ctx.Rig, "beads")
	}
	if ctx.Polecat != "dave" {
		t.Errorf("Polecat = %q, want %q (cwd should win)", ctx.Polecat, "dave")
	}
}

func TestResolveCallerIdentity_LocalShellFallsBackToEnvWhenCwdUnknown(t *testing.T) {
	// Non-proxied, cwd doesn't identify an agent (e.g. at a rig root). Env
	// vars fill in. Preserves the second branch of the original molecule_status
	// logic.
	townRoot := t.TempDir()
	rigRoot := filepath.Join(townRoot, "gastown")

	t.Setenv("GT_PROXY_IDENTITY", "")
	t.Setenv("GT_ROLE", "gastown/witness")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_CREW", "")
	t.Setenv("GT_POLECAT", "")

	ctx, err := resolveCallerIdentity(rigRoot, townRoot)
	if err != nil {
		t.Fatalf("resolveCallerIdentity: %v", err)
	}
	if ctx.Role != RoleWitness {
		t.Errorf("Role = %q, want %q", ctx.Role, RoleWitness)
	}
	if ctx.Rig != "gastown" {
		t.Errorf("Rig = %q, want %q", ctx.Rig, "gastown")
	}
}

func TestIsProxiedInvocation(t *testing.T) {
	t.Setenv("GT_PROXY_IDENTITY", "")
	if isProxiedInvocation() {
		t.Error("isProxiedInvocation() = true when GT_PROXY_IDENTITY is unset")
	}
	t.Setenv("GT_PROXY_IDENTITY", "Indigo/securityspy")
	if !isProxiedInvocation() {
		t.Error("isProxiedInvocation() = false when GT_PROXY_IDENTITY is set")
	}
}
