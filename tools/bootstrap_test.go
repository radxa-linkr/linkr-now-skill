package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const bootstrapFixtureVersion = "v0.1.0"

// The fixture also replaces curl/wget on Unix. It has no networking code, so a
// launcher download can only copy these local fixtures or fail as offline.
const bootstrapFixtureSource = `package main

import (
    "encoding/json"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "strings"
)

func main() {
    name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
    if name == "curl" || name == "wget" {
        if err := download(os.Args[1:]); err != nil {
            fmt.Fprintln(os.Stderr, err)
            os.Exit(1)
        }
        return
    }
    log, err := os.OpenFile(os.Getenv("LINKR_BOOTSTRAP_TEST_EXECUTIONS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
    if err != nil { panic(err) }
    if err := json.NewEncoder(log).Encode(os.Args[1:]); err != nil { panic(err) }
    if err := log.Close(); err != nil { panic(err) }
    if len(os.Args) == 2 && os.Args[1] == "--version" {
        fmt.Println("linkr-now v0.1.0")
        return
    }
    for _, arg := range os.Args[1:] {
        if arg == "--exit-seven" { os.Exit(7) }
    }
    if err := json.NewEncoder(os.Stdout).Encode(os.Args[1:]); err != nil { panic(err) }
}

func download(args []string) error {
    var output, url string
    for index := 0; index < len(args); index++ {
        switch args[index] {
        case "--output", "-o", "-O":
            index++
            if index >= len(args) { return fmt.Errorf("missing download destination") }
            output = args[index]
        default:
            if strings.HasPrefix(args[index], "https://") { url = args[index] }
        }
    }
    log, err := os.OpenFile(os.Getenv("LINKR_BOOTSTRAP_TEST_DOWNLOADS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
    if err != nil { return err }
    _, writeErr := fmt.Fprintln(log, url)
    closeErr := log.Close()
    if writeErr != nil { return writeErr }
    if closeErr != nil { return closeErr }
    if os.Getenv("LINKR_BOOTSTRAP_TEST_MODE") != "online" { return fmt.Errorf("test network is offline") }
    base := os.Getenv("LINKR_BOOTSTRAP_TEST_RELEASE")
    var source string
    switch url {
    case base + "/SHA256SUMS":
        source = os.Getenv("LINKR_BOOTSTRAP_TEST_MANIFEST")
    case base + "/" + os.Getenv("LINKR_BOOTSTRAP_TEST_ASSET"):
        source = os.Getenv("LINKR_BOOTSTRAP_TEST_BINARY")
    default:
        return fmt.Errorf("unexpected download URL: %q", url)
    }
    if output == "" { return fmt.Errorf("missing download destination") }
    input, err := os.Open(source)
    if err != nil { return err }
    defer input.Close()
    target, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
    if err != nil { return err }
    _, copyErr := io.Copy(target, input)
    closeErr = target.Close()
    if copyErr != nil { return copyErr }
    return closeErr
}
`

// Argument values and paths travel in environment variables, never as generated
// PowerShell source. Defining this function intercepts every launcher request in
// Windows PowerShell 5.1 as well as PowerShell 7 without making an HTTP request.
const bootstrapPowerShellWrapper = `
$ErrorActionPreference = 'Stop'
# The launcher must verify downloads even when this cmdlet cannot be loaded.
function Get-FileHash { throw 'Get-FileHash is unavailable in this test session.' }
function Invoke-WebRequest {
    [CmdletBinding()]
    param([switch]$UseBasicParsing, [string]$Uri, [string]$OutFile, [int]$TimeoutSec)
    [System.IO.File]::AppendAllText($env:LINKR_BOOTSTRAP_TEST_DOWNLOADS, $Uri + [System.Environment]::NewLine)
    if ($env:LINKR_BOOTSTRAP_TEST_MODE -ne 'online') { throw 'Test network is offline.' }
    $release = $env:LINKR_BOOTSTRAP_TEST_RELEASE
    if ($Uri -ceq ($release + '/SHA256SUMS')) {
        $source = $env:LINKR_BOOTSTRAP_TEST_MANIFEST
    }
    elseif ($Uri -ceq ($release + '/' + $env:LINKR_BOOTSTRAP_TEST_ASSET)) {
        $source = $env:LINKR_BOOTSTRAP_TEST_BINARY
    }
    else { throw ('Unexpected download URL: ' + $Uri) }
    [System.IO.File]::Copy($source, $OutFile, $true)
}
$forward = @()
foreach ($argument in (ConvertFrom-Json -InputObject $env:LINKR_BOOTSTRAP_TEST_ARGUMENTS)) {
    $forward += [string]$argument
}
& $env:LINKR_BOOTSTRAP_TEST_LAUNCHER @forward
exit $LASTEXITCODE
`

type bootstrapCase struct {
	launcher, shell, fixture, bundled, cached string
	manifest, executions, downloads           string
	asset, release, mockBin, cache            string
}

func TestBootstrapLaunchers(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("launchers support Linux, macOS, and Windows")
	}
	shell, platform := bootstrapPlatform(t)
	fixture := buildBootstrapFixture(t)
	arguments := []string{"scan", "--target", "device name with spaces", "--json", "--label=two words", "literal;$value"}

	t.Run("bundled_preserves_arguments_and_exit_code_without_download", func(t *testing.T) {
		c := newBootstrapCase(t, shell, platform, fixture, bootstrapFixtureVersion)
		copyBootstrapFixture(t, fixture, c.bundled)
		stdout, stderr, code := c.run(t, "offline", arguments...)
		checkBootstrapArguments(t, stdout, stderr, code, arguments)
		stdout, stderr, code = c.run(t, "offline", "--exit-seven")
		if code != 7 || stdout != "" {
			t.Fatalf("exit was not forwarded: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertBootstrapMissing(t, c.downloads)
		assertBootstrapMissing(t, c.cached)
	})

	t.Run("valid_cache_runs_offline_without_download", func(t *testing.T) {
		c := newBootstrapCase(t, shell, platform, fixture, bootstrapFixtureVersion)
		copyBootstrapFixture(t, fixture, c.cached)
		stdout, stderr, code := c.run(t, "offline", arguments...)
		checkBootstrapArguments(t, stdout, stderr, code, arguments)
		assertBootstrapMissing(t, c.downloads)
	})

	t.Run("first_download_is_verified_and_reused_offline", func(t *testing.T) {
		c := newBootstrapCase(t, shell, platform, fixture, bootstrapFixtureVersion)
		stdout, stderr, code := c.run(t, "online", arguments...)
		checkBootstrapArguments(t, stdout, stderr, code, arguments)
		gotDownloads := readBootstrapFile(t, c.downloads)
		urls := strings.Fields(gotDownloads)
		sort.Strings(urls)
		wantURLs := []string{c.release + "/" + c.asset, c.release + "/SHA256SUMS"}
		sort.Strings(wantURLs)
		if !reflect.DeepEqual(urls, wantURLs) {
			t.Fatalf("download URLs=%q, want %q", urls, wantURLs)
		}
		if readBootstrapFile(t, c.cached) != readBootstrapFile(t, fixture) {
			t.Fatal("activated cache does not match the verified fixture")
		}
		stdout, stderr, code = c.run(t, "offline", arguments...)
		checkBootstrapArguments(t, stdout, stderr, code, arguments)
		if after := readBootstrapFile(t, c.downloads); after != gotDownloads {
			t.Fatalf("cached invocation attempted another download: %q", after)
		}
	})

	t.Run("bad_checksum_is_never_executed_or_activated", func(t *testing.T) {
		c := newBootstrapCase(t, shell, platform, fixture, bootstrapFixtureVersion)
		writeBootstrapFile(t, c.manifest, []byte(strings.Repeat("0", 64)+"  "+c.asset+"\n"), 0600)
		stdout, stderr, code := c.run(t, "online", arguments...)
		if code == 0 || stdout != "" || !strings.Contains(strings.ToLower(stderr), "checksum") {
			t.Fatalf("checksum failure was not reported: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertBootstrapMissing(t, c.cached)
		assertBootstrapMissing(t, c.executions)
	})

	t.Run("runtime_must_match_pinned_version", func(t *testing.T) {
		c := newBootstrapCase(t, shell, platform, fixture, "v0.1.1")
		copyBootstrapFixture(t, fixture, c.bundled)
		stdout, stderr, code := c.run(t, "online", arguments...)
		if code == 0 || stdout != "" || !strings.Contains(strings.ToLower(stderr), "version") {
			t.Fatalf("version mismatch was not reported: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		assertBootstrapMissing(t, c.cached)
		for _, line := range strings.Split(strings.TrimSpace(readBootstrapFile(t, c.executions)), "\n") {
			var args []string
			if err := json.Unmarshal([]byte(line), &args); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args, []string{"--version"}) {
				t.Fatalf("unmatched runtime executed application arguments: %q", args)
			}
		}
	})
}

func bootstrapPlatform(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		shell, err := exec.LookPath("sh")
		if err != nil {
			t.Fatal(err)
		}
		return shell, runtime.GOOS + "-" + runtime.GOARCH
	}
	shell, err := exec.LookPath("powershell.exe")
	if err != nil {
		shell, err = exec.LookPath("pwsh")
		if err != nil {
			t.Fatal("Windows launcher integration requires PowerShell")
		}
	}
	// Match the launcher's OS architecture, including an emulated Go process.
	cmd := exec.Command(shell, "-NoProfile", "-Command", `try { [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE } }`)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("detect Windows architecture: %v: %s", err, output)
	}
	arch := strings.ToLower(strings.TrimSpace(string(output)))
	if arch == "x64" {
		arch = "amd64"
	}
	if arch != "amd64" && arch != "arm64" {
		t.Fatalf("unsupported Windows architecture: %q", arch)
	}
	return shell, "windows-" + arch
}

func buildBootstrapFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	writeBootstrapFile(t, source, []byte(bootstrapFixtureSource), 0600)
	binary := filepath.Join(dir, "fixture runtime")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binary, source)
	cmd.Env = bootstrapEnvironment(map[string]string{
		"GOOS": runtime.GOOS, "GOARCH": runtime.GOARCH, "CGO_ENABLED": "0",
		"GO111MODULE": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "",
	})
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build native bootstrap fixture: %v: %s", err, output)
	}
	return binary
}

func newBootstrapCase(t *testing.T, shell, platform, fixture, pin string) bootstrapCase {
	t.Helper()
	root := filepath.Join(t.TempDir(), "skill folder with spaces")
	cache := filepath.Join(t.TempDir(), "cache folder with spaces")
	name, launcherName, suffix := "linkr-now", "linkr-now.sh", ""
	if runtime.GOOS == "windows" {
		name, launcherName, suffix = "linkr-now.exe", "linkr-now.ps1", ".exe"
	}
	launcher := filepath.Join(root, "scripts", launcherName)
	copyBootstrapFixture(t, filepath.Join("..", "scripts", launcherName), launcher)
	writeBootstrapFile(t, filepath.Join(root, "VERSION"), []byte(pin+"\n"), 0600)
	c := bootstrapCase{
		launcher: launcher, shell: shell, fixture: fixture,
		bundled: filepath.Join(root, "bin", platform, name), cached: filepath.Join(cache, pin, platform, name),
		manifest: filepath.Join(root, "fixture SHA256SUMS"), executions: filepath.Join(root, "executions.jsonl"),
		downloads: filepath.Join(root, "downloads.log"), asset: "linkr-now-" + pin + "-" + platform + suffix,
		release: "https://github.com/radxa-linkr/linkr-now-skill/releases/download/" + pin,
		mockBin: filepath.Join(root, "mock commands"), cache: cache,
	}
	checksum := sha256.Sum256([]byte(readBootstrapFile(t, fixture)))
	writeBootstrapFile(t, c.manifest, []byte(fmt.Sprintf("%x  %s\n", checksum, c.asset)), 0600)
	if runtime.GOOS != "windows" {
		for _, command := range []string{"curl", "wget"} {
			copyBootstrapFixture(t, fixture, filepath.Join(c.mockBin, command))
		}
	}
	return c
}

func (c bootstrapCase) run(t *testing.T, mode string, args ...string) (string, string, int) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"LINKR_NOW_CACHE_DIR": c.cache, "LINKR_BOOTSTRAP_TEST_MODE": mode,
		"LINKR_BOOTSTRAP_TEST_BINARY": c.fixture, "LINKR_BOOTSTRAP_TEST_MANIFEST": c.manifest,
		"LINKR_BOOTSTRAP_TEST_DOWNLOADS": c.downloads, "LINKR_BOOTSTRAP_TEST_EXECUTIONS": c.executions,
		"LINKR_BOOTSTRAP_TEST_RELEASE": c.release, "LINKR_BOOTSTRAP_TEST_ASSET": c.asset,
		"LINKR_BOOTSTRAP_TEST_LAUNCHER": c.launcher, "LINKR_BOOTSTRAP_TEST_ARGUMENTS": string(encoded),
	}
	var commandArgs []string
	if runtime.GOOS == "windows" {
		commandArgs = []string{"-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", bootstrapPowerShellWrapper}
	} else {
		commandArgs = append([]string{c.launcher}, args...)
		env["PATH"] = c.mockBin + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.shell, commandArgs...)
	cmd.Env = bootstrapEnvironment(env)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out: %s", stderr.String())
	}
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return stdout.String(), stderr.String(), exit.ExitCode()
	}
	t.Fatalf("start launcher: %v", err)
	return "", "", -1
}

func bootstrapEnvironment(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		replaced := false
		for override := range overrides {
			if strings.EqualFold(key, override) {
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, value)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func checkBootstrapArguments(t *testing.T, stdout, stderr string, code int, want []string) {
	t.Helper()
	var got []string
	if code != 0 || json.Unmarshal([]byte(stdout), &got) != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("launcher arguments: code=%d stdout=%q stderr=%q, want %q", code, stdout, stderr, want)
	}
}

func copyBootstrapFixture(t *testing.T, source, target string) {
	t.Helper()
	writeBootstrapFile(t, target, []byte(readBootstrapFile(t, source)), 0755)
}

func readBootstrapFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeBootstrapFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func assertBootstrapMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected path to remain absent: %s (stat error: %v)", path, err)
	}
}
