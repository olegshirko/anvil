package debutil

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"anvil/internal/cli"
	"anvil/internal/domain"
	"anvil/internal/environment"
)

type mockGuest struct {
	commands    [][]string
	binaryCheck string // if set, "command -v <binaryCheck>" returns nil; others return error
}

func (m *mockGuest) Run(args ...string) error { panic("unexpected Run") }
func (m *mockGuest) RunQuiet(args ...string) error {
	m.commands = append(m.commands, args)
	// simulate binary existence check
	if len(args) == 3 && args[0] == "command" && args[1] == "-v" {
		if args[2] == m.binaryCheck {
			return nil // binary exists
		}
		return errors.New("not found") // binary missing
	}
	// simulate packages not installed by default
	if len(args) == 3 && args[0] == "sh" && args[1] == "-c" && strings.Contains(args[2], "dpkg-query") {
		return errors.New("packages not installed")
	}
	return nil // all other commands succeed
}
func (m *mockGuest) RunOutput(args ...string) (string, error) { panic("unexpected RunOutput") }
func (m *mockGuest) RunInteractive(args ...string) error      { panic("unexpected RunInteractive") }
func (m *mockGuest) RunWith(stdin io.Reader, stdout io.Writer, args ...string) error {
	panic("unexpected RunWith")
}
func (m *mockGuest) Read(fileName string) (string, error)                { panic("unexpected Read") }
func (m *mockGuest) Write(fileName string, body []byte) error            { panic("unexpected Write") }
func (m *mockGuest) Stat(fileName string) (os.FileInfo, error)           { panic("unexpected Stat") }
func (m *mockGuest) Start(ctx context.Context, conf domain.Config) error { panic("unexpected Start") }
func (m *mockGuest) Stop(ctx context.Context, force bool) error          { panic("unexpected Stop") }
func (m *mockGuest) Restart(ctx context.Context) error                   { panic("unexpected Restart") }
func (m *mockGuest) SSH(workingDir string, args ...string) error         { panic("unexpected SSH") }
func (m *mockGuest) Created() bool                                       { panic("unexpected Created") }
func (m *mockGuest) Running(ctx context.Context) bool                    { panic("unexpected Running") }
func (m *mockGuest) Env(string) (string, error)                          { panic("unexpected Env") }
func (m *mockGuest) Setting(key string) string                           { panic("unexpected Setting") }
func (m *mockGuest) SetSetting(key, value string) error                  { panic("unexpected SetSetting") }
func (m *mockGuest) User() (string, error)                               { panic("unexpected User") }
func (m *mockGuest) Arch() environment.Arch                              { panic("unexpected Arch") }

func TestEnsurePackages_SkipsWhenBinaryExists(t *testing.T) {
	guest := &mockGuest{binaryCheck: "docker"}
	chain := cli.New("test")

	updated, err := EnsurePackages(context.Background(), guest, chain, "docker", "docker-ce")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated {
		t.Error("expected updated=false when binary exists")
	}

	// Should only run "command -v docker", no apt commands
	if len(guest.commands) != 1 {
		t.Fatalf("expected 1 command, got %d: %v", len(guest.commands), guest.commands)
	}
	if len(guest.commands[0]) != 3 || guest.commands[0][0] != "command" || guest.commands[0][1] != "-v" || guest.commands[0][2] != "docker" {
		t.Errorf("expected 'command -v docker', got %v", guest.commands[0])
	}
}

func TestEnsurePackages_RunsWhenBinaryMissing(t *testing.T) {
	guest := &mockGuest{binaryCheck: ""}
	chain := cli.New("test")

	updated, err := EnsurePackages(context.Background(), guest, chain, "nonexistent-binary-12345", "docker-ce")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !updated {
		t.Error("expected updated=true because mock apt commands succeed")
	}

	// Should run binary check + dpkg-query + apt-get update + apt list + apt install = 5 commands
	if len(guest.commands) != 5 {
		t.Fatalf("expected 5 commands, got %d: %v", len(guest.commands), guest.commands)
	}

	// First command is binary check
	if len(guest.commands[0]) != 3 || guest.commands[0][0] != "command" || guest.commands[0][2] != "nonexistent-binary-12345" {
		t.Errorf("expected binary check first, got %v", guest.commands[0])
	}

	// Second command is dpkg-query check
	if !contains(guest.commands[1], "dpkg-query") {
		t.Errorf("expected dpkg-query check, got %v", guest.commands[1])
	}

	// Third command is apt-get update
	if !contains(guest.commands[2], "apt-get update") {
		t.Errorf("expected apt-get update, got %v", guest.commands[2])
	}
}

func TestEnsurePackages_ForcesUpdateWhenBinaryCheckEmpty(t *testing.T) {
	guest := &mockGuest{binaryCheck: ""}
	chain := cli.New("test")

	_, err := EnsurePackages(context.Background(), guest, chain, "", "docker-ce")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should skip binary check, run dpkg-query, then apt commands
	if len(guest.commands) != 4 {
		t.Fatalf("expected 4 commands, got %d: %v", len(guest.commands), guest.commands)
	}

	// First command should be dpkg-query (no binary check)
	if !contains(guest.commands[0], "dpkg-query") {
		t.Errorf("expected dpkg-query first, got %v", guest.commands[0])
	}

	// Second command should be apt-get update
	if !contains(guest.commands[1], "apt-get update") {
		t.Errorf("expected apt-get update second, got %v", guest.commands[1])
	}
}

func contains(args []string, substr string) bool {
	for _, a := range args {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}
