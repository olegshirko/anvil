package downloader

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"anvil/internal/util"
)

// SHA is the shasum of a file.
type SHA struct {
	Digest string // shasum
	URL    string // url to download the shasum file (if Digest is empty)
	Size   int    // one of 256 or 512
}

// ValidateFile validates the SHA of the file.
func (s SHA) ValidateFile(host hostActions, file string) error {
	dir, filename := filepath.Split(file)
	digest := strings.TrimPrefix(s.Digest, fmt.Sprintf("sha%d:", s.Size))
	shasumBinary := "shasum"
	if util.IsMacOS() {
		shasumBinary = "/usr/bin/shasum"
	}

	script := strings.NewReplacer(
		"{dir}", dir,
		"{digest}", digest,
		"{size}", strconv.Itoa(s.Size),
		"{filename}", filename,
		"{shasum_bin}", shasumBinary,
	).Replace(
		`cd {dir} && echo "{digest}  {filename}" | {shasum_bin} -a {size} --check --status`,
	)

	return host.Run("sh", "-c", script)
}

func (s SHA) validateDownload(host hostActions, url string, filename string) error {
	if s.URL == "" && s.Digest == "" {
		return fmt.Errorf("error validating SHA: one of Digest or URL must be set")
	}

	actualFilename := ""
	if url != "" {
		split := strings.Split(url, "/")
		actualFilename = split[len(split)-1]
	}

	// fetch digest from URL if empty
	if s.Digest == "" {
		digest, err := fetchSHAFromURLNative(s.URL, actualFilename)
		if err != nil {
			return err
		}
		s.Digest = digest
	}

	dir, downloadingName := filepath.Split(filename)
	// shasum --check expects the filename in the checksum line to match the actual file.
	// The downloaded file is named <sha256>.downloading, but the SHA file references the original filename.
	// Create a temporary symlink with the expected name to satisfy shasum --check.
	if actualFilename != "" && actualFilename != downloadingName {
		symlink := filepath.Join(dir, actualFilename)
		if err := os.Symlink(downloadingName, symlink); err != nil && !os.IsExist(err) {
			return fmt.Errorf("error creating symlink for SHA validation: %w", err)
		}
		defer func() { _ = os.Remove(symlink) }()
		return s.ValidateFile(host, symlink)
	}

	return s.ValidateFile(host, filename)
}

// fetchSHAFromURLNative fetches the SHA from the URL using native Go http client.
func fetchSHAFromURLNative(url, filename string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch SHA from URL '%s': %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch SHA from URL '%s': status code %d", url, resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == filename {
			return parts[0], nil // The SHA digest is the first part of the line
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading response body from '%s': %w", url, err)
	}

	return "", fmt.Errorf("SHA for filename '%s' not found in '%s'", filename, url)
}
