// Package plugins launches the plugin binaries an operator put on disk;
// it never downloads or installs anything.
package plugins

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/procx"
)

type Launch struct {
	Name    string
	Command string
	Args    []string
	Dir     string
}

type Supervisor struct {
	CallbackURL func() string
	Done        <-chan struct{}

	mu      sync.Mutex
	managed map[string]*managedPlugin
}

func New(callbackURL func() string, done <-chan struct{}) *Supervisor {
	return &Supervisor{
		CallbackURL: callbackURL,
		Done:        done,
		managed:     map[string]*managedPlugin{},
	}
}

const (
	// Counted from stdin closing: no signals, so it works the same on Windows.
	managedStopGrace    = 5 * time.Second
	managedHealthyAfter = 30 * time.Second
	BackoffMin          = time.Second
	managedBackoffMax   = time.Minute
	// Above bufio's 64 KiB default, which a traceback with a large repr
	// or JSON dump exceeds.
	LogLineMax = 1 << 20
)

type managedPlugin struct {
	name    string
	command string
	args    []string
	dir     string
	env     []string

	mu          sync.Mutex
	proc        *exec.Cmd
	stdin       io.WriteCloser
	running     bool
	supervising bool
	stopped     bool
	restarts    int
	stop        chan struct{}
}

func (s *Supervisor) Start(p Launch) {
	s.mu.Lock()
	m, ok := s.managed[p.Name]
	if !ok {
		m = &managedPlugin{name: p.Name}
		s.managed[p.Name] = m
	}
	s.mu.Unlock()

	env := []string{"MONBOORU_URL=" + s.CallbackURL()}

	m.mu.Lock()
	m.command, m.args = p.Command, p.Args
	m.dir, m.env = p.Dir, env
	m.stopped, m.restarts = false, 0
	// Fresh even when a supervisor runs: it would read the closed old
	// channel at its next backoff and quit with the plugin enabled, and a
	// later Disable would close it twice.
	m.stop = make(chan struct{})
	if m.supervising {
		m.mu.Unlock()
		return
	}
	m.supervising = true
	m.mu.Unlock()
	go s.supervise(m)
}

func (s *Supervisor) Stop(name string) {
	s.mu.Lock()
	m := s.managed[name]
	s.mu.Unlock()
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.stopped {
		m.stopped = true
		close(m.stop)
	}
	m.mu.Unlock()
	m.terminate()
}

func (s *Supervisor) StopAll() {
	s.mu.Lock()
	names := make([]string, 0, len(s.managed))
	for name := range s.managed {
		names = append(names, name)
	}
	s.mu.Unlock()
	for _, name := range names {
		s.Stop(name)
	}
}

func (s *Supervisor) supervise(m *managedPlugin) {
	defer func() {
		m.mu.Lock()
		m.supervising = false
		m.mu.Unlock()
	}()
	for {
		// The flag, not the channel, is the authority: the backoff select
		// can miss a stop, so it is read again before each run.
		m.mu.Lock()
		stopped := m.stopped
		m.mu.Unlock()
		if stopped {
			return
		}
		started := time.Now()
		if err := m.run(); err != nil {
			logx.Errorf("plugin %s: %v", m.name, err)
		}
		m.mu.Lock()
		if m.stopped {
			m.mu.Unlock()
			return
		}
		if time.Since(started) >= managedHealthyAfter {
			m.restarts = 0
		}
		m.restarts++
		stop := m.stop
		delay := min(BackoffMin<<min(m.restarts-1, 6), managedBackoffMax)
		m.mu.Unlock()
		logx.Warnf("plugin %s: exited, restarting in %s", m.name, delay)
		select {
		case <-time.After(delay):
		case <-stop:
			// An Enable may have followed the stop that closed this; the
			// flag decides.
			m.mu.Lock()
			stopped := m.stopped
			m.mu.Unlock()
			if stopped {
				return
			}
		case <-s.Done:
			return
		}
	}
}

func (m *managedPlugin) run() error {
	m.mu.Lock()
	// Checked again under the lock that publishes m.proc: a Stop after
	// the loop's read had no child to terminate.
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	cmd := exec.Command(m.command, m.args...)
	procx.HideConsole(cmd)
	cmd.Dir = m.dir
	cmd.Env = append(os.Environ(), m.env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.proc, m.stdin, m.running = cmd, stdin, true
	m.mu.Unlock()
	logx.Infof("plugin %s: started (pid %d)", m.name, cmd.Process.Pid)

	// Drain before Wait: it closes the pipe out from under a live read.
	scanner := bufio.NewScanner(out)
	scanner.Buffer(nil, LogLineMax)
	var last string
	for scanner.Scan() {
		last = scanner.Text()
		logx.Infof("plugin %s: %s", m.name, last)
	}
	if scanner.Err() != nil {
		// A line past the cap stops the scan; undrained, the child blocks
		// on a full pipe and Wait never returns.
		logx.Warnf("plugin %s: output dropped: %v", m.name, scanner.Err())
		_, _ = io.Copy(io.Discard, out)
	}
	err = cmd.Wait()

	m.mu.Lock()
	m.proc, m.stdin, m.running = nil, nil, false
	m.mu.Unlock()
	// Output logs at info, below the default threshold; without the last
	// line a crash reads as a bare exit status.
	if err != nil && last != "" {
		return fmt.Errorf("%w: %s", err, last)
	}
	return err
}

// Closing stdin is the exit signal a managed plugin must honour.
func (m *managedPlugin) terminate() {
	m.mu.Lock()
	cmd, stdin := m.proc, m.stdin
	m.mu.Unlock()
	if cmd == nil {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	deadline := time.Now().Add(managedStopGrace)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		running := m.running
		m.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
		logx.Warnf("plugin %s: killed after %s", m.name, managedStopGrace)
	}
}

func (s *Supervisor) State(name string) string {
	s.mu.Lock()
	m := s.managed[name]
	s.mu.Unlock()
	if m == nil {
		return "stopped"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.running:
		return "running"
	case m.stopped || !m.supervising:
		return "stopped"
	case m.restarts > 0:
		return "restarting (" + strconv.Itoa(m.restarts) + ")"
	default:
		return "starting"
	}
}
