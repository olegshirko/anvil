//go:build ignore

// update_manifest_gh.go fetches SHA512 checksums from a GitHub release
// using `gh release download`, then updates manifest.yaml and regenerates Go code.
//
// Usage:
//
//	go run update_manifest_gh.go
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	manifestFile = "manifest.yaml"
	repo         = "olegshirko/docker-mirror"
	releaseTag   = "master"
)

type rawManifest struct {
	Versions map[string]rawVersion `yaml:"versions"`
}

type rawVersion struct {
	Tag       string        `yaml:"tag"`
	BaseName  string        `yaml:"base_name"`
	BaseURL   string        `yaml:"base_url"`
	Artifacts []rawArtifact `yaml:"artifacts"`
}

type rawArtifact struct {
	Arch    string `yaml:"arch"`
	Runtime string `yaml:"runtime"`
	URL     string `yaml:"url"`
	Digest  string `yaml:"digest"`
}

func main() {
	tmpDir, err := os.MkdirTemp("", "anvil-manifest-gh-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("Downloading SHA512 sums from %s release %s ...\n", repo, releaseTag)
	cmd := exec.Command("gh", "release", "download", releaseTag,
		"--repo", repo,
		"--pattern", "*.sha512sum",
		"--dir", tmpDir,
		"--clobber")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gh release download failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Make sure `gh` CLI is installed and authenticated.")
		os.Exit(1)
	}

	// Build lookup: filename -> hash
	shaMap := make(map[string]string)
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read temp dir: %v\n", err)
		os.Exit(1)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sha512sum") {
			continue
		}
		path := filepath.Join(tmpDir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) == 2 {
				shaMap[parts[1]] = parts[0]
			}
		}
		f.Close()
	}

	data, err := os.ReadFile(manifestFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read %s: %v\n", manifestFile, err)
		os.Exit(1)
	}

	var manifest rawManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse %s: %v\n", manifestFile, err)
		os.Exit(1)
	}

	updated := 0
	for vKey, version := range manifest.Versions {
		for i := range version.Artifacts {
			art := &version.Artifacts[i]
			filename := art.URL[strings.LastIndex(art.URL, "/")+1:]
			hash, ok := shaMap[filename]
			if !ok {
				fmt.Printf("  [%s %s/%s] WARN: no sha512sum found for %s\n", vKey, art.Arch, art.Runtime, filename)
				continue
			}
			newDigest := "sha512:" + hash
			if art.Digest == newDigest {
				fmt.Printf("  [%s %s/%s] unchanged\n", vKey, art.Arch, art.Runtime)
				continue
			}
			art.Digest = newDigest
			updated++
			fmt.Printf("  [%s %s/%s] updated -> %s...\n", vKey, art.Arch, art.Runtime, hash[:16])
		}
		manifest.Versions[vKey] = version
	}

	if updated == 0 {
		fmt.Println("No changes.")
		return
	}

	out, err := yaml.Marshal(&manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to marshal manifest: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(manifestFile, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write %s: %v\n", manifestFile, err)
		os.Exit(1)
	}

	fmt.Printf("\nUpdated %d artifact(s) in %s\n", updated, manifestFile)
	fmt.Println("Regenerating Go code ...")

	genCmd := exec.Command("go", "run", "generate.go")
	genCmd.Stdout = os.Stdout
	genCmd.Stderr = os.Stderr
	if err := genCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "go generate failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Done.")
}
