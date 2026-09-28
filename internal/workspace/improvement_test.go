package workspace

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickael-menu/opencode-manager/internal/agent"
	"github.com/mickael-menu/opencode-manager/internal/config"
	"github.com/mickael-menu/opencode-manager/internal/runtime"
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
	global, _ := config.GlobalDir()
	personal := filepath.Join(global, "self-improvement", "AGENTS.md")
	writeTestFile(t, personal, []byte("custom instructions"))
	writeTestFile(t, filepath.Join(project, "opencode.json"), []byte(`{"$schema":"https://opencode.ai/config.json","default_agent":"harness-improver","model":"provider/custom"}`))
	writeTestFile(t, filepath.Join(project, "reports", "existing.md"), []byte("prior report"))
	again, err := r.EnsureImprovement()
	if err != nil || again.Manifest.OpenCodePort != s.Manifest.OpenCodePort {
		t.Fatalf("idempotent ensure: %+v, %v", again, err)
	}
	content, _ := os.ReadFile(filepath.Join(project, "AGENTS.md"))
	if !strings.Contains(string(content), "custom instructions") || !strings.Contains(string(content), "## Analysis workflow") {
		t.Fatal("did not compose default and user instructions")
	}
	content, _ = os.ReadFile(filepath.Join(project, "opencode.json"))
	if !strings.Contains(string(content), "provider/custom") {
		t.Fatal("overwrote local model preference")
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
			if source != mount.Source || mount.ReadOnly != (mount.Target == ImprovementWorkspacesMount) {
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

func TestImprovementInstructionsAndAdditionalRoots(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := testConfig(t)
	cfg.SelfImprovement.Enabled = true
	knowledge := t.TempDir()
	cfg.SelfImprovement.Directories = []config.ImprovementDirectory{{Name: "knowledge", Path: knowledge, Description: "Shared knowledge", ReadOnly: true}}
	r := NewRegistry(cfg)
	s, err := r.EnsureImprovement()
	if err != nil {
		t.Fatal(err)
	}
	global, _ := config.GlobalDir()
	personal := filepath.Join(global, "self-improvement", "AGENTS.md")
	writeTestFile(t, personal, []byte("# My protocol\nFocus on repeated interruptions."))
	cfg.SelfImprovement.Instructions.Mode = "replace"
	r = NewRegistry(cfg)
	if _, err := r.EnsureImprovement(); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(s.Manifest.HomeDir, "workspace")
	data, _ := os.ReadFile(filepath.Join(project, "AGENTS.md"))
	if !strings.Contains(string(data), "Focus on repeated interruptions") || strings.Contains(string(data), "## Analysis workflow") {
		t.Fatalf("replacement instructions: %s", data)
	}
	var settings struct {
		Analysis config.ImprovementAnalysis `json:"analysis"`
		Roots    []struct {
			Name, Path, Description string
			ReadOnly                bool
		} `json:"roots"`
	}
	data, _ = os.ReadFile(filepath.Join(project, "settings.json"))
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Analysis.InitialDays != 7 || settings.Analysis.MaxWorkers != 4 || len(settings.Roots) != 2 || settings.Roots[1].Path != "/mnt/improvement/knowledge" || !settings.Roots[1].ReadOnly {
		t.Fatalf("unexpected settings: %+v", settings)
	}
	l := Lifecycle{cfg: cfg, registry: r}
	mounts, err := l.improvementMounts()
	if err != nil || len(mounts) != 3 || mounts[2].Source != knowledge || !mounts[2].ReadOnly {
		t.Fatalf("mounts: %+v, %v", mounts, err)
	}
	cfg.SelfImprovement.Directories[0].Path = filepath.Join(knowledge, "missing")
	l.cfg = cfg
	if _, err := l.improvementMounts(); err == nil {
		t.Fatal("missing additional directory must be reported")
	}
}

func TestImprovementMigratesLegacyInstructionsOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := testConfig(t)
	cfg.SelfImprovement.Enabled = true
	r := NewRegistry(cfg)
	project := filepath.Join(r.ImprovementDir(), "home", "workspace")
	writeTestFile(t, filepath.Join(project, "AGENTS.md"), []byte("My legacy custom protocol"))
	for i := 0; i < 2; i++ {
		if _, err := r.EnsureImprovement(); err != nil {
			t.Fatal(err)
		}
	}
	global, _ := config.GlobalDir()
	data, _ := os.ReadFile(filepath.Join(global, "self-improvement", "AGENTS.md"))
	if strings.Count(string(data), "My legacy custom protocol") != 1 {
		t.Fatalf("incorrect migration: %s", data)
	}
	data, err := os.ReadFile(filepath.Join(project, "legacy-instructions", "AGENTS.md"))
	if err != nil || string(data) != "My legacy custom protocol" {
		t.Fatalf("missing legacy backup: %s, %v", data, err)
	}
}

type improvementAttachDriver struct{ *fakeDriver }

func (d *improvementAttachDriver) ExecCommand(_ string, args []string) *exec.Cmd {
	return exec.Command(args[0], args[1:]...)
}

func TestImprovementLaunchAndMountDrift(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := testConfig(t)
	cfg.SelfImprovement.Enabled = true
	r := NewRegistry(cfg)
	s, err := r.EnsureImprovement()
	if err != nil {
		t.Fatal(err)
	}
	driver := &improvementAttachDriver{&fakeDriver{output: func([]string) []byte { return nil }}}
	l := Lifecycle{cfg: cfg, registry: r, driver: driver, agents: agent.NewRegistry()}
	command, err := l.AttachRuntimeCommand(context.Background(), s, agent.OpenCode)
	if err != nil {
		t.Fatal(err)
	}
	if command.Args[0] != "opencode" || !contains(command.Args, "--prompt") || !contains(command.Args, "harness-improver") || contains(command.Args, "-c") {
		t.Fatalf("expected fresh prompted improvement session: %v", command.Args)
	}
	_, before, err := l.provision(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	l.cfg.SelfImprovement.Directories = []config.ImprovementDirectory{{Name: "knowledge", Path: t.TempDir()}}
	_, after, err := l.provision(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if before.Env[extraMountsFingerprintEnv] == after.Env[extraMountsFingerprintEnv] {
		t.Fatal("private mount changes must affect drift detection")
	}
	l.driver = &driftDriver{fakeDriver: &fakeDriver{}, rc: runtime.ContainerRuntimeConfig{Env: before.Env, NetworkMode: "bridge"}}
	if !l.containerSpecDrift(context.Background(), s.Manifest, after) {
		t.Fatal("private mount changes should recreate container")
	}
}
