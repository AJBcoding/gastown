package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/workspace"
)

// townCycleSession is the --session flag for town next/prev commands.
// When run via tmux key binding (run-shell), the session context may not be
// correct, so we pass the session name explicitly via #{session_name} expansion.
var townCycleSession string

// getTownLevelSessions returns the town-level session names for the current workspace.
func getTownLevelSessions() []string {
	mayorSession := getMayorSessionName()
	deaconSession := getDeaconSessionName()
	return []string{mayorSession, deaconSession}
}

// isTownLevelSession checks if the given session name is a town-level session.
// Town-level sessions (Mayor, Deacon) use the "hq-" prefix, so we can identify
// them by name alone without requiring workspace context. This is critical for
// tmux run-shell which may execute from outside the workspace directory.
func isTownLevelSession(sessionName string) bool {
	// Town-level sessions are identified by their fixed names
	mayorSession := getMayorSessionName()  // "hq-mayor"
	deaconSession := getDeaconSessionName() // "hq-deacon"
	return sessionName == mayorSession || sessionName == deaconSession
}

func init() {
	rootCmd.AddCommand(townCmd)
	townCmd.AddCommand(townNextCmd)
	townCmd.AddCommand(townPrevCmd)
	townCmd.AddCommand(townRootCmd)

	townNextCmd.Flags().StringVar(&townCycleSession, "session", "", "Override current session (used by tmux binding)")
	townPrevCmd.Flags().StringVar(&townCycleSession, "session", "", "Override current session (used by tmux binding)")
}

var townCmd = &cobra.Command{
	Use:   "town",
	Short: "Town-level operations",
	Long:  `Commands for town-level operations including session cycling.`,
}

var townNextCmd = &cobra.Command{
	Use:   "next",
	Short: "Switch to next town session (mayor/deacon)",
	Long: `Switch to the next town-level session in the cycle order.
Town sessions cycle between Mayor and Deacon.

This command is typically invoked via the C-b n keybinding when in a
town-level session (Mayor or Deacon).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cycleTownSession(1, townCycleSession)
	},
}

var townPrevCmd = &cobra.Command{
	Use:   "prev",
	Short: "Switch to previous town session (mayor/deacon)",
	Long: `Switch to the previous town-level session in the cycle order.
Town sessions cycle between Mayor and Deacon.

This command is typically invoked via the C-b p keybinding when in a
town-level session (Mayor or Deacon).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cycleTownSession(-1, townCycleSession)
	},
}

// townRootCmd prints the absolute path of the current Gas Town root.
//
// Restored after removal broke shell tooling that used `$(gt town root)` as a
// fallback when GT_TOWN_ROOT was unset: the missing subcommand printed help
// text to stdout, silently poisoning callers' TOWN_ROOT (gt-0rg). Resolution
// honors GT_TOWN_ROOT/GT_ROOT via workspace.FindFromCwdOrError.
var townRootCmd = &cobra.Command{
	Use:   "root",
	Short: "Print the path to the current Gas Town root",
	Long: `Print the absolute path of the current Gas Town root directory
(the directory containing mayor/town.json).

Resolution prefers the GT_TOWN_ROOT/GT_ROOT environment variables and otherwise
walks up from the current directory looking for the workspace marker. Exits
non-zero with a clear message if no town root can be found, so shell callers can
fail loudly rather than capturing garbage.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := workspace.FindFromCwdOrError()
		if err != nil {
			return fmt.Errorf("resolving town root: %w", err)
		}
		fmt.Fprintln(cmd.OutOrStdout(), root)
		return nil
	},
}

// cycleTownSession switches to the next or previous town-level session.
// direction: 1 for next, -1 for previous
// sessionOverride: if non-empty, use this instead of detecting current session
func cycleTownSession(direction int, sessionOverride string) error {
	currentSession, err := resolveCurrentSession(sessionOverride)
	if err != nil {
		return fmt.Errorf("not in a tmux session: %w", err)
	}
	if currentSession == "" {
		return fmt.Errorf("not in a tmux session")
	}

	if !isTownLevelSession(currentSession) {
		return nil
	}

	sessions, err := findRunningTownSessions()
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	return cycleInGroup(direction, currentSession, sessions)
}

// findRunningTownSessions returns a list of currently running town-level sessions.
func findRunningTownSessions() ([]string, error) {
	allSessions, err := listTmuxSessions()
	if err != nil {
		return nil, fmt.Errorf("listing tmux sessions: %w", err)
	}

	townLevelSessions := getTownLevelSessions()
	if townLevelSessions == nil {
		return nil, fmt.Errorf("cannot determine town-level sessions")
	}

	var running []string
	for _, s := range allSessions {
		for _, townSession := range townLevelSessions {
			if s == townSession {
				running = append(running, s)
				break
			}
		}
	}

	return running, nil
}
