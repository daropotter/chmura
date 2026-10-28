package test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type installerCase struct {
	os, arch, version, assetOS, assetArch string
	badChecksum, noChecksum, noInstall    bool
	badBinary                             bool
	compactReleases                       bool
}

func TestInstallerInstallsRelease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options installerCase
		version string
	}{
		{"newest including prerelease", installerCase{}, "0.2.0-beta.1"},
		{"compact release response", installerCase{compactReleases: true}, "0.2.0-beta.1"},
		{"pinned release", installerCase{version: "v0.1.0"}, "0.1.0"},
		{"darwin arm64", installerCase{os: "Darwin", arch: "arm64", assetOS: "darwin", assetArch: "arm64"}, "0.2.0-beta.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, dir := runInstaller(t, tc.options)
			if code != 0 {
				t.Fatalf("installer exit = %d, want 0\n%s", code, out)
			}
			for _, name := range binaries {
				path := filepath.Join(dir, name)
				info, err := os.Stat(path)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
					t.Fatalf("%s was not installed as an executable: %v", name, err)
				}
				got, err := exec.Command(path, "--version").CombinedOutput()
				if err != nil || strings.TrimSpace(string(got)) != tc.version {
					t.Errorf("%s version = %q, %v; want %s", name, got, err, tc.version)
				}
			}
		})
	}
}

func TestInstallerRejectsBadConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options installerCase
	}{
		{"unsupported OS", installerCase{os: "Plan9"}},
		{"unsupported architecture", installerCase{arch: "riscv64"}},
		{"version without v", installerCase{version: "0.1.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, dir := runInstaller(t, tc.options)
			if code != 2 {
				t.Errorf("installer exit = %d, want 2\n%s", code, out)
			}
			assertNothingInstalled(t, dir)
		})
	}
}

func TestInstallerRequiresVerifiedArchive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options installerCase
		signal  string
	}{
		{"checksum mismatch", installerCase{badChecksum: true}, "checksum"},
		{"no checksum tool", installerCase{noChecksum: true}, "sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, dir := runInstaller(t, tc.options)
			if code != 1 {
				t.Errorf("installer exit = %d, want 1\n%s", code, out)
			}
			if !strings.Contains(out, tc.signal) {
				t.Errorf("failure should explain %s\n%s", tc.signal, out)
			}
			assertNothingInstalled(t, dir)
		})
	}
}

func TestInstallerRequiresInstallTool(t *testing.T) {
	code, out, dir := runInstaller(t, installerCase{noInstall: true})
	if code != 1 {
		t.Errorf("installer exit = %d, want 1 when install is unavailable\n%s", code, out)
	}
	if !strings.Contains(out, "install tool") {
		t.Errorf("missing dependency should be named\n%s", out)
	}
	assertNothingInstalled(t, dir)
}

func TestInstallerRejectsNonRunnableBinary(t *testing.T) {
	code, out, _ := runInstaller(t, installerCase{badBinary: true})
	if code != 1 {
		t.Errorf("installer exit = %d, want 1 for a non-runnable chmura\n%s", code, out)
	}
	if !strings.Contains(out, "did not run") {
		t.Errorf("failure should explain that the binary did not run\n%s", out)
	}
}

func assertNothingInstalled(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil || len(entries) != 0 {
		t.Errorf("failed installer changed the installation directory: %v, %v", entries, err)
	}
}

func runInstaller(t *testing.T, tc installerCase) (int, string, string) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the installer supports Linux and macOS")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	if tc.os == "" {
		tc.os = "Linux"
	}
	if tc.arch == "" {
		tc.arch = "x86_64"
	}
	if tc.assetOS == "" {
		tc.assetOS = "linux"
	}
	if tc.assetArch == "" {
		tc.assetArch = "amd64"
	}
	tag := tc.version
	if tag == "" {
		tag = "v0.2.0-beta.1"
	}
	root := t.TempDir()
	binDir, fixtures := filepath.Join(root, "tools"), filepath.Join(root, "release")
	for _, dir := range []string{binDir, fixtures} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	archive := fmt.Sprintf("chmura_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), tc.assetOS, tc.assetArch)
	writeReleaseFixture(t, fixtures, archive, strings.TrimPrefix(tag, "v"), tc)
	tools := []string{"cat", "tr", "grep", "head", "sed", "mktemp", "rm", "mkdir", "tar", "gzip"}
	if !tc.noInstall {
		tools = append(tools, "install")
	}
	if !tc.noChecksum {
		if _, err := exec.LookPath("sha256sum"); err == nil {
			tools = append(tools, "sha256sum")
		} else {
			tools = append(tools, "shasum")
		}
	}
	for _, name := range tools {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("installer test requires %s: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeInstallerTestFile(t, filepath.Join(binDir, "uname"), `#!/bin/sh
case "$1" in
  -s) printf '%s\n' "$CHMURA_TEST_OS" ;;
  -m) printf '%s\n' "$CHMURA_TEST_ARCH" ;;
  *) exit 1 ;;
esac
`, 0o755)
	// The transport stub only serves local release fixtures. The installer still
	// uses real checksum verification, extraction, file installation and binaries.
	writeInstallerTestFile(t, filepath.Join(binDir, "curl"), `#!/bin/sh
set -eu
url= output=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output) output=$2; shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
case "$url" in
  https://api.github.com/repos/daropotter/chmura/releases) source="$CHMURA_TEST_FIXTURES/releases.json" ;;
  "https://github.com/daropotter/chmura/releases/download/$CHMURA_TEST_TAG/$CHMURA_TEST_ARCHIVE") source="$CHMURA_TEST_FIXTURES/$CHMURA_TEST_ARCHIVE" ;;
  "https://github.com/daropotter/chmura/releases/download/$CHMURA_TEST_TAG/checksums.txt") source="$CHMURA_TEST_FIXTURES/checksums.txt" ;;
  *) exit 22 ;;
esac
if [ -n "$output" ]; then cat "$source" > "$output"; else cat "$source"; fi
`, 0o755)
	script, err := filepath.Abs("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "installed binaries")
	cmd := exec.Command(sh, script)
	cmd.Env = append(os.Environ(), "PATH="+binDir, "VERSION="+tc.version, "CHMURA_INSTALL_DIR="+installDir,
		"CHMURA_TEST_OS="+tc.os, "CHMURA_TEST_ARCH="+tc.arch, "CHMURA_TEST_FIXTURES="+fixtures,
		"CHMURA_TEST_TAG="+tag, "CHMURA_TEST_ARCHIVE="+archive)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out), installDir
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("running installer: %v\n%s", err, out)
	}
	return exit.ExitCode(), string(out), installDir
}

func writeReleaseFixture(t *testing.T, dir, archive, version string, tc installerCase) {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	for _, name := range binaries {
		body := "#!/bin/sh\nprintf '%s\\n' '" + version + "'\n"
		if tc.badBinary && name == "chmura" {
			body = "#!/bin/sh\nexit 1\n"
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, archive), compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(compressed.Bytes())
	if tc.badChecksum {
		hash = [sha256.Size]byte{}
	}
	writeInstallerTestFile(t, filepath.Join(dir, "checksums.txt"), fmt.Sprintf("%x  %s\n", hash, archive), 0o644)
	releases := "[\n  {\"tag_name\": \"v0.2.0-beta.1\"},\n  {\"tag_name\": \"v0.1.0\"}\n]\n"
	if tc.compactReleases {
		releases = `[{"tag_name":"v0.2.0-beta.1"},{"tag_name":"v0.1.0"}]`
	}
	writeInstallerTestFile(t, filepath.Join(dir, "releases.json"), releases, 0o644)
}

func writeInstallerTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
