// Package termproxy runs an interactive command behind a pseudo-terminal so the
// manager can see its terminal traffic. It makes a detach key work for programs
// that switch the terminal into an extended keyboard protocol, and undoes the
// modes they leave behind when the user detaches.
package termproxy

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"github.com/muesli/cancelreader"
)

// Command runs cmd with the caller's terminal relayed through a pty. It
// implements tea.ExecCommand, so the dashboard can hand it the terminal.
type Command struct {
	cmd    *exec.Cmd
	key    byte
	modes  *Modes
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// New relays cmd, rewriting extended encodings of Ctrl+key to the raw control
// byte the detacher inside matches, and tracking terminal modes in modes.
func New(cmd *exec.Cmd, key byte, modes *Modes) *Command {
	if modes == nil {
		modes = &Modes{}
	}
	return &Command{cmd: cmd, key: key, modes: modes, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
}

func (c *Command) SetStdin(r io.Reader)  { c.stdin = r }
func (c *Command) SetStdout(w io.Writer) { c.stdout = w }
func (c *Command) SetStderr(w io.Writer) { c.stderr = w }

// outputDrainTimeout bounds the wait for the last output after the command
// exits, in case the pty stays open.
const outputDrainTimeout = time.Second

// Redraw timing: how long forceRedraw waits for the attach to produce output,
// and how long each size is held so the program handles both resizes.
const (
	redrawAttachTimeout = 2 * time.Second
	redrawStep          = 100 * time.Millisecond
)

// forceRedraw makes the program repaint its whole screen once the attach is
// live. On reattach the terminal no longer shows the program's screen, but a
// differential renderer such as Claude Code's only repaints what changed, and
// dtach's redraw signal alone keeps the same size, so nothing changes. A real
// resize, one column narrower and back, makes it repaint everything.
func forceRedraw(in *os.File, ptmx *os.File, size pty.Winsize, firstOutput, done <-chan struct{}) {
	select {
	case <-firstOutput:
	case <-time.After(redrawAttachTimeout):
	case <-done:
		return
	}
	if size.Cols < 2 {
		return
	}
	narrower := size
	narrower.Cols--
	for _, step := range []func() error{
		func() error { return pty.Setsize(ptmx, &narrower) },
		func() error { return pty.InheritSize(in, ptmx) },
	} {
		select {
		case <-time.After(redrawStep):
		case <-done:
			return
		}
		if step() != nil {
			return
		}
	}
}

// Run starts the command on a new pty sized like the caller's terminal, relays
// input and output until it exits, and then resets the modes it switched the
// terminal into. Without a terminal on stdin it runs the command directly.
func (c *Command) Run() error {
	in, ok := c.stdin.(*os.File)
	if !ok || !term.IsTerminal(in.Fd()) {
		c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = c.stdin, c.stdout, c.stderr
		return c.cmd.Run()
	}

	var size *pty.Winsize
	if width, height, err := term.GetSize(in.Fd()); err == nil {
		size = &pty.Winsize{Cols: uint16(width), Rows: uint16(height)}
	}
	ptmx, err := pty.StartWithSize(c.cmd, size)
	if err != nil {
		return fmt.Errorf("start %s on a pty: %w", c.cmd.Path, err)
	}
	defer ptmx.Close()

	state, err := term.MakeRaw(in.Fd())
	if err != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		return fmt.Errorf("switch terminal to raw mode: %w", err)
	}
	defer func() { _ = term.Restore(in.Fd(), state) }()

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer func() { signal.Stop(resize); close(resize) }()
	go func() {
		for range resize {
			_ = pty.InheritSize(in, ptmx)
		}
	}()

	if restore := c.modes.Restore(); len(restore) > 0 {
		_, _ = c.stdout.Write(restore)
	}

	outputDone := make(chan struct{})
	firstOutput := make(chan struct{})
	go func() {
		defer close(outputDone)
		buf := make([]byte, 32*1024)
		started := false
		for {
			n, err := ptmx.Read(buf)
			if n > 0 && !started {
				started = true
				close(firstOutput)
			}
			if n > 0 {
				c.modes.Observe(buf[:n])
				if _, err := c.stdout.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	if size != nil {
		go forceRedraw(in, ptmx, *size, firstOutput, outputDone)
	}

	// A cancelable reader, so no goroutine is left blocked on the terminal to
	// steal the next keystroke from the dashboard once the command exits.
	reader, err := cancelreader.NewReader(in)
	if err != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		return fmt.Errorf("read terminal input: %w", err)
	}
	defer reader.Close()
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				if _, err := ptmx.Write(translateDetachKey(buf[:n], c.key)); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	waitErr := c.cmd.Wait()
	reader.Cancel()
	<-inputDone
	select {
	case <-outputDone:
	case <-time.After(outputDrainTimeout):
		_ = ptmx.Close()
		<-outputDone
	}
	if reset := c.modes.Reset(); len(reset) > 0 {
		_, _ = c.stdout.Write(reset)
	}
	return waitErr
}
