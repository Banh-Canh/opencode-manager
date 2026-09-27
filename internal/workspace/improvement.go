package workspace

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// User-editable prompts are seeded once; the session reader is manager-owned.
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
	if err := seedImprovementAssets(summary.Manifest.HomeDir); err != nil {
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
		target := filepath.Join(home, "workspace", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		data, err := improvementAssets.ReadFile(path)
		if err != nil {
			return err
		}
		if rel == "sessions.ts" {
			previous, err := os.ReadFile(target)
			if err == nil && bytes.Equal(previous, data) {
				return nil
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			// Do not expose a partially rewritten helper to an ongoing analysis.
			temp, err := os.CreateTemp(filepath.Dir(target), ".sessions-*")
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
			return os.Rename(temp.Name(), target)
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
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
	return []runtime.Mount{
		{Source: global, Target: ImprovementConfigMount},
		{Source: root, Target: ImprovementWorkspacesMount},
	}, nil
}
