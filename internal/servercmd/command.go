// Package servercmd implements the chmura-server command line.
package servercmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/spf13/cobra"

	"github.com/daropotter/chmura/internal/server"
	"github.com/daropotter/chmura/internal/version"
)

type usageError struct{ error }

func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return &usageError{err}
	}
	return nil
}

// Execute runs the server command and returns the documented process exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := &cobra.Command{
		Use: "chmura-server", Short: "Run the Chmura control plane",
		Version: version.Version, SilenceErrors: true, SilenceUsage: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetVersionTemplate("{{.Version}}\n")
	root.Flags().Bool("version", false, "version for chmura-server")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err} })
	root.AddCommand(&cobra.Command{
		Use: "version", Short: "Print the version", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	})
	root.AddCommand(newServeCmd())
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, "Error:", err)
	if errors.Is(err, context.Canceled) {
		return 130
	}
	var usage *usageError
	if errors.As(err, &usage) || strings.HasPrefix(err.Error(), "unknown command") {
		return 2
	}
	return 1
}

func newServeCmd() *cobra.Command {
	var address, profile, certFile, keyFile string
	cmd := &cobra.Command{
		Use: "serve", Short: "Serve the control-plane API over HTTPS", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if certFile == "" || keyFile == "" {
				return &usageError{errors.New("serve requires --tls-cert and --tls-key")}
			}
			handler, err := server.NewHandler(profile)
			if err != nil {
				return &usageError{err}
			}
			certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return fmt.Errorf("load TLS certificate: %w", err)
			}
			listener, err := net.Listen("tcp", address)
			if err != nil {
				return err
			}
			defer listener.Close()
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Listening on https://%s\n", listener.Addr()); err != nil {
				return err
			}
			return server.Serve(cmd.Context(), listener, handler, certificate)
		},
	}
	cmd.Flags().StringVar(&address, "listen", "127.0.0.1:7443", "HTTPS listening address")
	cmd.Flags().StringVar(&profile, "profile", "production", "installation profile: production or dev")
	cmd.Flags().StringVar(&certFile, "tls-cert", "", "PEM certificate file")
	cmd.Flags().StringVar(&keyFile, "tls-key", "", "PEM private key file")
	return cmd
}
