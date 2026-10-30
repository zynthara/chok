package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zynthara/chok/db"
)

// repoRoot is the chok checkout the tests run in.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// goTool runs the go command in dir, outside any workspace, and fails the
// test with its output when it fails.
func goTool(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("go", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// dbEnv is cfg as the environment variables of an application whose
// environment prefix is prefix: the way to point a process at the lane's
// database without touching its chok.yaml.
func dbEnv(prefix string, cfg db.Config) []string {
	env := []string{prefix + "_DB_DRIVER=" + cfg.Driver}
	switch cfg.Driver {
	case "sqlite":
		env = append(env, prefix+"_DB_SQLITE_PATH="+cfg.SQLite.Path)
	case "postgres":
		env = append(env, prefix+"_DB_POSTGRES_DSN="+cfg.Postgres.DSN)
	case "mysql":
		m := cfg.MySQL
		env = append(env,
			prefix+"_DB_MYSQL_HOST="+m.Host,
			prefix+"_DB_MYSQL_PORT="+strconv.Itoa(m.Port),
			prefix+"_DB_MYSQL_USERNAME="+m.Username,
			prefix+"_DB_MYSQL_PASSWORD="+m.Password,
			prefix+"_DB_MYSQL_DATABASE="+m.Database,
		)
	}
	return env
}

// process is a started command whose combined output is collected line
// by line.
type process struct {
	cmd    *exec.Cmd
	lines  chan string // each output line, closed at EOF
	exited chan error  // Wait's result

	mu  sync.Mutex
	out []string
}

// startProcess starts c and kills it at cleanup unless it has exited.
func startProcess(t *testing.T, c *exec.Cmd) *process {
	t.Helper()
	stdout, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.Stderr = c.Stdout
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	p := &process{cmd: c, lines: make(chan string, 256), exited: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			p.mu.Lock()
			p.out = append(p.out, sc.Text())
			p.mu.Unlock()
			select {
			case p.lines <- sc.Text():
			default: // nobody is watching any more
			}
		}
		close(p.lines)
		p.exited <- c.Wait()
	}()
	t.Cleanup(func() {
		select {
		case err := <-p.exited:
			p.exited <- err
		default:
			_ = c.Process.Kill()
			t.Logf("%s killed at cleanup: %v", c.Path, <-p.exited)
		}
	})
	return p
}

func (p *process) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.out, "\n")
}

// listening waits for a chok App's "chok: http listening" line and
// returns the address it names.
func (p *process) listening(t *testing.T, within time.Duration) string {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case l, ok := <-p.lines:
			if !ok {
				t.Fatalf("the process exited before listening:\n%s", p.output())
			}
			var entry struct{ Msg, Addr string }
			if json.Unmarshal([]byte(l), &entry) == nil && entry.Msg == "chok: http listening" {
				return entry.Addr
			}
		case <-deadline:
			t.Fatalf("no listening address after %s:\n%s", within, p.output())
		}
	}
}

// interrupt sends SIGINT and expects a clean exit within the limit.
func (p *process) interrupt(t *testing.T, within time.Duration) {
	t.Helper()
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.exited:
		p.exited <- err // for cleanup
		if err != nil {
			t.Fatalf("exit %v after SIGINT:\n%s", err, p.output())
		}
	case <-time.After(within):
		t.Fatalf("still running %s after SIGINT:\n%s", within, p.output())
	}
}
