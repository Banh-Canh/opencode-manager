package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickael-menu/opencode-manager/internal/agent"
	"github.com/mickael-menu/opencode-manager/internal/config"
)

func TestImprovementPrivateLifecycle(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	r := NewRegistry(cfg)
	if _, err := r.EnsureImprovement(); err == nil {
		t.Fatal("disabled feature should not create a workspace")
	}
	if _, err := os.Stat(r.ImprovementDir()); !os.IsNotExist(err) {
		t.Fatalf("disabled workspace exists: %v", err)
	}
	cfg.SelfImprovement.Enabled = true
	r = NewRegistry(cfg)
	// Startup reconciliation creates the instance without needing Docker/Podman.
	if err := (openCodeConfigSyncer{registry: r}).reconcile(); err != nil {
		t.Fatal(err)
	}
	s, err := r.EnsureImprovement()
	if err != nil {
		t.Fatal(err)
	}
	if s.Manifest.EffectiveDefaultRuntime() != agent.OpenCode || s.Manifest.RuntimeEnabled(agent.DeepSeek) || s.Manifest.RuntimeEnabled(agent.Claude) {
		t.Fatalf("unexpected runtimes: %+v", s.Manifest)
	}
	project := filepath.Join(s.Manifest.HomeDir, "workspace")
	for _, file := range []string{"AGENTS.md", "opencode.json", "sessions.ts", ".opencode/commands/analyze.md", ".opencode/commands/audit-harness.md", ".opencode/commands/proposals.md", ".opencode/agents/harness-improver.md", ".opencode/agents/harness-session-analyst.md", ".opencode/agents/harness-config-analyst.md"} {
		if _, err := os.Stat(filepath.Join(project, file)); err != nil {
			t.Fatalf("missing asset %s: %v", file, err)
		}
	}
	writeTestFile(t, filepath.Join(project, "AGENTS.md"), []byte("custom instructions"))
	writeTestFile(t, filepath.Join(project, "reports", "existing.md"), []byte("prior report"))
	again, err := r.EnsureImprovement()
	if err != nil || again.Manifest.OpenCodePort != s.Manifest.OpenCodePort {
		t.Fatalf("idempotent ensure: %+v, %v", again, err)
	}
	content, _ := os.ReadFile(filepath.Join(project, "AGENTS.md"))
	if string(content) != "custom instructions" {
		t.Fatal("overwrote user instructions")
	}
	ordinary, err := r.Create("internal-self-improvement")
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Manifest.ContainerName == s.Manifest.ContainerName || ordinary.Manifest.OpenCodePort == s.Manifest.OpenCodePort {
		t.Fatal("internal workspace collides with an ordinary workspace")
	}
	listed, err := r.List()
	if err != nil || len(listed) != 1 || listed[0].Path != ordinary.Path {
		t.Fatalf("private workspace leaked into List: %+v, %v", listed, err)
	}
	if err := r.Delete(s); err == nil {
		t.Fatal("ordinary delete must not delete the internal workspace")
	}
	cfg.SelfImprovement.Enabled = false
	if _, err := NewRegistry(cfg).EnsureImprovement(); err == nil {
		t.Fatal("disabled feature should refuse access")
	}
	if _, err := os.Stat(filepath.Join(project, "reports", "existing.md")); err != nil {
		t.Fatal("disable must preserve reports", err)
	}
}

func TestImprovementProvisionMountsAndIsolation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := testConfig(t)
	cfg.SelfImprovement.Enabled = true
	cfg.WorkspaceEnv = map[string]string{"ANALYSIS_MODEL": "test"}
	r := NewRegistry(cfg)
	s, err := r.EnsureImprovement()
	if err != nil {
		t.Fatal(err)
	}
	driver := &specRecordingDriver{fakeDriver: &fakeDriver{}}
	l := Lifecycle{cfg: cfg, registry: r, driver: driver, agents: agent.NewRegistry()}
	_, spec, err := l.provision(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Env["ANALYSIS_MODEL"] != "test" {
		t.Fatal("shared workspace environment not inherited")
	}
	global, _ := config.GlobalDir()
	want := map[string]string{ImprovementConfigMount: global, ImprovementWorkspacesMount: r.WorkspacesDir()}
	for _, mount := range spec.Mounts {
		if source, ok := want[mount.Target]; ok {
			if source != mount.Source || mount.ReadOnly {
				t.Fatalf("incorrect internal mount: %+v", mount)
			}
			delete(want, mount.Target)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing internal mounts: %+v", want)
	}
	ordinary, err := r.Create("ordinary")
	if err != nil {
		t.Fatal(err)
	}
	_, spec, err = l.provision(context.Background(), Summary{Path: ordinary.Path, Manifest: ordinary.Manifest})
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range spec.Mounts {
		if mount.Target == ImprovementConfigMount || mount.Target == ImprovementWorkspacesMount {
			t.Fatalf("private access leaked into ordinary workspace: %+v", mount)
		}
	}
	l.cfg.ExtraMounts = []config.ExtraMount{{Source: t.TempDir(), Target: "/mnt"}}
	if _, _, err := l.provision(context.Background(), s); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("expected mount conflict: %v", err)
	}
	l.cfg.SelfImprovement.Enabled = false
	if _, _, err := l.provision(context.Background(), s); err == nil {
		t.Fatal("disabled internal workspace must not start")
	}
}
