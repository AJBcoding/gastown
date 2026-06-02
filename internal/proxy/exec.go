package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
	"golang.org/x/time/rate"
)

// roleSegments enumerates the role keywords that may appear as the third
// segment of an extended cert CN ("gt-<rig>-<role>-<name>"). When a CN's
// third-from-last hyphen-delimited segment matches one of these values,
// it is treated as the role and the parts before it are treated as the rig.
// Any other CN (e.g. legacy "gt-<rig>-<name>") falls back to defaultRoleSegment.
//
// The values map onto Gas Town's BD_ACTOR / GT_ROLE convention:
//
//	polecats → <rig>/polecats/<name>
//	crew     → <rig>/crew/<name>
//
// Adding a new role here is sufficient to make that role's certs identity-aware;
// no other changes to the proxy are required.
var roleSegments = map[string]string{
	"polecats": constants.RolePolecat,
	"crew":     constants.RoleCrew,
}

// defaultRoleSegment is the CN-format segment assumed when a legacy cert
// ("gt-<rig>-<name>") is presented. Existing certs were issued exclusively
// for polecats (IssuePolecat is the only issuer), so defaulting to polecats
// preserves the original behavior for the install base.
const defaultRoleSegment = "polecats"

// execRequest is the body for POST /v1/exec.
type execRequest struct {
	Argv []string `json:"argv"`
}

// execResponse is the response for POST /v1/exec.
type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body to prevent a misbehaving client from exhausting memory.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	// Extract identity from client cert CN; produces "<rig>/<role>/<name>".
	identity := extractIdentity(r)

	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if len(req.Argv) == 0 {
		http.Error(w, "argv is empty", http.StatusBadRequest)
		return
	}

	// Validate argv[0] is in the allowlist.
	cmd0 := req.Argv[0]
	if !s.isAllowed(cmd0) {
		http.Error(w, fmt.Sprintf("command not allowed: %q", cmd0), http.StatusForbidden)
		return
	}

	// Validate argv[1] (subcommand) if this command has a subcommand allowlist.
	if subs, ok := s.allowedSubs[cmd0]; ok {
		if len(req.Argv) < 2 {
			http.Error(w, "subcommand required", http.StatusForbidden)
			return
		}
		sub := req.Argv[1]
		if !subs[sub] {
			http.Error(w, fmt.Sprintf("subcommand not allowed: %q %q", cmd0, sub), http.StatusForbidden)
			return
		}
	}

	// Build argv as a copy of req.Argv to avoid mutating the decoded request.
	argv := append([]string(nil), req.Argv...)
	// Use the resolved absolute binary path to prevent PATH hijacking after startup.
	if resolved, ok := s.resolvedPaths[cmd0]; ok {
		argv[0] = resolved
	}

	// Per-client rate limiting: identified by cert CN (or "unknown" if absent).
	rateKey := identity
	if rateKey == "" {
		rateKey = "unknown"
	}
	if !s.limiterFor(rateKey).Allow() {
		s.log.Warn("exec rate limit exceeded", "identity", identity)
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Global concurrency cap: reject immediately if all slots are busy.
	select {
	case s.execSem <- struct{}{}:
		defer func() { <-s.execSem }()
	default:
		s.log.Warn("exec concurrency limit exceeded", "identity", identity)
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return
	}

	execCtx := r.Context()
	if s.execTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, s.execTimeout)
		defer cancel()
	}
	cwd := resolveBdCwd(s.cfg.TownRoot, argv)
	out, errOut, exitCode := runCommand(execCtx, argv, identity, cwd)

	// Audit log (do not log full argv — it may contain tokens or secrets).
	if exitCode == 0 {
		s.log.Info("exec", "identity", identity, "cmd", cmd0,
			"sub", subForLog(req.Argv), "exit", exitCode)
	} else {
		s.log.Warn("exec failed", "identity", identity, "cmd", cmd0,
			"sub", subForLog(req.Argv), "exit", exitCode)
	}

	// The handler always returns HTTP 200 even when the subprocess exits
	// non-zero. This is intentional: the RPC call itself succeeded (the request was
	// well-formed, the command was allowed, and the subprocess ran). The subprocess's
	// outcome is reported in the JSON body via exitCode. Callers must inspect exitCode
	// rather than the HTTP status to determine whether the command succeeded.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(execResponse{
		Stdout:   out,
		Stderr:   errOut,
		ExitCode: exitCode,
	})
}

// subForLog returns a truncated argv[1] if present, otherwise "".
// Used for audit logging to capture the subcommand without logging full argv.
// Truncates to 128 bytes to prevent oversized log lines from exceeding
// go test -json's scanner buffer (64 KiB), which causes CI hangs.
func subForLog(argv []string) string {
	if len(argv) < 2 {
		return ""
	}
	s := argv[1]
	if len(s) > 128 {
		return s[:128] + "..."
	}
	return s
}

// extractIdentity parses the client cert CN "gt-<rig>-<name>" into "<rig>/<name>".
// Uses LastIndex to correctly handle hyphenated rig names (e.g. "gas-town").
func extractIdentity(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	cn := r.TLS.PeerCertificates[0].Subject.CommonName
	return cnToIdentity(cn)
}

// polecatName extracts the polecat name from a CN of the form "gt-<rig>-<name>".
// The last "-" is the rig/name separator, so hyphenated rig names are handled correctly.
// Returns "" if the CN does not match the expected format, or if rig or name is empty.
func polecatName(cn string) string {
	if !strings.HasPrefix(cn, "gt-") {
		return ""
	}
	rest := cn[3:] // strip "gt-"
	idx := strings.LastIndex(rest, "-")
	// idx <= 0: idx < 0 means no separator; idx == 0 means rig is empty.
	if idx <= 0 {
		return ""
	}
	return rest[idx+1:]
}

// cnToIdentity converts a CN to its Gas Town address ("<rig>/<role>/<name>").
// Both CN formats are supported:
//
//	gt-<rig>-<role>-<name> → "<rig>/<role>/<name>"   (extended; role embeds explicitly)
//	gt-<rig>-<name>        → "<rig>/polecats/<name>" (legacy; assumes polecat role)
//
// Returns "" if the CN is malformed.
func cnToIdentity(cn string) string {
	rig, role, name := cnToIdentityParts(cn)
	if rig == "" || name == "" {
		return ""
	}
	return rig + "/" + role + "/" + name
}

// cnToIdentityParts splits a cert CN into its (rig, role, name) components.
//
// The CN format is one of:
//
//	gt-<rig>-<role>-<name>   role ∈ {polecats, crew}; rig may contain "-"
//	gt-<rig>-<name>          legacy; role defaults to "polecats"
//
// Disambiguation: after stripping "gt-" and peeling off <name> (the last
// hyphen-delimited segment), the remaining prefix's last segment is checked
// against the role-segment allowlist. Match → that segment is the role; the
// rig is the prefix before it. No match → the entire prefix is the rig and
// the role defaults to "polecats".
//
// The role return value is the role *segment* used in the BD_ACTOR address
// ("polecats" or "crew"), NOT the role *constant* (RolePolecat/RoleCrew),
// because addresses use the segment form.
//
// Returns ("", "", "") for malformed CNs (no "gt-" prefix, missing rig, or
// missing name).
func cnToIdentityParts(cn string) (rig, role, name string) {
	if !strings.HasPrefix(cn, "gt-") {
		return "", "", ""
	}
	rest := cn[3:] // strip "gt-"
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 {
		// No separator, or rig segment empty.
		return "", "", ""
	}
	name = rest[idx+1:]
	if name == "" {
		return "", "", ""
	}
	prefix := rest[:idx] // <rig> or <rig>-<role>

	// Peel an optional trailing role segment off the prefix.
	role = defaultRoleSegment
	if roleIdx := strings.LastIndex(prefix, "-"); roleIdx >= 0 {
		candidate := prefix[roleIdx+1:]
		if _, ok := roleSegments[candidate]; ok {
			role = candidate
			prefix = prefix[:roleIdx]
		}
	}

	if prefix == "" {
		return "", "", ""
	}
	return prefix, role, name
}

// isAllowed reports whether cmd is in the allowlist.
func (s *Server) isAllowed(cmd string) bool {
	return s.allowed[cmd]
}

// limiterFor returns the rate.Limiter for the given client identity, creating
// one if it does not exist. The limiter is stored in a sync.Map so concurrent
// requests for the same identity safely share a single limiter.
//
// Note: entries are never evicted; each unique CN accumulates ~200 bytes.
// Acceptable for typical deployments (dozens of polecats); consider adding a
// periodic sweep if the server handles thousands of unique certs.
func (s *Server) limiterFor(identity string) *rate.Limiter {
	if v, ok := s.rateLimiters.Load(identity); ok {
		return v.(*rate.Limiter)
	}
	l := rate.NewLimiter(s.rateLimit, s.rateBurst)
	v, _ := s.rateLimiters.LoadOrStore(identity, l)
	return v.(*rate.Limiter)
}

func runCommand(ctx context.Context, argv []string, identity, cwd string) (stdout, stderr string, exitCode int) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// Restrict the subprocess environment to prevent server credentials from
	// leaking into gt/bd calls. The cert-derived identity is then re-injected
	// as the Gas Town identity env vars (BD_ACTOR, GT_ROLE, etc.) so gt/bd
	// scope mail, beads attribution, and role-aware output to the caller —
	// not to whatever role the server happens to be running as (gh#gt-muo).
	env := append(minimalEnv(), identityEnv(identity)...)
	cmd.Env = env
	err := cmd.Run()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			exitCode = exit.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// identityEnv builds the slice of "KEY=value" env entries needed to make gt/bd
// honor the cert-derived caller identity. The slice is empty when identity is
// empty (e.g., a non-mTLS request in a unit test).
//
// GT_PROXY_IDENTITY is retained as a debug breadcrumb so existing audit
// tooling that greps for it keeps working; the load-bearing variables are
// BD_ACTOR, GT_ROLE, GT_RIG, and GT_POLECAT/GT_CREW (see
// internal/cmd/mail_identity.go detectSenderFromRole).
func identityEnv(identity string) []string {
	if identity == "" {
		return nil
	}
	out := []string{"GT_PROXY_IDENTITY=" + identity}

	// identity is "<rig>/<role>/<name>" (post-fix) — split it back into
	// the components AgentEnv expects. A malformed identity (e.g. a future
	// caller bypassing cnToIdentity) yields only GT_PROXY_IDENTITY, which is
	// harmless but won't scope gt/bd.
	rig, roleSegment, name := splitIdentity(identity)
	if rig == "" || roleSegment == "" || name == "" {
		return out
	}
	role, ok := roleSegments[roleSegment]
	if !ok {
		return out
	}
	for k, v := range agentEnvForIdentity(role, rig, name) {
		out = append(out, k+"="+v)
	}
	return out
}

// splitIdentity is the inverse of cnToIdentity for the post-fix format.
// Returns ("", "", "") if identity is not "<rig>/<role>/<name>".
func splitIdentity(identity string) (rig, role, name string) {
	parts := strings.Split(identity, "/")
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

// agentEnvForIdentity returns the identity-scoping env vars gt/bd consult.
// This intentionally avoids config.AgentEnv, which also injects session-level
// settings (Dolt ports, OTEL, NODE_OPTIONS, etc.) that don't apply to a
// single proxied RPC subprocess and would defeat minimalEnv's isolation goal.
func agentEnvForIdentity(role, rig, name string) map[string]string {
	address := fmt.Sprintf("%s/%s/%s", rig, roleSegmentFor(role), name)
	env := map[string]string{
		"GT_ROLE":          address,
		"GT_RIG":           rig,
		"BD_ACTOR":         address,
		"GIT_AUTHOR_NAME":  name,
		"BEADS_AGENT_NAME": fmt.Sprintf("%s/%s", rig, name),
	}
	switch role {
	case constants.RolePolecat:
		env["GT_POLECAT"] = name
	case constants.RoleCrew:
		env["GT_CREW"] = name
	}
	return env
}

// roleSegmentFor returns the address-segment form of a role constant.
// Inverse of roleSegments. Falls back to the constant itself for unknown
// roles (defensive; cnToIdentityParts only emits known roles).
func roleSegmentFor(role string) string {
	for seg, r := range roleSegments {
		if r == role {
			return seg
		}
	}
	return role
}
