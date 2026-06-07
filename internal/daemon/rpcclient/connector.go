package rpcclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Connector implements the language-server-style auto-spawn lifecycle (plan
// §4.2): resolve the UDS path → connect; on a missing/stale socket → locate and
// spawn the autoskd binary detached → wait for readiness with bounded backoff.
// autoskd's single-instance binding makes a double-spawn harmless (the loser
// exits 0 and this connector connects to the winner).
type Connector struct {
	sock    string
	bin     string // optional explicit autoskd path
	noSpawn bool

	mu      sync.Mutex
	spawned bool
}

// NewConnector builds a connector for a socket path. bin is optional (empty →
// resolve via $AUTOSKD_BIN, alongside the calling binary, then PATH).
func NewConnector(sock, bin string) *Connector {
	return &Connector{sock: sock, bin: bin}
}

// spawnWait bounds how long we wait for a freshly-spawned (or peer-spawned)
// daemon to start accepting connections.
const (
	spawnWaitTotal = 5 * time.Second
	spawnWaitStep  = 25 * time.Millisecond
)

// Dial returns a connection to the daemon, auto-spawning it if absent.
func (c *Connector) Dial(ctx context.Context) (net.Conn, error) {
	if conn, err := dialUnix(ctx, c.sock); err == nil {
		return conn, nil
	} else if c.noSpawn {
		return nil, fmt.Errorf("autoskd not reachable at %s: %w", c.sock, err)
	}
	if err := c.ensureSpawn(); err != nil {
		return nil, err
	}
	return c.waitConnect(ctx)
}

// ensureSpawn starts autoskd once per connector. Concurrent first-callers in
// this process serialise on the mutex and only one spawn fires; concurrent
// callers across processes are handled by autoskd's single-instance binding.
func (c *Connector) ensureSpawn() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spawned {
		return nil
	}
	bin, err := c.locate()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "serve", "--sock", c.sock) // #nosec G204 - bin is operator/path-resolved
	cmd.Env = os.Environ()
	// Detach: new session, no controlling terminal, stdio to /dev/null so the
	// daemon outlives this client and never writes to its tty.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn autoskd: %w", err)
	}
	// We never Wait — the daemon is long-lived. Release the handle so we don't
	// leak a zombie if it exits.
	_ = cmd.Process.Release()
	c.spawned = true
	return nil
}

// waitConnect retries connecting with bounded backoff until the daemon is up,
// the context is cancelled, or the deadline elapses.
func (c *Connector) waitConnect(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(spawnWaitTotal)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		conn, err := dialUnix(ctx, c.sock)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(spawnWaitStep):
		}
	}
	return nil, fmt.Errorf("autoskd did not become ready at %s: %w", c.sock, lastErr)
}

// locate resolves the autoskd binary: explicit override → $AUTOSKD_BIN →
// alongside the calling binary → PATH.
func (c *Connector) locate() (string, error) {
	if c.bin != "" {
		return c.bin, nil
	}
	if env := os.Getenv("AUTOSKD_BIN"); env != "" {
		return env, nil
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "autoskd")
		if isExecutable(cand) {
			return cand, nil
		}
	}
	if p, err := exec.LookPath("autoskd"); err == nil {
		return p, nil
	}
	return "", errors.New("autoskd binary not found (build it, or set AUTOSKD_BIN)")
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}
