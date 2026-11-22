package servercmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/daropotter/chmura/internal/identity"
	"github.com/daropotter/chmura/internal/state"
	"github.com/daropotter/chmura/internal/state/sqlite"
)

func newInitCmd() *cobra.Command {
	var statePath, name, tokenPath, key string
	cmd := &cobra.Command{
		Use: "init", Short: "Initialize the first administrator in the state database", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) (result error) {
			ctx := cmd.Context()
			if err := ctx.Err(); err != nil {
				return err
			}
			if !filepath.IsAbs(statePath) || strings.TrimSpace(name) == "" || strings.TrimSpace(tokenPath) == "" || strings.TrimSpace(key) == "" {
				return &usageError{errors.New("init requires an absolute --state path, --admin, --admin-token-file and --idempotency-key")}
			}
			digest, err := bootstrapTokenDigest(tokenPath)
			if err != nil {
				return &usageError{err}
			}
			store, err := sqlite.Open(ctx, statePath)
			if err != nil {
				return err
			}
			defer func() { result = errors.Join(result, store.Close()) }()
			admin, _, err := store.BootstrapAdmin(ctx, state.BootstrapAdminRequest{Name: name, IdempotencyKey: key, TokenDigest: digest})
			if errors.Is(err, state.ErrConflict) {
				return fmt.Errorf("first administrator is already initialized with different inputs: %w", err)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Administrator %q is initialized.\n", admin.Name)
			return err
		},
	}
	cmd.Flags().StringVar(&statePath, "state", "", "Absolute path to the single-mode state database")
	cmd.Flags().StringVar(&name, "admin", "", "Name of the first administrator")
	cmd.Flags().StringVar(&tokenPath, "admin-token-file", "", "Private file containing a random 32-byte token in lowercase hex")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Stable request key for retrying initialization")
	return cmd
}

func bootstrapTokenDigest(path string) ([32]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !privateRegularFile(info) {
		return [32]byte{}, errors.New("--admin-token-file must be a readable regular file with no group or other permissions")
	}
	file, err := os.Open(path)
	if err != nil {
		return [32]byte{}, errors.New("cannot open --admin-token-file")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !privateRegularFile(openedInfo) || !os.SameFile(info, openedInfo) {
		return [32]byte{}, errors.New("--admin-token-file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, identity.AdminTokenLength+3))
	if err != nil {
		return [32]byte{}, errors.New("cannot read --admin-token-file")
	}
	if token, hasNewline := bytes.CutSuffix(data, []byte("\n")); hasNewline {
		data = bytes.TrimSuffix(token, []byte("\r"))
	}
	digest, err := identity.AdminTokenDigest(data)
	if err != nil {
		return [32]byte{}, fmt.Errorf("--admin-token-file: %w", err)
	}
	return digest, nil
}

func privateRegularFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
}
