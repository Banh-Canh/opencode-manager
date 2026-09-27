package workspace

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestActivityFromReport(t *testing.T) {
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Second)

	cases := []struct {
		name     string
		report   statusReport
		wantAct  Activity
		wantPend int
	}{
		{"working", statusReport{Activity: "working", UpdatedAt: fresh}, ActivityWorking, 0},
		{"idle maps to sleeping", statusReport{Activity: "idle", UpdatedAt: fresh}, ActivitySleeping, 0},
		{"needs-approval maps to waiting", statusReport{Activity: "needs-approval", PendingApproval: 2, UpdatedAt: fresh}, ActivityWaiting, 2},
		{"error", statusReport{Activity: "error", UpdatedAt: fresh}, ActivityError, 0},
		{"starting maps to unknown", statusReport{Activity: "starting", UpdatedAt: fresh}, ActivityUnknown, 0},
		{"unknown activity is off", statusReport{Activity: "weird", UpdatedAt: fresh}, ActivityOff, 0},
		{"stale heartbeat is off", statusReport{Activity: "working", PendingApproval: 1, UpdatedAt: now.Add(-time.Minute)}, ActivityOff, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			act, pend := activityFromReport(tc.report, now)
			if act != tc.wantAct {
				t.Fatalf("activity = %q, want %q", act, tc.wantAct)
			}
			if pend != tc.wantPend {
				t.Fatalf("pending = %d, want %d", pend, tc.wantPend)
			}
		})
	}
}

func TestReadWorkspaceActivityPrefersLiveDeepSeekState(t *testing.T) {
	home := writeStatus(t, `{"activity":"idle","updatedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	writeStatusAt(t, home, deepSeekStatusFileRelPath, `{"activity":"working","updatedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	if act, _ := readWorkspaceActivity(home, true, true, false); act != ActivityWorking {
		t.Fatalf("readWorkspaceActivity = %q, want working", act)
	}

	writeStatusAt(t, home, deepSeekStatusFileRelPath, `{"activity":"needs-approval","pendingApproval":1,"updatedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	if act, pending := readWorkspaceActivity(home, true, true, false); act != ActivityWaiting || pending != 1 {
		t.Fatalf("readWorkspaceActivity = %q,%d, want waiting,1", act, pending)
	}
}

func TestReadWorkspaceActivityShowsDeepSeekStarting(t *testing.T) {
	home := writeStatus(t, `{"activity":"idle","updatedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	writeStatusAt(t, home, deepSeekStatusFileRelPath, `{"activity":"starting","updatedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	if act, _ := readWorkspaceActivity(home, true, true, false); act != ActivityUnknown {
		t.Fatalf("readWorkspaceActivity = %q, want unknown", act)
	}
}

func TestReadActivityMissingFile(t *testing.T) {
	// No status file + container stopped => the workspace has never been used.
	if act, pend := readActivity(t.TempDir(), false); act != ActivityNew || pend != 0 {
		t.Fatalf("readActivity(missing, stopped) = %q,%d; want new,0", act, pend)
	}
	// No status file + container running => opencode is still booting.
	if act, _ := readActivity(t.TempDir(), true); act != ActivityUnknown {
		t.Fatalf("readActivity(missing, running) = %q; want unknown", act)
	}
}

func TestReadActivityStoppedButUsed(t *testing.T) {
	home := writeStatus(t, `{"activity":"working","updatedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	// File present but container stopped => used before, nothing live to show.
	if act, _ := readActivity(home, false); act != ActivityUnknown {
		t.Fatalf("readActivity(present, stopped) = %q; want unknown", act)
	}
}

func TestReadActivityParsesFile(t *testing.T) {
	home := writeStatus(t, `{"activity":"needs-approval","pendingApproval":1,"sessions":1,"updatedAt":"`+
		time.Now().UTC().Format(time.RFC3339)+`"}`)
	if act, pend := readActivity(home, true); act != ActivityWaiting || pend != 1 {
		t.Fatalf("readActivity = %q,%d; want waiting,1", act, pend)
	}
}

func TestReadDeepSeekUsage(t *testing.T) {
	home := t.TempDir()
	writeStatusAt(t, home, deepSeekStatusFileRelPath, `{"totalTokens":321,"messageCount":4}`)
	tokens, messages := readDeepSeekUsage(home)
	if tokens != 321 || messages != 4 {
		t.Fatalf("readDeepSeekUsage = %d,%d; want 321,4", tokens, messages)
	}
}

func writeStatus(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	writeStatusAt(t, home, statusFileRelPath, content)
	return home
}

func writeStatusAt(t *testing.T, home, relativePath, content string) {
	t.Helper()
	path := filepath.Join(home, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeClaudeSession(t *testing.T, home, id, activity string, pending int, age time.Duration) {
	t.Helper()
	rel := filepath.Join(claudeStatusDirRelPath, id+".json")
	writeStatusAt(t, home, rel, `{"activity":"`+activity+`","pendingApproval":`+strconv.Itoa(pending)+`,"updatedAt":"2000-01-01T00:00:00Z"}`)
	mtime := time.Now().Add(-age)
	if err := os.Chtimes(filepath.Join(home, rel), mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestReadWorkspaceActivityReportsClaude(t *testing.T) {
	home := writeStatus(t, `{"activity":"idle","updatedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
	writeClaudeSession(t, home, "a", "working", 0, 0)
	if act, _ := readWorkspaceActivity(home, true, false, true); act != ActivityWorking {
		t.Fatalf("readWorkspaceActivity = %q, want working", act)
	}
	if act, _ := readWorkspaceActivity(home, true, false, false); act != ActivitySleeping {
		t.Fatalf("claude disabled: readWorkspaceActivity = %q, want sleeping", act)
	}
	if act, _ := readWorkspaceActivity(home, false, false, true); act != ActivityUnknown {
		t.Fatalf("stopped: readWorkspaceActivity = %q, want unknown", act)
	}
}

func TestReadWorkspaceActivityClaudeWithoutOpenCode(t *testing.T) {
	home := t.TempDir()
	writeClaudeSession(t, home, "a", "idle", 0, 0)
	if act, _ := readWorkspaceActivity(home, true, false, true); act != ActivitySleeping {
		t.Fatalf("readWorkspaceActivity = %q, want sleeping", act)
	}
}

func TestReadWorkspaceActivityAggregatesWaitingRuntimes(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		openCode, deepSeek             int
		deepSeekEnabled, claudeEnabled bool
		want                           int
	}{
		{"OpenCode and Claude", 2, 0, false, true, 4},
		{"DeepSeek and Claude", 0, 3, true, true, 5},
		{"all runtimes", 2, 3, true, true, 7},
		{"Claude disabled", 2, 3, true, false, 5},
		{"DeepSeek disabled", 2, 3, false, true, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for path, pending := range map[string]int{statusFileRelPath: tc.openCode, deepSeekStatusFileRelPath: tc.deepSeek} {
				activity := "idle"
				if pending > 0 {
					activity = "needs-approval"
				}
				writeStatusAt(t, home, path, `{"activity":"`+activity+`","pendingApproval":`+strconv.Itoa(pending)+`,"updatedAt":"`+time.Now().UTC().Format(time.RFC3339)+`"}`)
			}
			writeClaudeSession(t, home, "a", "needs-approval", 1, 0)
			writeClaudeSession(t, home, "b", "needs-approval", 1, 0)
			writeClaudeSession(t, home, "stale", "needs-approval", 9, time.Minute)
			if act, pending := readWorkspaceActivity(home, true, tc.deepSeekEnabled, tc.claudeEnabled); act != ActivityWaiting || pending != tc.want {
				t.Fatalf("activity = %q,%d, want waiting,%d", act, pending, tc.want)
			}
		})
	}
}

func TestReadClaudeActivityAggregatesLiveSessions(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	if _, _, ok := readClaudeActivity(home, now); ok {
		t.Fatal("no status directory should report no live session")
	}

	writeClaudeSession(t, home, "a", "idle", 0, 0)
	writeClaudeSession(t, home, "b", "needs-approval", 1, 0)
	writeClaudeSession(t, home, "c", "needs-approval", 1, 0)
	writeClaudeSession(t, home, "stale", "error", 0, time.Minute)
	writeStatusAt(t, home, filepath.Join(claudeStatusDirRelPath, "a.heartbeat"), "123")
	writeStatusAt(t, home, filepath.Join(claudeStatusDirRelPath, "bad.json"), "{")

	act, pending, ok := readClaudeActivity(home, now)
	if !ok || act != ActivityWaiting || pending != 2 {
		t.Fatalf("readClaudeActivity = %q,%d,%v, want waiting,2,true", act, pending, ok)
	}

	writeClaudeSession(t, home, "b", "working", 0, 0)
	writeClaudeSession(t, home, "c", "idle", 0, 0)
	act, pending, ok = readClaudeActivity(home, now)
	if !ok || act != ActivityWorking || pending != 0 {
		t.Fatalf("readClaudeActivity = %q,%d,%v, want working,0,true", act, pending, ok)
	}

	for _, id := range []string{"a", "b", "c"} {
		writeClaudeSession(t, home, id, "idle", 0, time.Minute)
	}
	if act, _, ok := readClaudeActivity(home, now); ok {
		t.Fatalf("only stale sessions: got %q, want none", act)
	}
}
