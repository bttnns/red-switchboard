// Package cli is the redswitchboard command tree, built on cobra. One binary
// groups every flow (run, inspect, discover) behind a single root command. Each
// command lives in its own file as a newXCmd() constructor that wires its flags
// and RunE. Logging in to a vendor (to mint the creds file a source reads) is
// intentionally NOT here: that lives in separate tools (Rivian:
// github.com/bttnns/rivian_auth; Tesla: github.com/adriankumpf/tesla_auth). At
// runtime a source only refreshes its short-lived session tokens; it never
// performs a credentialed login.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is the redswitchboard build version (overridable via -ldflags).
var version = "0.1.0"

// Command groups shown in `redswitchboard --help`.
const (
	groupRun      = "run"
	groupInspect  = "inspect"
	groupDiscover = "discover"
)

// newRootCmd assembles the command tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "redswitchboard",
		Short: "a hub: pick a source protocol and a sink protocol",
		Long: "redswitchboard - a hub: pick a source protocol and a sink protocol.\n" +
			"Each protocol works as either side. Run `sources`/`sinks` to list them.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddGroup(
		&cobra.Group{ID: groupRun, Title: "Run:"},
		&cobra.Group{ID: groupInspect, Title: "Inspect:"},
		&cobra.Group{ID: groupDiscover, Title: "Discover:"},
	)
	root.AddCommand(
		newServeCmd(), newMockCmd(),
		newStatusCmd(), newStatsCmd(), newMetricsCmd(), newCacheCmd(), newShowCmd(), newTeslamateCmd(),
		newSourcesCmd(), newSinksCmd(), newStreamSourcesCmd(), newStreamSinksCmd(), newCommandersCmd(), newConfigCmd(), newVersionCmd(),
	)
	return root
}

// Execute is the binary entry point.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "print the redswitchboard version",
		GroupID: groupDiscover,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("redswitchboard " + version)
			return nil
		},
	}
}
