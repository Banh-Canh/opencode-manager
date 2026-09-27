package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRuntimeRegistry(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{OpenCode, DeepSeek, Claude} {
		provider, err := registry.Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		if provider.Name() != name {
			t.Fatalf("provider name = %q, want %q", provider.Name(), name)
		}
	}
}

func TestClaudeCommands(t *testing.T) {
	provider, _ := NewRegistry().Get(Claude)
	attach, err := provider.AttachCommand()
	if err != nil || fmt.Sprint(attach[4:]) != "[claude]" {
		t.Fatalf("AttachCommand = %v, %v", attach, err)
	}
	run, err := provider.RunCommand("hello")
	if err != nil || fmt.Sprint(run[4:]) != "[claude -p hello]" {
		t.Fatalf("RunCommand = %v, %v", run, err)
	}
}

// Claude Code runs tools itself: it must start with ~/.env loaded, and the
// prompt must reach it verbatim (not through the shell).
func TestClaudeCommandsLoadWorkspaceEnv(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("export OCM_TEST_VAR=from-env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider, _ := NewRegistry().Get(Claude)
	run, _ := provider.RunCommand(`it's "$HOME" ; $(false)`)
	// Swap the claude binary for printf to see what it would get.
	argv := append(append([]string{}, run[:4]...), "sh", "-c", `printf '%s|%s' "$OCM_TEST_VAR" "$2"`, "claude")
	argv = append(argv, run[5:]...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	if got, want := string(out), `from-env|it's "$HOME" ; $(false)`; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestDeepSeekAttachCommandStartsDshTui(t *testing.T) {
	provider, _ := NewRegistry().Get(DeepSeek)
	command, err := provider.AttachCommand()
	if err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	if got, want := fmt.Sprint(command), "[/usr/local/bin/dsh-tui --continue]"; got != want {
		t.Errorf("AttachCommand = %s, want %s", got, want)
	}
}

func TestDeepSeekDoesNotAdvertiseHeadlessRun(t *testing.T) {
	provider, _ := NewRegistry().Get(DeepSeek)
	if _, err := provider.RunCommand("hello"); err == nil {
		t.Fatal("RunCommand returned nil error")
	}
}
