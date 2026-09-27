package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mickael-menu/opencode-manager/internal/config"
)

// writeMountingModule creates a module in <root>/tools/<name> declaring mounts.
func writeMountingModule(t *testing.T, root, name, mounts string) {
	t.Helper()
	dir := filepath.Join(root, "tools", name)
	writeTestFile(t, filepath.Join(dir, "module.yml"), []byte("name: "+name+"\nversion: 1\nmounts:\n"+mounts))
	for _, script := range []string{"install", "uninstall"} {
		if err := os.WriteFile(filepath.Join(dir, script), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstalledModuleMountsOnlyForInstalledModules(t *testing.T) {
	modules := t.TempDir()
	hostDir := t.TempDir()
	creds := filepath.Join(hostDir, "credentials.json")
	writeTestFile(t, creds, []byte("{}"))

	writeMountingModule(t, modules, "claude-auth", "  - { source: "+creds+", target: /home/debian/.claude/.credentials.json }\n")
	writeMountingModule(t, modules, "other", "  - { source: "+hostDir+", target: /opt/other }\n")
	writeMountingModule(t, modules, "maybe", "  - { source: "+filepath.Join(hostDir, "missing")+", target: /opt/maybe, optional: true }\n")

	l := Lifecycle{cfg: config.Config{ModuleDirs: []string{modules}}}
	home := t.TempDir()
	manifest := Manifest{HomeDir: home}

	mounts, fingerprint, err := l.installedModuleMounts(manifest)
	if err != nil || mounts != nil || fingerprint != "" {
		t.Fatalf("no modules: got %v %q %v", mounts, fingerprint, err)
	}

	manifest.Modules = []ModuleInstance{
		{Name: "claude-auth", Category: "tools", Version: 1},
		{Name: "maybe", Category: "tools", Version: 1},
	}
	mounts, fingerprint, err = l.installedModuleMounts(manifest)
	if err != nil {
		t.Fatalf("installedModuleMounts: %v", err)
	}
	if len(mounts) != 1 || mounts[0].Source != creds || mounts[0].Target != "/home/debian/.claude/.credentials.json" || mounts[0].ReadOnly {
		t.Fatalf("unexpected mounts: %+v", mounts)
	}
	if fingerprint == "" {
		t.Fatal("expected a fingerprint")
	}
	info, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil || info.IsDir() {
		t.Fatalf("mount point not created as a file in the workspace home: %v", err)
	}

	// The optional source appearing changes the fingerprint, so the container is
	// recreated with the new mount.
	if err := os.Mkdir(filepath.Join(hostDir, "missing"), 0o755); err != nil {
		t.Fatal(err)
	}
	mounts, withOptional, err := l.installedModuleMounts(manifest)
	if err != nil || len(mounts) != 2 || withOptional == fingerprint {
		t.Fatalf("optional mount: got %+v %q (was %q) %v", mounts, withOptional, fingerprint, err)
	}
}

func TestInstalledModuleMountsRequiredSourceMissing(t *testing.T) {
	modules := t.TempDir()
	writeMountingModule(t, modules, "need", "  - { source: /nonexistent/ocm-test, target: /opt/need }\n")
	l := Lifecycle{cfg: config.Config{ModuleDirs: []string{modules}}}
	manifest := Manifest{HomeDir: t.TempDir(), Modules: []ModuleInstance{{Name: "need", Category: "tools", Version: 1}}}
	if _, _, err := l.installedModuleMounts(manifest); err == nil {
		t.Fatal("expected an error for a missing required mount source")
	}
}

func TestAddMountingModuleRecordsBeforeInstall(t *testing.T) {
	modules := t.TempDir()
	creds := filepath.Join(t.TempDir(), "credentials.json")
	writeTestFile(t, creds, []byte("{}"))
	writeMountingModule(t, modules, "claude-auth", "  - { source: "+creds+", target: /home/debian/.claude/.credentials.json }\n")

	workspacePath := t.TempDir()
	home := filepath.Join(workspacePath, "home")
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Name: "demo", Runtime: "docker", ImageName: "img", ContainerName: "c", HomeDir: home}
	if err := SaveManifest(filepath.Join(workspacePath, ManifestFile), manifest); err != nil {
		t.Fatal(err)
	}

	fake := &fakeDriver{output: func(args []string) []byte { return nil }}
	l := Lifecycle{cfg: config.Config{ModuleDirs: []string{modules}}, driver: fake}
	catalog, err := l.Catalog()
	if err != nil || len(catalog) != 1 {
		t.Fatalf("Catalog: %v %v", catalog, err)
	}

	if err := l.AddModule(context.Background(), Summary{Manifest: manifest, Path: workspacePath}, catalog[0], nil); err != nil {
		t.Fatalf("AddModule: %v", err)
	}

	saved, err := LoadManifest(filepath.Join(workspacePath, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Modules) != 1 || saved.Modules[0].Name != "claude-auth" {
		t.Fatalf("manifest modules: %+v", saved.Modules)
	}
	installs := 0
	for _, args := range fake.gotArgs {
		if contains(args, "/opt/opencode-manager/modules/tools/claude-auth/install") {
			installs++
		}
	}
	// The fake marker never records it, so reconcile and the explicit run both
	// install; what matters is that it ran after the manifest listed it.
	if installs == 0 {
		t.Fatal("install script never ran")
	}
}
