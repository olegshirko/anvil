package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"anvil/internal/cli"
	"anvil/internal/domain"

	log "github.com/sirupsen/logrus"
)

// waitForUserEdit launches a temporary file with content using editor,
// and waits for the user to close the editor.
func waitForUserEdit(editor string, content []byte) (string, error) {
	tmp, err := os.CreateTemp("", "anvil-*.yaml")
	if err != nil {
		return "", fmt.Errorf("error creating temporary file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		return "", fmt.Errorf("error writing temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("error closing temporary file: %w", err)
	}

	if err := launchEditor(editor, tmp.Name()); err != nil {
		return "", err
	}

	if f, err := os.ReadFile(tmp.Name()); err == nil && len(bytes.TrimSpace(f)) == 0 {
		return "", nil
	}

	return tmp.Name(), nil
}

var editors = []string{
	"vim",
	"code --wait --new-window",
	"nano",
}

func launchEditor(editor string, file string) error {
	if editor != "" {
		log.Println("editing in", editor)
	}
	if editor == "" {
		if os.Getenv("TERM_PROGRAM") == "vscode" {
			log.Println("vscode detected, editing in vscode")
			editor = "code --wait"
		}
	}
	if editor == "" {
		if e := os.Getenv("EDITOR"); e != "" {
			log.Println("editing in", e, "from", "$EDITOR environment variable")
			editor = e
		}
	}
	if editor == "" {
		for _, e := range editors {
			s := strings.Fields(e)
			if _, err := exec.LookPath(s[0]); err == nil {
				editor = e
				log.Println("editing in", e)
				break
			}
		}
	}
	if editor == "" {
		return fmt.Errorf("no editor found in $PATH, kindly set $EDITOR environment variable and try again")
	}

	switch editor {
	case "code", "code-insiders", "code-oss", "codium", "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code":
		editor = strconv.Quote(editor) + " --wait --new-window"
	case "mate", "/Applications/TextMate 2.app/Contents/MacOS/mate", "/Applications/TextMate 2.app/Contents/MacOS/TextMate":
		editor = strconv.Quote(editor) + " --wait"
	}

	return cli.Exec("sh", "-c", editor+" "+file).Interactive().Run()
}

// dnsHostsFromFlag converts dns hosts from cli flag format to config file format
func dnsHostsFromFlag(hosts []string) map[string]string {
	mapping := make(map[string]string, len(hosts))
	for _, host := range hosts {
		str := strings.SplitN(host, "=", 2)
		if len(str) != 2 {
			log.Warnf("unable to parse custom dns host: %v, skipping\n", host)
			continue
		}
		mapping[str[0]] = str[1]
	}
	return mapping
}

// mountsFromFlag converts mounts from cli flag format to config file format
func mountsFromFlag(mounts []string) []domain.Mount {
	mnts := make([]domain.Mount, len(mounts))
	for i, mount := range mounts {
		str := strings.SplitN(mount, ":", 3)
		mnt := domain.Mount{Location: str[0]}
		if len(str) > 1 {
			if filepath.IsAbs(str[1]) {
				mnt.MountPoint = str[1]
			} else if str[1] == "w" {
				mnt.Writable = true
			}
		}
		if len(str) > 2 && str[2] == "w" {
			mnt.Writable = true
		}
		mnts[i] = mnt
	}
	return mnts
}
