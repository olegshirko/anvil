package downloader

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"anvil/internal/environment"
	"crypto/sha256"
	"encoding/hex"

	"anvil/internal/util/terminal"

	"github.com/sirupsen/logrus"
)

type (
	hostActions  = environment.HostActions
	guestActions = environment.GuestActions
)

// EnableParallel toggles multi-threaded (parallel) downloading.
// When false (default), all downloads use a single thread.
var EnableParallel bool

// Request is download request
type Request struct {
	URL string // request URL
	SHA *SHA   // shasum url
}

// DownloadToGuest downloads file at url and saves it in the destination.
//
// In the implementation, the file is downloaded (and cached) on the host, but copied to the desired
// destination for the guest.
// filename must be an absolute path and a directory on the guest that does not require root access.
func DownloadToGuest(host hostActions, guest guestActions, log *logrus.Logger, cacheDir string, r Request, filename string) error {
	// if file is on the filesystem, no need for download. A copy suffices
	if strings.HasPrefix(r.URL, "/") {
		return guest.RunQuiet("cp", r.URL, filename)
	}

	cacheFile, err := Download(host, log, cacheDir, r)
	if err != nil {
		return err
	}

	return guest.RunQuiet("cp", cacheFile, filename)
}

// Download downloads file at url and returns the location of the downloaded file.
func Download(host hostActions, log *logrus.Logger, cacheDir string, r Request) (string, error) {
	d := downloader{
		host:     host,
		log:      log,
		cacheDir: cacheDir,
	}

	if !d.hasCache(r.URL) {
		if err := d.downloadFile(r); err != nil {
			return "", fmt.Errorf("error downloading '%s': %w", r.URL, err)
		}
	}

	return CacheFilename(cacheDir, r.URL), nil
}

type downloader struct {
	host     hostActions
	log      *logrus.Logger
	cacheDir string
}

// CacheFilename returns the computed filename for the url.
func CacheFilename(cacheDir, url string) string {
	h := sha256.Sum256([]byte(url))
	return filepath.Join(cacheDir, "caches", hex.EncodeToString(h[:]))
}

func (d downloader) cacheDownloadingFileName(url string) string {
	return CacheFilename(d.cacheDir, url) + ".downloading"
}

func (d downloader) downloadFile(r Request) (err error) {
	basename := path.Base(r.URL)

	// retry transient failures up to 3 times
	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			d.log.Warnf("retrying download of %s (attempt %d/%d)...", basename, attempt, maxRetries)
			time.Sleep(time.Duration(attempt-1) * time.Second)
		}

		err = d.downloadFileAttempt(r, basename)
		if err == nil {
			return nil
		}
		// do not retry client errors (4xx) except 408/429
		if isNonRetryable(err) {
			break
		}
	}
	return err
}

func isNonRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// simplistic check: 4xx errors other than 408/429/5xx should not be retried
	if strings.Contains(s, "status code 4") {
		if strings.Contains(s, "408") || strings.Contains(s, "429") {
			return false
		}
		return true
	}
	return false
}

func (d downloader) downloadFileAttempt(r Request, basename string) (err error) {
	d.log.Infof("downloading %s from %s ...", basename, r.URL)

	cacheDownloadingFilename := d.cacheDownloadingFileName(r.URL)
	if err := os.MkdirAll(filepath.Dir(cacheDownloadingFilename), 0750); err != nil {
		return fmt.Errorf("error preparing cache dir: %w", err)
	}

	// Check if we can resume an existing partial download
	fi, err := os.Stat(cacheDownloadingFilename)
	if err == nil && fi.Size() > 0 {
		// If there's a partial file, use single-threaded resume to avoid complexity
		return d.downloadSingleThreaded(r, basename, cacheDownloadingFilename, fi.Size())
	}

	// Try parallel download only when explicitly enabled.
	if EnableParallel {
		if err := d.downloadParallel(r, basename, cacheDownloadingFilename); err == nil {
			return d.finalizeDownload(r, basename, cacheDownloadingFilename)
		}
		d.log.Warnf("parallel download failed, falling back to single-threaded: %v", err)
		_ = os.Remove(cacheDownloadingFilename)
	}

	// Single-threaded download (default)
	if err := d.downloadSingleThreaded(r, basename, cacheDownloadingFilename, 0); err != nil {
		return err
	}
	return d.finalizeDownload(r, basename, cacheDownloadingFilename)
}

func (d downloader) downloadParallel(r Request, basename, filename string) error {
	const (
		workers      = 8
		minChunkSize = 2 * 1024 * 1024 // 2 MB
	)

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	// Resolve redirects manually so we can probe the final CDN URL for Range support.
	headClient := &http.Client{
		Transport:     transport,
		Timeout:       30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest("HEAD", r.URL, nil)
	if err != nil {
		return err
	}
	resp, err := headClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	// If we got a redirect, follow it manually for the actual download URL.
	downloadURL := r.URL
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if loc != "" {
			downloadURL = loc
		}
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HEAD status %d", resp.StatusCode)
	}

	// Probe the final URL for Range support and file size.
	req2, err := http.NewRequest("HEAD", downloadURL, nil)
	if err != nil {
		return err
	}
	resp2, err := headClient.Do(req2)
	if err != nil {
		return err
	}
	_ = resp2.Body.Close()

	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		return fmt.Errorf("HEAD status %d", resp2.StatusCode)
	}

	acceptRanges := resp2.Header.Get("Accept-Ranges")
	contentLength := resp2.ContentLength
	if acceptRanges != "bytes" || contentLength <= 0 {
		return fmt.Errorf("server does not support parallel download")
	}

	if contentLength < int64(workers*minChunkSize) {
		return fmt.Errorf("file too small for parallel download")
	}

	// Create destination file
	destFile, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("error creating destination file: %w", err)
	}
	defer func() { _ = destFile.Close() }()

	// Pre-allocate file size (best effort)
	_ = destFile.Truncate(contentLength)

	chunkSize := contentLength / int64(workers)
	if chunkSize < int64(minChunkSize) {
		chunkSize = int64(minChunkSize)
	}

	var downloaded atomic.Int64
	progress := newProgress(d.log, basename, contentLength, 0)
	defer progress.Finish()

	var wg sync.WaitGroup
	errChan := make(chan error, workers)

	for i := 0; i < workers; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == workers-1 {
			end = contentLength - 1
		}

		wg.Add(1)
		go func(start, end int64) {
			defer wg.Done()
			if err := d.downloadChunk(downloadURL, destFile, start, end, &downloaded, progress); err != nil {
				errChan <- err
			}
		}(start, end)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		return err
	}

	return nil
}

func (d downloader) downloadChunk(url string, destFile *os.File, start, end int64, downloaded *atomic.Int64, progress *Progress) error {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chunk status %d", resp.StatusCode)
	}

	buf := make([]byte, 64*1024)
	offset := start
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := destFile.WriteAt(buf[:n], offset); werr != nil {
				return werr
			}
			offset += int64(n)
			downloaded.Add(int64(n))
			progress.Update(downloaded.Load())
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d downloader) downloadSingleThreaded(r Request, basename, filename string, resumeFrom int64) error {
	flags := os.O_CREATE | os.O_WRONLY
	if resumeFrom == 0 {
		flags |= os.O_TRUNC
	}
	destFile, err := os.OpenFile(filename, flags, 0600) //nolint:gosec
	if err != nil {
		return fmt.Errorf("error creating destination file: %w", err)
	}
	defer func() { _ = destFile.Close() }()

	req, err := http.NewRequest("GET", r.URL, nil)
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}
	if resumeFrom > 0 {
		d.log.Tracef("resuming download from byte %d", resumeFrom)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error during download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		if err := destFile.Truncate(0); err != nil {
			return fmt.Errorf("error truncating file: %w", err)
		}
		resumeFrom = 0
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	if resp.StatusCode == http.StatusPartialContent {
		if _, err := destFile.Seek(0, io.SeekEnd); err != nil {
			return fmt.Errorf("error seeking to end of file: %w", err)
		}
	} else {
		if _, err := destFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("error seeking to start of file: %w", err)
		}
	}

	progress := newProgress(d.log, basename, resp.ContentLength+resumeFrom, resumeFrom)
	reader := io.TeeReader(resp.Body, progress)

	if _, err := io.Copy(destFile, reader); err != nil {
		return fmt.Errorf("error writing to file: %w", err)
	}
	progress.Finish()
	return nil
}

func (d downloader) finalizeDownload(r Request, basename, cacheDownloadingFilename string) error {
	if r.SHA != nil {
		if err := r.SHA.validateDownload(d.host, r.URL, cacheDownloadingFilename); err != nil {
			_ = os.Rename(cacheDownloadingFilename, cacheDownloadingFilename+".invalid")
			return fmt.Errorf("error validating SHA sum for '%s': %w", basename, err)
		}
	}

	d.log.Infof("downloaded %s", basename)
	return os.Rename(cacheDownloadingFilename, CacheFilename(d.cacheDir, r.URL))
}

// Progress tracks download progress.
type Progress struct {
	Total      int64 // total size
	Current    int64 // downloaded size
	mu         sync.Mutex
	lastReport time.Time
	logger     *logrus.Logger
	basename   string
	startTime  time.Time
	startSize  int64
}

// newProgress creates a new Progress.
func newProgress(logger *logrus.Logger, basename string, total, current int64) *Progress {
	return &Progress{
		Total:     total,
		Current:   current,
		logger:    logger,
		basename:  basename,
		startTime: time.Now(),
		startSize: current,
	}
}

// Write implements io.Writer.
func (p *Progress) Write(b []byte) (int, error) {
	n := len(b)
	p.mu.Lock()
	defer p.mu.Unlock()

	p.Current += int64(n)
	p.report()
	return n, nil
}

// Update updates the current progress from an external counter (e.g. parallel download).
func (p *Progress) Update(current int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.Current = current
	p.report()
}

func (p *Progress) report() {
	if time.Since(p.lastReport) < (time.Second / 2) {
		return
	}
	p.lastReport = time.Now()
	fmt.Printf("\rdownloading %s ... %s ", p.basename, p.format())
}

func (p *Progress) format() string {
	pct := terminal.Progress(p.Current, p.Total)
	current := terminal.HumanBytes(p.Current)
	total := terminal.HumanBytes(p.Total)

	elapsed := time.Since(p.startTime).Seconds()
	speed := ""
	eta := ""
	if elapsed > 0 {
		bytesPerSec := float64(p.Current-p.startSize) / elapsed
		if bytesPerSec > 0 {
			speed = terminal.HumanBytes(int64(bytesPerSec)) + "/s"
			if p.Total > 0 {
				remaining := p.Total - p.Current
				secondsLeft := float64(remaining) / bytesPerSec
				eta = time.Duration(secondsLeft * float64(time.Second)).Round(time.Second).String()
			}
		}
	}

	parts := []string{pct}
	if p.Total > 0 {
		parts = append(parts, fmt.Sprintf("%s / %s", current, total))
	}
	if speed != "" {
		parts = append(parts, speed)
	}
	if eta != "" {
		parts = append(parts, "ETA "+eta)
	}
	return strings.Join(parts, ", ")
}

// Finish prints the final done message and clears the progress line.
func (p *Progress) Finish() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.Total > 0 {
		fmt.Printf("\rdownloaded %s (%s) done\n", p.basename, terminal.HumanBytes(p.Total))
	} else {
		fmt.Printf("\rdownloaded %s done\n", p.basename)
	}
}

func (d downloader) hasCache(url string) bool {
	_, err := os.Stat(CacheFilename(d.cacheDir, url))
	return err == nil
}
