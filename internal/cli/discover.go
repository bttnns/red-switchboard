package cli

import (
	"fmt"

	"github.com/bttnns/red-switchboard/internal/config"
	"github.com/bttnns/red-switchboard/internal/plugin/commander"
	"github.com/bttnns/red-switchboard/internal/plugin/sink"
	"github.com/bttnns/red-switchboard/internal/plugin/source"
	"github.com/bttnns/red-switchboard/internal/plugin/streamsink"
	"github.com/bttnns/red-switchboard/internal/plugin/streamsource"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// newSourcesCmd lists the registered input source plugins.
func newSourcesCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "sources",
		Short:   "list registered input source plugins",
		GroupID: groupDiscover,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("registered input sources:")
			for _, n := range source.Names() {
				fmt.Printf("  %s\n", n)
			}
			return nil
		},
	}
}

// newSinksCmd lists the registered output sink plugins.
func newSinksCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "sinks",
		Short:   "list registered output sink plugins",
		GroupID: groupDiscover,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("registered output sinks:")
			for _, n := range sink.Names() {
				fmt.Printf("  %s\n", n)
			}
			return nil
		},
	}
}

// newConfigCmd shows the resolved configuration (`config print`).
func newConfigCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:     "config",
		Short:   "config print: show the resolved configuration",
		GroupID: groupDiscover,
		Args:    cobra.MaximumNArgs(1), // optional "print" verb
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			out, err := yaml.Marshal(cfg)
			if err != nil {
				return err
			}
			fmt.Printf("# resolved from %s (defaults filled)\n%s", configPath, string(out))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "config/redswitchboard.yaml", "path to config YAML")
	return cmd
}

// newStreamSourcesCmd lists the registered streaming-source plugins (push in).
func newStreamSourcesCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "stream-sources",
		Short:   "list registered streaming-source plugins (push in)",
		GroupID: groupDiscover,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("registered streaming sources:")
			for _, n := range streamsource.Names() {
				fmt.Printf("  %s\n", n)
			}
			return nil
		},
	}
}

// newStreamSinksCmd lists the registered streaming-sink plugins (push out WSS).
func newStreamSinksCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "stream-sinks",
		Short:   "list registered streaming-sink plugins (push out WSS)",
		GroupID: groupDiscover,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("registered streaming sinks:")
			for _, n := range streamsink.Names() {
				fmt.Printf("  %s\n", n)
			}
			return nil
		},
	}
}

// newCommandersCmd lists the registered commander plugins (signed-command write).
func newCommandersCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "commanders",
		Short:   "list registered commander plugins (signed-command write path)",
		GroupID: groupDiscover,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Println("registered commanders:")
			for _, n := range commander.Names() {
				fmt.Printf("  %s\n", n)
			}
			return nil
		},
	}
}
