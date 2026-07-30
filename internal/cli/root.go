// Package cli builds the chmura command-line interface.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/daropotter/chmura/internal/version"
)

// NewRootCmd builds the root chmura command. Top-level help shows the mental
// model and the available commands; detail lives one level down.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "chmura",
		Short: "Deploy applications to a private cloud you control.",
		Long: `Chmura deploys applications to a private cloud you control.

You work in a small vocabulary — installations, remotes, clusters, spaces,
projects, applications, volumes, and endpoints — never the objects of the
orchestrator underneath.`,
		Example: "  chmura version      Print the version\n" +
			"  chmura --help       Show this help",
		Version:       version.Version,
		SilenceErrors: true,
	}

	// The version contract is a bare version string, shared with every binary.
	root.SetVersionTemplate("{{.Version}}\n")
	// Pre-register --version without a shorthand so cobra does not auto-assign
	// -v to it; -v is reserved for --verbose (CLI contract).
	root.Flags().Bool("version", false, "version for chmura")
	// Ship the completion command deliberately later, not by default.
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return nil
		},
	}
}
