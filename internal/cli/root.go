// Package cli builds the chmura command-line interface.
package cli

import (
	"fmt"
	"strings"

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

	// Mark flag-parsing errors as usage errors so they map to ExitUsage. pflag
	// offers no flag suggestion, so when a typo'd flag is close to a known one,
	// append a hint using cobra's own phrasing — adapt to the framework rather
	// than restyling its messages.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if tok, ok := unknownLongFlag(err); ok {
			if s := suggestFlag(cmd.Flags(), strings.TrimLeft(tok, "-")); s != "" {
				return &usageError{fmt.Errorf("%w\n\nDid you mean this?\n\t--%s", err, s)}
			}
		}
		return &usageError{err}
	})

	// The version contract is a bare version string, shared with every binary.
	root.SetVersionTemplate("{{.Version}}\n")
	// Pre-register --version without a shorthand so cobra does not auto-assign
	// -v to it; -v is reserved for --verbose (CLI contract).
	root.Flags().Bool("version", false, "version for chmura")
	// Ship the completion command deliberately later, not by default.
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(newVersionCmd())
	root.AddCommand(newDoctorCmd())
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
