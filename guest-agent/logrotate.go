package main

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// In debug mode (ANVIL_DEBUG=1) stage2 appends guest-agent's stdout/stderr to
// guest-agent.log on the virtiofs share. Without a cap a long debug session
// grows that file on the host disk without bound, so the agent takes the file
// over: it points its own stdout/stderr at a managed descriptor and rotates
// the log once it exceeds debugLogMaxBytes, keeping a single backup.
const (
	debugLogPath       = anvilRunDir + "/guest-agent.log"
	debugLogMaxBytes   = 50 << 20 // 50 MiB
	debugLogCheckEvery = time.Minute
)

// setupDebugLogRotation redirects stdout/stderr into the managed debug log
// and rotates it by size. No-op outside debug mode; on failure the shell
// redirect from stage2 keeps working unrotated.
func setupDebugLogRotation() {
	if os.Getenv("ANVIL_DEBUG") == "" {
		return
	}
	if err := rotateDebugLog(); err != nil {
		log.Printf("[log] initial rotation failed: %v", err)
	}
	go func() {
		for range time.Tick(debugLogCheckEvery) {
			info, err := os.Stat(debugLogPath)
			if err == nil && info.Size() > debugLogMaxBytes {
				if err := rotateDebugLog(); err != nil {
					log.Printf("[log] rotation failed: %v", err)
				}
			}
		}
	}()
}

// rotateDebugLog moves an oversized log aside and reopens stdout/stderr onto
// a fresh file.
func rotateDebugLog() error {
	if info, err := os.Stat(debugLogPath); err == nil && info.Size() > debugLogMaxBytes {
		os.Remove(debugLogPath + ".1")
		if err := os.Rename(debugLogPath, debugLogPath+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(debugLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// The directory may not exist yet (first debug boot with the
		// .anvil-run layout); create it and retry once.
		if os.MkdirAll(filepath.Dir(debugLogPath), 0o755) != nil {
			return err
		}
		if f, err = os.OpenFile(debugLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			return err
		}
	}
	// The log package and runtime panics write to fds 1/2, so dup covers
	// both (see dupfd_*.go — dup3 on linux, dup2 elsewhere).
	if err := dupFD(int(f.Fd()), int(os.Stdout.Fd())); err != nil {
		f.Close()
		return err
	}
	if err := dupFD(int(f.Fd()), int(os.Stderr.Fd())); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return nil
}
