package router

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"box/internal/sandbox"
	"box/internal/server"
)

// The test binary stands in for ssh when BOX_FAKE_SSH_REMOTE is set: it
// answers __ensure-serve with that socket path, and for -L local:remote it
// forwards local to remote in-process, as ssh would over the network.
func TestMain(m *testing.M) {
	if remote := os.Getenv("BOX_FAKE_SSH_REMOTE"); remote != "" {
		fakeSSH(os.Args[1:], remote)
		return
	}
	os.Exit(m.Run())
}

func fakeSSH(args []string, remote string) {
	if log := os.Getenv("BOX_FAKE_SSH_LOG"); log != "" {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		f.WriteString(strings.Join(args, " ") + "\n")
		f.Close()
	}
	if os.Getenv("BOX_FAKE_SSH_FAIL") != "" {
		os.Stderr.WriteString("Permission denied (publickey).\n")
		os.Exit(255)
	}
	if args[len(args)-1] == "__ensure-serve" {
		os.Stdout.WriteString(remote + "\n")
		os.Exit(0)
	}
	for i, a := range args {
		if a != "-L" {
			continue
		}
		local, to, _ := strings.Cut(args[i+1], ":")
		ln, err := net.Listen("unix", local)
		if err != nil {
			os.Exit(255)
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				os.Exit(255)
			}
			go func() {
				defer c.Close()
				r, err := net.Dial("unix", to)
				if err != nil {
					return // as ssh does: the channel opens and closes
				}
				defer r.Close()
				go io.Copy(r, c)
				io.Copy(c, r)
			}()
		}
	}
	os.Exit(2)
}

// serveAPI serves h on a fresh Unix socket until the test ends, or until the
// returned stop is called.
func serveAPI(t *testing.T, sock string, h http.Handler) (stop func()) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	stop = func() { srv.Close() }
	t.Cleanup(stop)
	return stop
}

func fakeSSHEnv(t *testing.T, remote string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := sshCommand
	sshCommand = exe
	t.Cleanup(func() { sshCommand = old })
	log := filepath.Join(t.TempDir(), "ssh.log")
	t.Setenv("BOX_FAKE_SSH_REMOTE", remote)
	t.Setenv("BOX_FAKE_SSH_LOG", log)
	return log
}

func TestConfigValidation(t *testing.T) {
	good := `hosts:
  - name: gpu1
    ssh: me@gpu1.example.com
    max_up: 4
  - name: lab
    ssh: lab-alias
    box: /opt/box/bin/box
  - name: fwd
    socket: /run/user/1000/fwd.sock
`
	p := filepath.Join(t.TempDir(), "hosts.yaml")
	os.WriteFile(p, []byte(good), 0o600)
	c, err := LoadConfig(p)
	if err != nil || len(c.Hosts) != 3 || c.Hosts[0].MaxUp != 4 {
		t.Fatalf("good config: %v %+v", err, c)
	}
	for name, body := range map[string]string{
		"option as target":   "hosts:\n  - name: a\n    ssh: -oProxyCommand=touch /tmp/x\n",
		"space in target":    "hosts:\n  - name: a\n    ssh: me@h ls\n",
		"shell in box path":  "hosts:\n  - name: a\n    ssh: h\n    box: box;id\n",
		"relative socket":    "hosts:\n  - name: a\n    socket: x.sock\n",
		"colon in socket":    "hosts:\n  - name: a\n    socket: /a:b\n",
		"both transports":    "hosts:\n  - name: a\n    ssh: h\n    socket: /s\n",
		"neither transport":  "hosts:\n  - name: a\n",
		"named local":        "hosts:\n  - name: local\n    ssh: h\n",
		"duplicate":          "hosts:\n  - name: a\n    ssh: h\n  - name: a\n    ssh: g\n",
		"bad name":           "hosts:\n  - name: ../a\n    ssh: h\n",
		"unknown key":        "hosts:\n  - name: a\n    ssh: h\n    port: 22\n",
		"negative max_up":    "hosts:\n  - name: a\n    ssh: h\n    max_up: -1\n",
		"yaml library panic": "hosts: !\n",
	} {
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}

// ssh never sees a destination it could read as an option, and the remote
// command is nothing but the box binary and its fixed argument.
func TestSSHArgs(t *testing.T) {
	h := HostConfig{Name: "a", SSH: "me@host"}
	got := strings.Join(ensureArgs(h), " ")
	if !strings.HasSuffix(got, "-- me@host box __ensure-serve") || !strings.Contains(got, "BatchMode=yes") {
		t.Errorf("ensure: %s", got)
	}
	got = strings.Join(forwardArgs(h, "/l.sock", "/r.sock"), " ")
	if !strings.HasSuffix(got, "-L /l.sock:/r.sock -- me@host") || !strings.Contains(got, "ExitOnForwardFailure=yes") {
		t.Errorf("forward: %s", got)
	}
	if strings.Contains(got, "StrictHostKeyChecking") {
		t.Error("host key checking must be left to the user's ssh config")
	}
}

// A remote host reached over (fake) ssh: the router lists its sandboxes,
// qualified, through a real box API.
func TestRemoteOverSSH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BOX_HOME", home)
	if _, err := sandbox.Create("dev"); err != nil {
		t.Fatal(err)
	}
	bf, _ := sandbox.BoxfilePath("dev")
	if err := os.WriteFile(bf, []byte("base: docker.io/library/alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.sock")
	serveAPI(t, remote, server.NewHandler("remote-v"))
	log := fakeSSHEnv(t, remote)

	rt, closeAll, err := Open("v", &fakeHost{}, Config{Hosts: []HostConfig{{Name: "gpu1", SSH: "me@gpu1"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	code, _, arr := do(t, rt, "GET", "/v1/sandboxes", "")
	if code != 200 || len(arr) != 1 || arr[0]["name"] != "gpu1/dev" {
		t.Fatalf("list = %d %v", code, arr)
	}
	_, _, hosts := do(t, rt, "GET", "/v1/hosts", "")
	if len(hosts) != 2 || hosts[1]["version"] != "remote-v" || hosts[1]["error"] != nil {
		t.Errorf("hosts = %v", hosts)
	}
	b, _ := os.ReadFile(log)
	if n := strings.Count(string(b), "__ensure-serve"); n != 1 {
		t.Errorf("ensure ran %d times, want once:\n%s", n, b)
	}
	if strings.Count(string(b), "-L ") != 1 {
		t.Errorf("want one forward:\n%s", b)
	}
}

// A host whose ssh fails is reported, not fatal, and is not retried on
// every request.
func TestUnreachableHost(t *testing.T) {
	log := fakeSSHEnv(t, "/nonexistent.sock")
	t.Setenv("BOX_FAKE_SSH_FAIL", "1")
	rt, closeAll, err := Open("v", &fakeHost{sandboxes: []map[string]any{sb("dev", false, 0)}},
		Config{Hosts: []HostConfig{{Name: "gpu1", SSH: "me@gpu1"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	_, _, arr := do(t, rt, "GET", "/v1/sandboxes", "")
	if len(arr) != 1 || arr[0]["name"] != "dev" {
		t.Errorf("the reachable host's sandboxes should still list: %v", arr)
	}
	_, _, hosts := do(t, rt, "GET", "/v1/hosts", "")
	if e, _ := hosts[1]["error"].(string); !strings.Contains(e, "Permission denied") {
		t.Errorf("the ssh error should be reported: %v", hosts[1])
	}
	code, obj, _ := do(t, rt, "POST", "/v1/sandboxes/gpu1/dev/exec", "{}")
	if code != 502 || obj["code"] != "unreachable" {
		t.Errorf("exec on an unreachable host = %d %v", code, obj)
	}
	b, _ := os.ReadFile(log)
	if n := strings.Count(string(b), "__ensure-serve"); n != 1 {
		t.Errorf("a failing host should be retried after a pause, not per request: %d attempts", n)
	}
}

// When the remote server goes away and comes back (an idle exit, an
// upgrade), the next request after a quiet spell finds it again.
func TestRemoteServerComesBack(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.sock")
	stop := serveAPI(t, remote, &fakeHost{sandboxes: []map[string]any{sb("dev", false, 0)}})
	fakeSSHEnv(t, remote)
	rm := NewRemote(HostConfig{Name: "gpu1", SSH: "me@gpu1"}, t.TempDir())
	defer rm.Close()
	h := &Host{Name: "gpu1", API: rm}
	rt := New("v", []Host{*h})

	if code, _, _ := do(t, rt, "POST", "/v1/sandboxes/gpu1/dev/up", ""); code != 200 {
		t.Fatalf("first request: %d", code)
	}
	stop()
	os.Remove(remote)
	if code, obj, _ := do(t, rt, "POST", "/v1/sandboxes/gpu1/dev/up", ""); code != 502 {
		t.Errorf("with the server gone: %d %v", code, obj)
	}
	serveAPI(t, remote, &fakeHost{sandboxes: []map[string]any{sb("dev", false, 0)}})
	if code, obj, _ := do(t, rt, "POST", "/v1/sandboxes/gpu1/dev/up", ""); code != 200 {
		t.Errorf("after it came back: %d %v", code, obj)
	}
	rm.mu.Lock()
	rm.lastOK = time.Now().Add(-2 * pingAfter)
	rm.mu.Unlock()
	if code, _, _ := do(t, rt, "POST", "/v1/sandboxes/gpu1/dev/up", ""); code != 200 {
		t.Errorf("after a quiet spell: %d", code)
	}
}
