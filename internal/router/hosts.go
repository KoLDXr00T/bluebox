package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"

	"box/internal/sandbox"
)

// Config is ~/.box/hosts.yaml: the remote hosts a router fronts besides the
// local one.
type Config struct {
	Hosts []HostConfig `yaml:"hosts"`
}

// HostConfig is one remote host. It is reached over SSH, which is the whole
// access control: whoever can log in can run commands in its sandboxes, as
// whoever can open the local socket can here. Socket is for a box API whose
// socket is already reachable from this machine (forwarded by hand, or a
// second box home in tests).
type HostConfig struct {
	Name   string `yaml:"name"`
	SSH    string `yaml:"ssh"`
	Socket string `yaml:"socket"`
	Box    string `yaml:"box"` // the box binary on the remote host; default "box"
	MaxUp  int    `yaml:"max_up"`
}

// ConfigPath is where the host list lives.
func ConfigPath() (string, error) {
	h, err := sandbox.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "hosts.yaml"), nil
}

var (
	// An ssh destination: a user@host or an ~/.ssh/config alias. Never an
	// option, which ssh would otherwise parse ahead of the "--".
	sshTargetRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@-]*$`)
	// ssh runs the remote command through the remote user's shell, so the
	// binary path is restricted to characters no shell gives meaning to.
	remotePathRe = regexp.MustCompile(`^/?[A-Za-z0-9._/-]+$`)
	// A socket path also appears in -L local:remote, so no colon either.
	socketPathRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// LoadConfig reads and checks a host list.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := decodeYAML(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// decodeYAML is strict, and turns the YAML library's panics on malformed
// input into errors, as boxfile.Parse does.
func decodeYAML(b []byte, v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("cannot parse: %v", r)
		}
	}()
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(b), yaml.DisallowUnknownField())
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (c Config) validate() error {
	seen := map[string]bool{Local: true}
	for i, h := range c.Hosts {
		if err := sandbox.ValidName(h.Name); err != nil {
			return fmt.Errorf("hosts[%d]: host name: %w", i, err)
		}
		if seen[h.Name] {
			if h.Name == Local {
				return fmt.Errorf("hosts[%d]: %q is the name of this machine", i, Local)
			}
			return fmt.Errorf("hosts[%d]: %q is listed twice", i, h.Name)
		}
		seen[h.Name] = true
		switch {
		case (h.SSH == "") == (h.Socket == ""):
			return fmt.Errorf("host %s: give exactly one of ssh and socket", h.Name)
		case h.SSH != "" && !sshTargetRe.MatchString(h.SSH):
			return fmt.Errorf("host %s: ssh must be user@host or an ssh config alias, got %q", h.Name, h.SSH)
		case h.Socket != "" && !socketPathRe.MatchString(h.Socket):
			return fmt.Errorf("host %s: socket must be an absolute path, got %q", h.Name, h.Socket)
		case h.Box != "" && !remotePathRe.MatchString(h.Box):
			return fmt.Errorf("host %s: box must be a plain path, got %q", h.Name, h.Box)
		case h.MaxUp < 0:
			return fmt.Errorf("host %s: max_up must not be negative", h.Name)
		}
	}
	return nil
}

// sshCommand is the ssh client; a variable so tests can stand in for it.
var sshCommand = "ssh"

// sshOpts apply to every ssh the router starts. BatchMode: never prompt,
// since stdin and stdout belong to the MCP client. Host keys are checked as
// the user's ssh config says; nothing here relaxes that.
var sshOpts = []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}

func ensureArgs(h HostConfig) []string {
	box := h.Box
	if box == "" {
		box = "box"
	}
	return append(append([]string{}, sshOpts...), "--", h.SSH, box, "__ensure-serve")
}

func forwardArgs(h HostConfig, local, remote string) []string {
	return append(append([]string{}, sshOpts...),
		"-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30",
		"-o", "StreamLocalBindUnlink=yes",
		"-L", local+":"+remote, "--", h.SSH)
}

// Remote is the API of one configured host. It connects on first use, keeps
// one ssh forward per host, and reconnects when the forward or the remote
// server has gone away.
type Remote struct {
	cfg HostConfig
	dir string // owner-only directory for the forwarded socket

	mu      sync.Mutex
	client  *http.Client
	fwd     *exec.Cmd
	fwdDone chan struct{}
	lastOK  time.Time
	retryAt time.Time
	lastErr error
	closed  bool
}

// pingAfter is how long a connection is trusted without a check. A remote
// box serve exits after 30 idle minutes, or sooner when its binary is
// upgraded, so a request after a quiet spell checks first.
const pingAfter = 60 * time.Second

// retryAfter spaces out reconnection attempts to a host that is down.
const retryAfter = 15 * time.Second

// NewRemote returns the API for h. dir holds forwarded sockets and must be
// private to this user.
func NewRemote(h HostConfig, dir string) *Remote { return &Remote{cfg: h, dir: dir} }

func (rm *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	client, err := rm.ready(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "unreachable", "host %s: %v", rm.cfg.Name, err)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, "http://box"+r.URL.Path, r.Body)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "%v", err)
		return
	}
	out.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(out)
	if err != nil {
		rm.mu.Lock()
		rm.lastOK = time.Time{} // check before trusting it again
		rm.mu.Unlock()
		fail(w, http.StatusBadGateway, "unreachable", "host %s: %v", rm.cfg.Name, err)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	rm.mu.Lock()
	rm.lastOK = time.Now()
	rm.mu.Unlock()
}

// ready returns a client known to reach the host's box serve recently.
func (rm *Remote) ready(ctx context.Context) (*http.Client, error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.closed {
		return nil, errors.New("router closed")
	}
	if rm.client != nil && rm.alive() {
		if time.Since(rm.lastOK) < pingAfter {
			return rm.client, nil
		}
		if rm.ping(ctx) == nil {
			return rm.client, nil
		}
		// The forward is up but nothing answers behind it: the remote server
		// exited. Start it again; the forward reaches the same socket path.
		if rm.cfg.SSH != "" {
			if _, err := rm.ensure(ctx); err == nil && rm.ping(ctx) == nil {
				return rm.client, nil
			}
		}
		rm.teardown()
	}
	if time.Now().Before(rm.retryAt) {
		return nil, rm.lastErr
	}
	if err := rm.connect(ctx); err != nil {
		rm.teardown()
		rm.lastErr, rm.retryAt = err, time.Now().Add(retryAfter)
		return nil, err
	}
	return rm.client, nil
}

func (rm *Remote) alive() bool {
	if rm.fwd == nil {
		return true // a socket host has no process to lose
	}
	select {
	case <-rm.fwdDone:
		return false
	default:
		return true
	}
}

func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

func (rm *Remote) connect(ctx context.Context) error {
	if rm.cfg.Socket != "" {
		rm.client = unixClient(rm.cfg.Socket)
		return rm.ping(ctx)
	}
	remote, err := rm.ensure(ctx)
	if err != nil {
		return err
	}
	local := filepath.Join(rm.dir, rm.cfg.Name+".sock")
	os.Remove(local)
	cmd := exec.Command(sshCommand, forwardArgs(rm.cfg, local, remote)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start ssh: %w", err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	rm.fwd, rm.fwdDone = cmd, done
	deadline := time.Now().Add(15 * time.Second)
	for {
		if c, err := net.Dial("unix", local); err == nil {
			c.Close()
			break
		}
		select {
		case <-done:
			return fmt.Errorf("ssh forward to %s exited: %s", rm.cfg.SSH, strings.TrimSpace(stderr.String()))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ssh forward to %s never came up", rm.cfg.SSH)
		}
	}
	rm.client = unixClient(local)
	return rm.ping(ctx)
}

// ensure starts box serve on the host if it is not running and returns the
// path of its socket there.
func (rm *Remote) ensure(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, sshCommand, ensureArgs(rm.cfg)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("ssh %s: %s", rm.cfg.SSH, msg)
	}
	sock := strings.TrimSpace(stdout.String())
	if !socketPathRe.MatchString(sock) {
		return "", fmt.Errorf("%s: box __ensure-serve answered %q, not a socket path (is box on the host up to date?)", rm.cfg.SSH, sock)
	}
	return sock, nil
}

// ping checks the host answers as a box API of the version this router speaks.
func (rm *Remote) ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, "GET", "http://box/v1/version", nil)
	resp, err := rm.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var v struct{ API string }
	if err := decodeJSON(resp.Body, &v); err != nil || v.API != "v1" {
		return fmt.Errorf("host %s does not speak API v1 (got %q)", rm.cfg.Name, v.API)
	}
	rm.lastOK = time.Now()
	return nil
}

func (rm *Remote) teardown() {
	if rm.fwd != nil {
		rm.fwd.Process.Kill()
		<-rm.fwdDone
		rm.fwd, rm.fwdDone = nil, nil
	}
	if rm.client != nil {
		rm.client.CloseIdleConnections()
		rm.client = nil
	}
}

// Close stops the host's ssh forward.
func (rm *Remote) Close() error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.closed = true
	rm.teardown()
	return nil
}

func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

// Open builds a router over the local API and the configured hosts. The
// returned close stops every ssh forward and removes their sockets.
func Open(version string, local http.Handler, cfg Config) (*Router, func(), error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	// MkdirTemp makes it 0700: only this user can reach the forwarded
	// sockets, as only this user can reach the local one.
	dir, err := os.MkdirTemp(base, "box-router-")
	if err != nil {
		return nil, nil, err
	}
	hosts := []Host{{Name: Local, API: local}}
	var remotes []*Remote
	for _, hc := range cfg.Hosts {
		rm := NewRemote(hc, dir)
		remotes = append(remotes, rm)
		hosts = append(hosts, Host{Name: hc.Name, API: rm, MaxUp: hc.MaxUp})
	}
	closeAll := func() {
		for _, rm := range remotes {
			rm.Close()
		}
		os.RemoveAll(dir)
	}
	return New(version, hosts), closeAll, nil
}
