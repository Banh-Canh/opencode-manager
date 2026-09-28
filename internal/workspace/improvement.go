package workspace

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mickael-menu/opencode-manager/internal/agent"
	"github.com/mickael-menu/opencode-manager/internal/config"
	"github.com/mickael-menu/opencode-manager/internal/runtime"
)

// These mounts are deliberately stable: the dedicated instructions and commands
// use container paths, never host-specific workspace locations.
const (
	ImprovementConfigMount     = "/mnt/manager-config"
	ImprovementWorkspacesMount = "/mnt/workspaces"
)

//go:embed improvement/* improvement/.opencode/commands/* improvement/.opencode/agents/*
var improvementAssets embed.FS

var improvementMu sync.Mutex

// ImprovementDir is outside WorkspacesDir, keeping the internal instance out of
// ordinary list, selection, deletion, and bulk workspace operations.
func (r Registry) ImprovementDir() string {
	return filepath.Join(r.cfg.WorkspaceRoot, "internal", "self-improvement")
}

func (r Registry) improvementSummary() (Summary, error) {
	path := r.ImprovementDir()
	m, err := LoadManifest(filepath.Join(path, ManifestFile))
	return Summary{Manifest: m, Path: path}, err
}

// EnsureImprovement creates the persistent layout without starting a container.
// The effective protocol is manager-owned; personal instructions are preserved.
func (r Registry) EnsureImprovement() (Summary, error) {
	if !r.cfg.SelfImprovement.Enabled {
		return Summary{}, fmt.Errorf("self-improvement is disabled; set selfImprovement.enabled: true in config.yaml")
	}
	improvementMu.Lock()
	defer improvementMu.Unlock()
	summary, err := r.improvementSummary()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Summary{}, err
	}
	if errors.Is(err, os.ErrNotExist) {
		port, err := r.AllocateOpenCodePort()
		if err != nil {
			return Summary{}, err
		}
		now := time.Now().UTC()
		summary = Summary{Path: r.ImprovementDir(), Manifest: Manifest{
			Name:           "Self improvement",
			Runtime:        r.cfg.Runtime,
			ContainerName:  "opencode-manager--self-improvement",
			ImageName:      "opencode-manager/internal-self-improvement:latest",
			Image:          imageConfigFromConfig(r.cfg),
			HomeDir:        filepath.Join(r.ImprovementDir(), workspaceHomeSubdir),
			OpenCodePort:   port,
			DefaultRuntime: agent.OpenCode,
			Runtimes:       RuntimeConfigMap{agent.OpenCode: {Enabled: true}, agent.Claude: {Enabled: false}},
			Env:            map[string]string{}, CreatedAt: now, UpdatedAt: now,
		}}
	}
	if err := r.createLayout(summary.Path); err != nil {
		return Summary{}, err
	}
	if err := os.MkdirAll(r.WorkspacesDir(), 0o700); err != nil {
		return Summary{}, err
	}
	if err := r.configureImprovement(summary.Manifest.HomeDir); err != nil {
		return Summary{}, err
	}
	if _, err := os.Stat(filepath.Join(summary.Path, ManifestFile)); errors.Is(err, os.ErrNotExist) {
		if err := SaveManifest(filepath.Join(summary.Path, ManifestFile), summary.Manifest); err != nil {
			return Summary{}, err
		}
	}
	return summary, nil
}

func seedImprovementAssets(home string) error {
	return fs.WalkDir(improvementAssets, "improvement", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(path, "improvement/")
		if rel == "AGENTS.md" { // composed with personal instructions below
			return nil
		}
		target := filepath.Join(home, "workspace", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		data, err := improvementAssets.ReadFile(path)
		if err != nil {
			return err
		}
		previous, err := os.ReadFile(target)
		if rel == "opencode.json" && err == nil {
			// Keep local model/provider preferences from the previous seeded config.
			return nil
		}
		if err == nil && bytes.Equal(previous, data) {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && !strings.HasSuffix(rel, ".ts") {
			// Preserve old/custom prompts before installing the current protocol.
			backup := filepath.Join(home, "workspace", "legacy-instructions", fmt.Sprintf("%x", sha256.Sum256(previous)), filepath.FromSlash(rel))
			if err := writeImprovementFile(backup, previous); err != nil {
				return err
			}
		}
		return writeImprovementFile(target, data)
	})
}

func writeImprovementFile(file string, data []byte) error {
	if old, err := os.ReadFile(file); err == nil {
		if bytes.Equal(old, data) {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(file), ".improvement-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, writeErr := temp.Write(data)
	closeErr := temp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), file)
}

func (r Registry) configureImprovement(home string) error {
	global, err := config.GlobalDir()
	if err != nil {
		return err
	}
	project := filepath.Join(home, "workspace")
	personalPath := filepath.Join(global, "self-improvement", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(personalPath), 0o700); err != nil {
		return err
	}
	personal, err := os.ReadFile(personalPath)
	if errors.Is(err, os.ErrNotExist) {
		personal = []byte("# Personal self-improvement instructions\n\n<!-- Add priorities, context, and proposal preferences here. OCM preserves this file. -->\n")
	} else if err != nil {
		return err
	}
	marker := filepath.Join(project, ".instructions-v2")
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		if legacy, err := os.ReadFile(filepath.Join(project, "AGENTS.md")); err == nil {
			if err := writeImprovementFile(filepath.Join(project, "legacy-instructions", "AGENTS.md"), legacy); err != nil {
				return err
			}
			// Migrate customized V1 instructions, but not the obsolete stock protocol.
			if fmt.Sprintf("%x", sha256.Sum256(legacy)) != "d1a35aea56bc0cea7bd783a4d52d0e465a6ede2dfc0070a922cc1ed04c6d6a95" && !bytes.Contains(personal, legacy) {
				personal = append(personal, append([]byte("\n\n# Migrated personal instructions\n\n"), legacy...)...)
			}
		}
	}
	if err := writeImprovementFile(personalPath, personal); err != nil {
		return err
	}
	meaningful := strings.TrimSpace(regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(string(personal), ""))
	if r.cfg.SelfImprovement.Instructions.Mode == "replace" && (meaningful == "" || meaningful == "# Personal self-improvement instructions") {
		return fmt.Errorf("replace mode requires personal instructions in %s", personalPath)
	}
	if err := seedImprovementAssets(home); err != nil {
		return err
	}
	base, err := improvementAssets.ReadFile("improvement/AGENTS.md")
	if err != nil {
		return err
	}
	if r.cfg.SelfImprovement.Instructions.Mode == "replace" {
		base = nil
	}
	effective := append(base, []byte("\n\n# Personal instructions (take precedence for analysis preferences)\n\n")...)
	effective = append(effective, personal...)
	if err := writeImprovementFile(filepath.Join(project, "AGENTS.md"), effective); err != nil {
		return err
	}
	if err := writeImprovementFile(marker, []byte("2\n")); err != nil {
		return err
	}
	type root struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		Description string `json:"description"`
		ReadOnly    bool   `json:"readOnly"`
	}
	roots := []root{{Name: "manager-config", Path: ImprovementConfigMount, Description: "Entire OCM configuration, harnesses and knowledge base"}}
	for _, directory := range r.cfg.SelfImprovement.Directories {
		roots = append(roots, root{directory.Name, "/mnt/improvement/" + directory.Name, directory.Description, directory.ReadOnly})
	}
	settings, err := json.MarshalIndent(struct {
		Analysis config.ImprovementAnalysis `json:"analysis"`
		Roots    []root                     `json:"roots"`
	}{r.cfg.SelfImprovement.Analysis.WithDefaults(), roots}, "", "  ")
	if err != nil {
		return err
	}
	return writeImprovementFile(filepath.Join(project, "settings.json"), settings)
}

func (l Lifecycle) isImprovement(summary Summary) bool {
	return filepath.Clean(summary.Path) == filepath.Clean(l.registry.ImprovementDir())
}

func (l Lifecycle) improvementMounts() ([]runtime.Mount, error) {
	global, err := config.GlobalDir()
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(l.registry.WorkspacesDir())
	if err != nil {
		return nil, err
	}
	mounts := []runtime.Mount{
		{Source: global, Target: ImprovementConfigMount},
		{Source: root, Target: ImprovementWorkspacesMount, ReadOnly: true},
	}
	for _, directory := range l.cfg.SelfImprovement.Directories {
		source, err := directory.HostPath()
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(source)
		if err != nil {
			return nil, fmt.Errorf("self-improvement directory %q: %w", directory.Name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("self-improvement directory %q is not a directory", directory.Name)
		}
		mounts = append(mounts, runtime.Mount{Source: source, Target: "/mnt/improvement/" + directory.Name, ReadOnly: directory.ReadOnly})
	}
	return mounts, nil
}
