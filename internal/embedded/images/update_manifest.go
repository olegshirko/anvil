//go:build ignore

// update_manifest.go fetches SHA512 checksums for all artifacts in manifest.yaml
// and updates their digest fields.
//
// Usage:
//
//	go run update_manifest.go
package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
	const manifestFile = "manifest.yaml"

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

	client := &http.Client{Timeout: 30 * time.Second}
	updated := false

	for versionKey, version := range manifest.Versions {
		for i := range version.Artifacts {
			art := &version.Artifacts[i]
			shaURL := art.URL + ".sha512sum"

			fmt.Printf("[%s %s/%s] fetching %s ... ", versionKey, art.Arch, art.Runtime, shaURL)
			digest, err := fetchSHA(client, shaURL, art.URL)
			if err != nil {
				fmt.Printf("ERROR: %v\n", err)
				continue
			}

			newDigest := "sha512:" + digest
			if art.Digest == newDigest {
				fmt.Println("unchanged")
				continue
			}

			art.Digest = newDigest
			updated = true
			fmt.Println("updated")
		}
		manifest.Versions[versionKey] = version
	}

	if !updated {
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

	fmt.Printf("\nWrote %s. Regenerating Go code...\n", manifestFile)

	genCmd := exec.Command("go", "run", "generate.go")
	genCmd.Stdout = os.Stdout
	genCmd.Stderr = os.Stderr
	if err := genCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "go generate failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Done.")
}

// fetchSHA downloads the .sha512sum file and extracts the digest for the given artifact URL.
func fetchSHA(client *http.Client, shaURL, artifactURL string) (string, error) {
	resp, err := client.Get(shaURL)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	filename := artifactURL[strings.LastIndex(artifactURL, "/")+1:]

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == filename {
			return parts[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read failed: %w", err)
	}

	return "", fmt.Errorf("digest for %q not found", filename)
}
