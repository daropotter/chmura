package cli

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/daropotter/chmura/internal/apiclient"
)

func newRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "remote", Short: "Inspect installation connections"}
	cmd.AddCommand(newRemoteInspectCmd())
	return cmd
}

func newRemoteInspectCmd() *cobra.Command {
	var endpoint, caFile string
	cmd := &cobra.Command{
		Use: "inspect", Short: "Inspect an installation by its HTTPS endpoint",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if endpoint == "" {
				return &usageError{errors.New("remote inspect requires --endpoint")}
			}
			httpClient, err := inspectionHTTPClient(caFile)
			if err != nil {
				return &usageError{err}
			}
			if httpClient != nil {
				defer httpClient.CloseIdleConnections()
			}
			client, err := apiclient.New(endpoint, httpClient)
			if err != nil {
				return &usageError{err}
			}
			info, err := client.Info(cmd.Context())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Endpoint: %s\nServer version: %q\nAPI versions: %q\nProfile: %s\nFeatures: %q\n",
				endpoint, info.ServerVersion, info.APIVersions, info.Profile, info.Features)
			return err
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "installation HTTPS endpoint")
	cmd.Flags().StringVar(&caFile, "ca-cert", "", "PEM CA certificates to trust in addition to system roots")
	return cmd
}

func inspectionHTTPClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return nil, nil
	}
	certificates, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(certificates) {
		return nil, errors.New("CA file contains no valid PEM certificates")
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13},
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   true,
	}}, nil
}
