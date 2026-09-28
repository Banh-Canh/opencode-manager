package workspace

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mickael-menu/opencode-manager/internal/runtime"
)

type statusProbeDriver struct {
	*fakeDriver
	probe func(context.Context, string) (string, error)
}

func (d statusProbeDriver) ContainerStatus(ctx context.Context, name string) (string, error) {
	return d.probe(ctx, name)
}

func TestStatusesDeferWhenRuntimeSaturates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	calls := 0
	l := Lifecycle{driver: statusProbeDriver{probe: func(ctx context.Context, _ string) (string, error) {
		calls++
		<-ctx.Done()
		// exec.CommandContext often loses the context error after killing the CLI.
		return runtime.StatusUnknown, errors.New("signal: killed")
	}}}
	workspaces := []Summary{
		{Manifest: Manifest{Name: "alpha", ContainerName: "alpha"}},
		{Manifest: Manifest{Name: "beta", ContainerName: "beta"}},
	}
	statuses := l.Statuses(ctx, workspaces)
	if len(statuses) != 2 || calls != 1 {
		t.Fatalf("got %d statuses and %d probes, want 2 statuses and 1 probe", len(statuses), calls)
	}
	for i, status := range statuses {
		if !status.CheckPending || status.Error != "" || status.Workspace.Manifest.Name != workspaces[i].Manifest.Name {
			t.Fatalf("unexpected deferred status: %+v", status)
		}
	}
}

func TestStatusesDistinguishProbeCancellationFromFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		pending bool
	}{
		{"timeout", fmt.Errorf("inspect: %w", context.DeadlineExceeded), true},
		{"cancelled", fmt.Errorf("inspect: %w", context.Canceled), true},
		{"failure", errors.New("permission denied"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := Lifecycle{driver: statusProbeDriver{probe: func(context.Context, string) (string, error) {
				return runtime.StatusUnknown, tc.err
			}}}
			status := l.Statuses(context.Background(), []Summary{{}})[0]
			if status.CheckPending != tc.pending || (status.Error == "") != tc.pending {
				t.Fatalf("unexpected status: %+v", status)
			}
		})
	}
}
