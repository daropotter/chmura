package cli

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Depth controls how deep init scans for applications.
type Depth int

const (
	DepthFlat     Depth = iota // the directory itself (default)
	DepthChildren              // the directory and its immediate subdirectories
	DepthAll                   // the whole tree below the directory
)

func (d Depth) String() string {
	switch d {
	case DepthChildren:
		return "1"
	case DepthAll:
		return "all"
	default:
		return "0"
	}
}

// parseDepth maps the --depth flag value onto a Depth. The contract is "0", "1",
// or "all"; anything else is a usage error.
func parseDepth(s string) (Depth, error) {
	switch s {
	case "0":
		return DepthFlat, nil
	case "1":
		return DepthChildren, nil
	case "all":
		return DepthAll, nil
	default:
		return 0, fmt.Errorf("invalid --depth %q: use 0, 1, or all", s)
	}
}

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Detect the project and write chmura.yaml",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			raw, err := cmd.Flags().GetString("depth")
			if err != nil {
				return &usageError{err}
			}
			depth, err := parseDepth(raw)
			if err != nil {
				return &usageError{err}
			}
			return runInit(cmd.OutOrStdout(), dir, depth)
		},
	}
	cmd.Flags().String("depth", "0", "how deep to scan for applications: 0 (this directory), 1 (direct subdirectories), all (the whole tree)")
	return cmd
}

// app is one application detected by init: its kebab-case name and its build
// context relative to the project directory.
type app struct {
	name    string
	context string
}

// runInit inspects dir and writes dir/chmura.yaml. It never overwrites an
// existing manifest, and refuses when no application source is found.
func runInit(out io.Writer, dir string, depth Depth) error {
	manifest := filepath.Join(dir, "chmura.yaml")
	if _, err := os.Stat(manifest); err == nil {
		return &usageError{fmt.Errorf("chmura.yaml already exists in %s", dir)}
	}
	if _, err := os.Stat(dir); err != nil {
		return &usageError{fmt.Errorf("no such directory: %s", dir)}
	}

	apps, err := findApps(dir, depth)
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		return noSourceError(dir, depth)
	}

	name, err := projectName(dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifest, []byte(generateManifest(name, apps)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s\n", manifest)
	return nil
}

// noSourceError is the precise "nothing found" failure. The message varies with
// the depth so it says what was actually scanned.
func noSourceError(dir string, depth Depth) error {
	if depth == DepthFlat {
		return &usageError{fmt.Errorf("no application source found in %s: expected a Dockerfile", dir)}
	}
	return &usageError{fmt.Errorf("no application source found under %s (depth %s): expected Dockerfiles", dir, depth)}
}

// findApps discovers applications under dir. An application is a directory that
// contains a Dockerfile. Hidden directories and directories that are themselves
// a separate Chmura project (they contain a chmura.yaml) are skipped and not
// descended into. App names are the kebab-cased directory basenames; a name
// collision after that normalization is an error, never a silent rename.
func findApps(dir string, depth Depth) ([]app, error) {
	var found []string
	switch depth {
	case DepthFlat:
		if hasDockerfile(dir) {
			found = append(found, dir)
		}
	case DepthChildren:
		if hasDockerfile(dir) {
			found = append(found, dir)
		}
		children, err := directChildren(dir)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if isSkipped(child) {
				continue
			}
			if hasDockerfile(child) {
				found = append(found, child)
			}
		}
	case DepthAll:
		if hasDockerfile(dir) {
			found = append(found, dir)
		}
		if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == dir || !d.IsDir() {
				return nil
			}
			if isSkipped(path) {
				return fs.SkipDir
			}
			if hasDockerfile(path) {
				found = append(found, path)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	apps := make([]app, 0, len(found))
	seen := make(map[string]string, len(found))
	for _, d := range found {
		ctx := "."
		if rel, err := filepath.Rel(dir, d); err == nil && rel != "." {
			ctx = filepath.ToSlash(rel)
		}
		// The name is the directory's real basename; for the root itself (a
		// relative ".") that is the absolute path's last element, not a dot.
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		name := kebabCase(filepath.Base(abs))
		if name == "" {
			return nil, &usageError{fmt.Errorf("cannot derive an application name from directory %q", d)}
		}
		if prev, ok := seen[name]; ok {
			return nil, &usageError{fmt.Errorf("cannot derive unique application names: %s and %s both normalize to %q; rename one of them", prev, ctx, name)}
		}
		seen[name] = ctx
		apps = append(apps, app{name: name, context: ctx})
	}
	return apps, nil
}

// hasDockerfile reports whether dir holds a regular Dockerfile. A directory
// named "Dockerfile" is not a source — it could never be built as one.
func hasDockerfile(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "Dockerfile"))
	return err == nil && fi.Mode().IsRegular()
}

// directChildren lists the immediate subdirectories of dir, in sorted order.
// A read failure is returned, never silently dropped.
func directChildren(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// isSkipped reports whether a directory is not a candidate: hidden, or its own
// Chmura project (it holds a chmura.yaml of its own).
func isSkipped(path string) bool {
	return strings.HasPrefix(filepath.Base(path), ".") || fileExists(filepath.Join(path, "chmura.yaml"))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func projectName(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	name := kebabCase(filepath.Base(abs))
	if name == "" {
		return "", &usageError{fmt.Errorf("cannot derive a project name from directory %s", abs)}
	}
	return name, nil
}

// kebabCase lowercases s and collapses runs of non-alphanumeric characters into
// a single dash, so a directory like "My Service" yields "my-service".
func kebabCase(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// generateManifest returns a minimal, valid chmura.yaml. What cannot be derived
// from a Dockerfile is written as commented scaffolding — once, under the first
// application — never guessed.
func generateManifest(name string, apps []app) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by chmura init. Fill in the commented scaffolding below —\n")
	fmt.Fprintf(&b, "# Chmura does not guess ports, health checks, endpoints, or values.\n")
	fmt.Fprintf(&b, "version: 1\n")
	fmt.Fprintf(&b, "name: %s\n\n", name)
	fmt.Fprintf(&b, "applications:\n")
	for i, a := range apps {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "  %s:\n", a.name)
		fmt.Fprintf(&b, "    source:\n")
		fmt.Fprintf(&b, "      context: %s\n", a.context)
		fmt.Fprintf(&b, "      dockerfile: Dockerfile\n")
		if i == 0 {
			b.WriteString("\n    # ports:\n    #   http: { number: 8080, protocol: http, visibility: project }\n")
			b.WriteString("\n    # env:\n    #   LOG_LEVEL: info\n")
			b.WriteString("\n    # health:\n    #   check: { http: { path: /healthz, port: http } }\n    #   startup-timeout: 2m\n")
		}
	}
	b.WriteString("\n# volumes: {}\n# endpoints: {}\n")
	return b.String()
}

// usageArgs wraps a positional-args validator so a violation maps to a usage
// error (exit 2) rather than an execution error.
func usageArgs(inner cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := inner(cmd, args); err != nil {
			return &usageError{err}
		}
		return nil
	}
}
