package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickael-menu/opencode-manager/internal/agent"
	"github.com/mickael-menu/opencode-manager/internal/config"
	"github.com/mickael-menu/opencode-manager/internal/runtime"
)

func TestUpdateWorkspaceImageReconcilesModulesBeforeReturning(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "install failure"}[fail], func(t *testing.T) {
			path := t.TempDir()
			summary := Summary{Path: path, Manifest: Manifest{
				Name: "demo", ContainerName: "demo", ImageName: "ocm/demo:latest",
				HomeDir: filepath.Join(path, "home"), OpenCodePort: 4096,
				Modules: []ModuleInstance{{Name: "golang", Category: "tools", Version: 1}},
			}}
			if err := SaveManifest(filepath.Join(path, ManifestFile), summary.Manifest); err != nil {
				t.Fatal(err)
			}
			driver := &updateModuleDriver{updateDriver: &updateDriver{fakeDriver: &fakeDriver{}}, fail: fail}
			l := Lifecycle{cfg: config.Config{BaseImage: config.BaseImageConfig{Name: config.DefaultBaseImage}}, driver: driver}
			err := l.UpdateWorkspaceImage(context.Background(), summary)
			if fail {
				if err == nil || !strings.Contains(err.Error(), "install failed") {
					t.Fatalf("update error = %v", err)
				}
				if driver.marked {
					t.Fatal("failed install must not be marked complete")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !driver.marked {
					t.Fatal("update returned before module marker was written")
				}
				if err := l.EnsureStarted(context.Background(), summary); err != nil {
					t.Fatal(err)
				}
				if driver.removed != 1 {
					t.Fatal("subsequent use recreated the container")
				}
			}
			if driver.installs != 1 {
				t.Fatalf("installs = %d, want 1 during update", driver.installs)
			}
		})
	}
}

type updateModuleDriver struct {
	*updateDriver
	fail     bool
	installs int
	marked   bool
}

func (d *updateModuleDriver) Exec(_ context.Context, spec runtime.ExecSpec) ([]byte, error) {
	args := strings.Join(spec.Args, " ")
	switch {
	case strings.HasSuffix(args, "/golang/install"):
		d.installs++
		if d.fail {
			return nil, errors.New("install failed")
		}
	case strings.Contains(args, "base64 -d > "+markerPath):
		d.marked = true
	case strings.Contains(args, "cat "+markerPath) && d.marked:
		return []byte(`[{"name":"golang","version":1}]`), nil
	}
	return nil, nil
}

func TestUpdateWorkspaceImageRefreshesBaseAndRecreatesContainer(t *testing.T) {
	driver := &updateDriver{fakeDriver: &fakeDriver{}}
	path := t.TempDir()
	home := filepath.Join(path, "home")
	summary := Summary{Manifest: Manifest{
		Name:          "demo",
		ImageName:     "ocm/demo:latest",
		Image:         ImageConfig{BaseImage: "docker.io/mroger78/ocm-base:0.7.0"},
		ContainerName: "demo",
		HomeDir:       home,
		OpenCodePort:  4096,
	}, Path: path}
	if err := SaveManifest(filepath.Join(path, ManifestFile), summary.Manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	l := Lifecycle{cfg: config.Config{Runtime: config.RuntimeDocker, BaseImage: config.BaseImageConfig{Name: config.DefaultBaseImage}}, driver: driver, agents: agent.NewRegistry()}

	if err := l.UpdateWorkspaceImage(context.Background(), summary); err != nil {
		t.Fatalf("UpdateWorkspaceImage error: %v", err)
	}
	if len(driver.pulled) != 1 || driver.pulled[0] != config.DefaultBaseImage {
		t.Fatalf("pulls = %v, want forced pull of %s", driver.pulled, config.DefaultBaseImage)
	}
	if len(driver.builds) != 1 || driver.builds[0].BaseImage != config.DefaultBaseImage || !driver.builds[0].Refresh {
		t.Fatalf("workspace builds = %#v", driver.builds)
	}
	if driver.removed != 1 || driver.created != 1 || driver.started != 1 {
		t.Fatalf("replacement = remove:%d create:%d start:%d, want 1 each", driver.removed, driver.created, driver.started)
	}
	updated, err := LoadManifest(filepath.Join(path, ManifestFile))
	if err != nil {
		t.Fatalf("load updated manifest: %v", err)
	}
	if updated.Image.BaseImage != config.DefaultBaseImage {
		t.Fatalf("updated base image = %q, want %q", updated.Image.BaseImage, config.DefaultBaseImage)
	}
}

type updateDriver struct {
	*fakeDriver
	pulled  []string
	builds  []runtime.BuildSpec
	removed int
	created int
}

func (d *updateDriver) PullImage(_ context.Context, ref string) error {
	d.pulled = append(d.pulled, ref)
	return nil
}

func (d *updateDriver) BuildImage(_ context.Context, spec runtime.BuildSpec) error {
	d.builds = append(d.builds, spec)
	return nil
}

func (d *updateDriver) ContainerStatus(context.Context, string) (string, error) {
	return runtime.StatusRunning, nil
}

// Podman can preserve the image ID when the rebuilt tag has the same config. An
// explicit update must still replace the container to pick up the refreshed base.
func (d *updateDriver) ContainerImageID(context.Context, string) (string, error) { return "same", nil }
func (d *updateDriver) ImageID(context.Context, string) (string, error)          { return "same", nil }
func (d *updateDriver) RemoveContainer(context.Context, string) error {
	d.removed++
	return nil
}
func (d *updateDriver) CreateContainer(context.Context, runtime.ContainerSpec) error {
	d.created++
	return nil
}

// recreateDriver reports the container as missing once removed, and running
// again once a new container has been started.
type recreateDriver struct {
	*updateDriver
	missingWorkspaceImage bool
}

func (d *recreateDriver) ImageID(_ context.Context, name string) (string, error) {
	// Only the workspace image is cached; its base has been pruned.
	if name == "ocm/demo:latest" && !d.missingWorkspaceImage {
		return "same", nil
	}
	return "", nil
}

func (d *recreateDriver) ContainerStatus(context.Context, string) (string, error) {
	switch {
	case d.removed > 0 && d.created == 0:
		return runtime.StatusMissing, nil
	case d.created > d.started:
		return runtime.StatusCreated, nil
	default:
		return runtime.StatusRunning, nil
	}
}

func TestRecreateContainerReplacesContainerAndKeepsWorkspace(t *testing.T) {
	driver := &recreateDriver{updateDriver: &updateDriver{fakeDriver: &fakeDriver{}}}
	path := t.TempDir()
	summary := Summary{Manifest: Manifest{
		Name:          "demo",
		ImageName:     "ocm/demo:latest",
		Image:         ImageConfig{BaseImage: "docker.io/mroger78/ocm-base:0.7.0"},
		ContainerName: "demo",
		HomeDir:       filepath.Join(path, "home"),
		OpenCodePort:  4096,
	}, Path: path}
	if err := SaveManifest(filepath.Join(path, ManifestFile), summary.Manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	l := Lifecycle{cfg: config.Config{Runtime: config.RuntimeDocker, BaseImage: config.BaseImageConfig{Name: config.DefaultBaseImage}}, driver: driver, agents: agent.NewRegistry()}

	if err := l.RecreateContainer(context.Background(), summary); err != nil {
		t.Fatalf("RecreateContainer error: %v", err)
	}
	if driver.removed != 1 || driver.created != 1 || driver.started != 1 {
		t.Fatalf("replacement = remove:%d create:%d start:%d, want 1 each", driver.removed, driver.created, driver.started)
	}
	if len(driver.pulled) != 0 {
		t.Fatalf("pulls = %v, recreate must not refresh the base image", driver.pulled)
	}
	if len(driver.builds) != 0 {
		t.Fatalf("builds = %v, recreate must reuse the existing workspace image", driver.builds)
	}
	kept, err := LoadManifest(filepath.Join(path, ManifestFile))
	if err != nil {
		t.Fatalf("workspace manifest should be kept: %v", err)
	}
	if kept.Image.BaseImage != summary.Manifest.Image.BaseImage {
		t.Fatalf("base image = %q, recreate must keep the workspace image config", kept.Image.BaseImage)
	}
}

func TestRecreateMissingImagePreservesContainer(t *testing.T) {
	driver := &recreateDriver{updateDriver: &updateDriver{fakeDriver: &fakeDriver{}}, missingWorkspaceImage: true}
	summary := Summary{Manifest: Manifest{Name: "demo", ContainerName: "demo", ImageName: "ocm/demo:latest", OpenCodePort: 4096}}
	l := Lifecycle{driver: driver}
	if err := l.RecreateContainer(context.Background(), summary); err == nil {
		t.Fatal("expected error for missing workspace image")
	}
	if driver.removed != 0 || driver.created != 0 || len(driver.pulled) != 0 || len(driver.builds) != 0 {
		t.Fatalf("missing image must leave the container untouched: %+v", driver.updateDriver)
	}
}
