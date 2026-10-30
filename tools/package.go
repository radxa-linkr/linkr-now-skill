// package builds self-contained skill archives using only the Go toolchain.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func main() {
	versionFile, err := os.ReadFile("VERSION")
	if err != nil {
		fail(fmt.Errorf("read VERSION from the repository root: %w", err))
	}
	pinnedVersion := strings.TrimSpace(string(versionFile))
	version := flag.String("version", pinnedVersion, "binary and archive version (must match VERSION)")
	flag.Parse()
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$`).MatchString(*version) {
		fail(fmt.Errorf("version must use vMAJOR.MINOR.PATCH, optionally followed by a prerelease suffix"))
	}
	if *version != pinnedVersion {
		fail(fmt.Errorf("version %q does not match VERSION %q; update VERSION before packaging", *version, pinnedVersion))
	}
	if _, err := os.Stat("go.mod"); err != nil {
		fail(fmt.Errorf("run the packager from the repository root: %w", err))
	}
	if err := os.MkdirAll("dist", 0755); err != nil {
		fail(err)
	}
	platforms := []struct{ os, arch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	}
	var checksums []string
	for _, platform := range platforms {
		name := platform.os + "-" + platform.arch
		binary := filepath.Join("bin", name, "linkr-now")
		if platform.os == "windows" {
			binary += ".exe"
		}
		if err := os.MkdirAll(filepath.Dir(binary), 0755); err != nil {
			fail(err)
		}
		command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -X main.buildVersion="+*version, "-o", binary, "./cmd/linkr-now")
		command.Env = append(os.Environ(), "GOOS="+platform.os, "GOARCH="+platform.arch, "CGO_ENABLED=0", "GOTOOLCHAIN=local")
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			fail(fmt.Errorf("build %s: %w", name, err))
		}
		if err := os.Chmod(binary, 0755); err != nil {
			fail(err)
		}
		standalone := filepath.Join("dist", "linkr-now-"+*version+"-"+name)
		if platform.os == "windows" {
			standalone += ".exe"
		}
		if err := copyExecutable(binary, standalone); err != nil {
			fail(err)
		}
		archive := filepath.Join("dist", "linkr-now-skill-"+*version+"-"+name+".zip")
		files := []string{"SKILL.md", "agents/openai.yaml", "references/usage.md", "LICENSE", "THIRD_PARTY_NOTICES.txt", "VERSION", "scripts/linkr-now.sh", "scripts/linkr-now.ps1", binary}
		if err := makeArchive(archive, files); err != nil {
			fail(err)
		}
		for _, path := range []string{archive, standalone} {
			entry, err := checksum(path)
			if err != nil {
				fail(err)
			}
			checksums = append(checksums, entry)
			fmt.Println(path)
		}
	}
	for _, name := range []string{"LICENSE", "THIRD_PARTY_NOTICES.txt"} {
		data, err := os.ReadFile(name)
		if err != nil {
			fail(err)
		}
		path := filepath.Join("dist", name)
		if err := os.WriteFile(path, data, 0644); err != nil {
			fail(err)
		}
		entry, err := checksum(path)
		if err != nil {
			fail(err)
		}
		checksums = append(checksums, entry)
	}
	sort.Strings(checksums)
	if err := os.WriteFile(filepath.Join("dist", "SHA256SUMS"), []byte(strings.Join(checksums, "\n")+"\n"), 0644); err != nil {
		fail(err)
	}
}

func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func checksum(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x  %s", hash.Sum(nil), filepath.Base(path)), nil
}

func makeArchive(path string, files []string) (err error) {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	w := zip.NewWriter(file)
	defer func() {
		if closeErr := w.Close(); err == nil {
			err = closeErr
		}
	}()
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = "linkr-now-skill/" + filepath.ToSlash(path)
		header.Method = zip.Deflate
		header.Modified = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		entry, err := w.CreateHeader(header)
		if err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(entry, input)
		input.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
