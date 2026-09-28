package termproxy

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestTranslateDetachKey(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"legacy byte", "\x11", "\x11"},
		{"kitty", "\x1b[113;5u", "\x11"},
		{"kitty press event", "\x1b[113;5:1u", "\x11"},
		{"kitty repeat event", "\x1b[113;5:2u", "\x11"},
		{"kitty with caps lock", "\x1b[113;69u", "\x11"},
		{"kitty alternate keys (AZERTY base layout)", "\x1b[113::97;5u", "\x11"},
		{"modifyOtherKeys", "\x1b[27;5;113~", "\x11"},
		{"surrounded", "ab\x1b[113;5ucd", "ab\x11cd"},
		{"kitty release event", "\x1b[113;5:3u", "\x1b[113;5:3u"},
		{"ctrl+shift", "\x1b[113;6u", "\x1b[113;6u"},
		{"other letter", "\x1b[97;5u", "\x1b[97;5u"},
		{"plain q", "\x1b[113u", "\x1b[113u"},
		{"arrow key", "\x1b[A", "\x1b[A"},
		{"escape key", "\x1b", "\x1b"},
		{"split sequence", "\x1b[113;", "\x1b[113;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(translateDetachKey([]byte(tc.in), 'q')); got != tc.want {
				t.Fatalf("translateDetachKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Claude Code's startup sequences on a kitty-capable terminal, split mid-way.
func TestModesResetAndRestore(t *testing.T) {
	var modes Modes
	output := "\x1b[?25l\x1b[<u\x1b[>5u\x1b[>4;2m\x1b[?1004h\x1b[?2004h\x1b[?2031h hello"
	modes.Observe([]byte(output[:12]))
	modes.Observe([]byte(output[12:]))

	if got, want := string(modes.Reset()), "\x1b[<1u\x1b[>4;0m\x1b[?25h\x1b[?2031l\x1b[?2004l\x1b[?1004l"; got != want {
		t.Fatalf("Reset = %q, want %q", got, want)
	}
	restore := "\x1b[?1004h\x1b[?2004h\x1b[?2031h\x1b[?25l\x1b[>5u\x1b[>4;2m"
	if got := string(modes.Restore()); got != restore {
		t.Fatalf("Restore = %q, want %q", got, restore)
	}
	// A new Claude process renegotiates on top of the restored modes; the next
	// detach pops both pushes.
	modes.Observe([]byte("\x1b[>5u"))
	if got := string(modes.Reset()); !strings.HasPrefix(got, "\x1b[<2u") {
		t.Fatalf("Reset after renegotiation = %q, want two pops", got)
	}
}

func TestModesUntouchedTerminalNeedsNoReset(t *testing.T) {
	var modes Modes
	modes.Observe([]byte("plain output \x1b[31mred\x1b[0m"))
	if got := modes.Reset(); len(got) != 0 {
		t.Fatalf("Reset = %q, want nothing", got)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// End to end through real ptys: the program enables the kitty protocol, the
// user's terminal sends Ctrl-Q in its extended encoding, and the program reads
// the raw byte; afterwards the terminal is told to leave the kitty mode.
func TestRunDeliversDetachKeyAndResetsModes(t *testing.T) {
	user, userTTY, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer user.Close()
	defer userTTY.Close()

	script := `stty raw -echo; printf '\033[>5uready\n'; dd bs=1 count=1 2>/dev/null | od -An -tx1`
	var out syncBuffer
	proxy := New(exec.Command("/bin/sh", "-c", script), 'q', nil)
	proxy.SetStdin(userTTY)
	proxy.SetStdout(&out)
	done := make(chan error, 1)
	go func() { done <- proxy.Run() }()

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "ready") {
		if time.Now().After(deadline) {
			t.Fatalf("program never became ready; output %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := user.Write([]byte("\x1b[113;5u")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return; output %q", out.String())
	}
	if got := out.String(); !strings.Contains(got, " 11") {
		t.Fatalf("program did not receive Ctrl-Q as 0x11; output %q", got)
	}
	if got := out.String(); !strings.HasSuffix(got, "\x1b[<1u") {
		t.Fatalf("output %q does not end by leaving the kitty mode", got)
	}
}

// Reattaching must make the program repaint: the relay narrows the pty by one
// column and restores the real size, two resizes the program cannot ignore.
func TestForceRedrawResizesAndRestores(t *testing.T) {
	user, userTTY, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer user.Close()
	defer userTTY.Close()
	program, programTTY, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer program.Close()
	defer programTTY.Close()
	size := pty.Winsize{Rows: 40, Cols: 120}
	if err := pty.Setsize(user, &size); err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(program, &size); err != nil {
		t.Fatal(err)
	}

	started, done := make(chan struct{}), make(chan struct{})
	close(started)
	go func() { forceRedraw(userTTY, program, size, started, done); close(done) }()

	var widths []uint16
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		current, err := pty.GetsizeFull(programTTY)
		if err != nil {
			t.Fatal(err)
		}
		if len(widths) == 0 || widths[len(widths)-1] != current.Cols {
			widths = append(widths, current.Cols)
		}
		select {
		case <-done:
			if got := fmt.Sprint(widths); got != "[120 119 120]" {
				t.Fatalf("program pty widths = %s, want [120 119 120]", got)
			}
			return
		default:
		}
	}
	t.Fatalf("forceRedraw did not finish; widths %v", widths)
}
