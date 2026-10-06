package cli

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const (
	unixSocketPathSafeLimit = 100
	testNMHomePrefix        = "nm-cli-test-"
	maxMkdirTempSuffix      = "4294967295"
)

// socketSafeTempBase keeps ordinary test paths under the configured temp root
// when the longest NM_HOME fixture still leaves room for the Unix socket path.
// os.MkdirTemp appends a decimal uint32, whose longest value is 4294967295.
func socketSafeTempBase(tempDir, goos string) string {
	if goos == "windows" {
		return tempDir
	}

	base, err := filepath.Abs(tempDir)
	if err != nil {
		return "/tmp"
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "/tmp"
	}
	base = filepath.Clean(base)

	root := filepath.Join(base, testNMHomePrefix+maxMkdirTempSuffix)
	socketPath := paths.WithRoot(root).Socket()
	if len([]byte(socketPath)) >= unixSocketPathSafeLimit {
		return "/tmp"
	}
	return base
}

func TestSocketSafeTempBaseUsesShortConfiguredTempDir(t *testing.T) {
	base := ownedTempDir(t, "st-")
	got := socketSafeTempBase(base, "linux")
	want, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("socketSafeTempBase(%q) = %q, want canonical temp dir %q", base, got, want)
	}
}

func TestSocketSafeTempBaseFallsBackForLongAndMultibytePaths(t *testing.T) {
	base := ownedTempDir(t, "lg-")

	longBase := filepath.Join(base, strings.Repeat("x", 80))
	if err := os.MkdirAll(longBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := socketSafeTempBase(longBase, "linux"); got != "/tmp" {
		t.Fatalf("long path base = %q, want /tmp fallback", got)
	}

	unicodeBase := filepath.Join(base, strings.Repeat("界", 12))
	if err := os.MkdirAll(unicodeBase, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(unicodeBase, testNMHomePrefix+maxMkdirTempSuffix)
	probe := paths.WithRoot(root).Socket()
	if runeCount := len([]rune(probe)); runeCount >= unixSocketPathSafeLimit {
		t.Fatalf("test path setup has %d runes; want fewer than %d", runeCount, unixSocketPathSafeLimit)
	}
	if byteCount := len([]byte(probe)); byteCount < unixSocketPathSafeLimit {
		t.Fatalf("test path setup has %d bytes; want at least %d", byteCount, unixSocketPathSafeLimit)
	}
	if got := socketSafeTempBase(unicodeBase, "linux"); got != "/tmp" {
		t.Fatalf("multibyte path base = %q, want /tmp fallback", got)
	}
}

func TestSocketSafeTempBasePreservesWindowsTempDir(t *testing.T) {
	configured := filepath.Join(os.TempDir(), "windows-temp")
	if got := socketSafeTempBase(configured, "windows"); got != configured {
		t.Fatalf("Windows temp base = %q, want unchanged %q", got, configured)
	}
}

func TestShortConfiguredTempDirAllowsOrdinaryDaemonSocket(t *testing.T) {
	configured := ownedTempDir(t, "sock-")
	t.Setenv("TMPDIR", configured)
	base := socketSafeTempBase(os.TempDir(), runtime.GOOS)
	root, err := os.MkdirTemp(base, "nmh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	if filepath.Dir(root) != base {
		t.Fatalf("ordinary NM_HOME root %q is outside selected temp dir %q", root, base)
	}
	socketPath := paths.WithRoot(root).Socket()
	if runtime.GOOS != "windows" {
		if got := len([]byte(socketPath)); got >= unixSocketPathSafeLimit {
			t.Fatalf("ordinary daemon socket path has %d bytes, want fewer than %d: %q", got, unixSocketPathSafeLimit, socketPath)
		}
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("listen on ordinary daemon socket path %q: %v", socketPath, err)
		}
		if err := listener.Close(); err != nil {
			t.Fatalf("close ordinary daemon socket listener: %v", err)
		}
	}
}

func ownedTempDir(t *testing.T, pattern string) string {
	t.Helper()

	dir, err := os.MkdirTemp(os.TempDir(), pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
