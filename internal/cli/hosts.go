package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"box/internal/router"
	"box/internal/sandbox"
	"box/internal/server"
)

// remoteIdle is how long a box serve started for a router waits unused
// before exiting. The router checks after a quiet spell and starts it again.
const remoteIdle = 30 * time.Minute

// apiFor is the API box mcp and box hosts speak through: the local one, or a
// router over it and the hosts in the host list. A missing default host
// list means local only; a missing file named with --hosts is an error.
func apiFor(hostsFile string, localOnly bool) (http.Handler, func(), error) {
	local := server.NewHandler(Version)
	if localOnly {
		return local, func() {}, nil
	}
	path, explicit := hostsFile, hostsFile != ""
	if !explicit {
		p, err := router.ConfigPath()
		if err != nil {
			return nil, nil, err
		}
		path = p
	}
	cfg, err := router.LoadConfig(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return local, func() {}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rt, closeAll, err := router.Open(Version, local, cfg)
	if err != nil {
		return nil, nil, err
	}
	return rt, closeAll, nil
}

func hostsCmd() *cobra.Command {
	var hostsFile string
	c := &cobra.Command{
		Use: "hosts", Short: "the remote hosts box mcp reaches", GroupID: groupInspect,
		Long: "Lists the hosts in ~/.box/hosts.yaml, connects to each and shows\n" +
			"what it has. box mcp fronts all of them: a sandbox on another host is\n" +
			"named host/name. A host is reached over ssh, so whoever can log in\n" +
			"there can run commands in its sandboxes.\n\n" +
			"  hosts:\n" +
			"    - name: gpu1\n" +
			"      ssh: me@gpu1.example.com   # user@host or an ssh config alias\n" +
			"      max_up: 8                  # optional: placement keeps at most 8 up\n" +
			"      box: /usr/local/bin/box    # optional: box on that host",
		Args:              cobra.NoArgs,
		ValidArgsFunction: completeNothing,
		RunE: func(*cobra.Command, []string) error {
			api, closeAll, err := apiFor(hostsFile, false)
			if err != nil {
				return err
			}
			defer closeAll()
			rec := httptest.NewRecorder()
			api.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/hosts", nil))
			if rec.Code == http.StatusNotFound {
				fmt.Println("no hosts configured; add them to ~/.box/hosts.yaml (see box hosts --help)")
				return nil
			}
			var hosts []struct {
				Name, Version, Error string
				Sandboxes, Up, Warm  int
				MaxUp                int `json:"max_up"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "HOST\tSANDBOXES\tUP\tWARM\tVERSION\tSTATUS")
			down := 0
			for _, h := range hosts {
				status := "ok"
				if h.Error != "" {
					status, down = "unreachable: "+h.Error, down+1
				}
				up := fmt.Sprint(h.Up)
				if h.MaxUp > 0 {
					up += fmt.Sprintf("/%d", h.MaxUp)
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\n", h.Name, h.Sandboxes, up, h.Warm, h.Version, status)
			}
			tw.Flush()
			if down > 0 {
				exitCode = 1
				return fmt.Errorf("%s unreachable", plural(down, "host", "hosts"))
			}
			return nil
		},
	}
	c.Flags().StringVar(&hostsFile, "hosts", "", "host list (default ~/.box/hosts.yaml)")
	return c
}

// ensureServeCmd starts box serve if it is not running and prints its socket
// path. A router runs it on a remote host over ssh, then forwards that path.
func ensureServeCmd() *cobra.Command {
	return &cobra.Command{
		Use: "__ensure-serve", Hidden: true,
		Args:              cobra.NoArgs,
		ValidArgsFunction: completeNothing,
		RunE: func(*cobra.Command, []string) error {
			sock, err := sandbox.SocketPath()
			if err != nil {
				return err
			}
			if !listening(sock) {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				// No stdio: ssh waits for every holder of its stdout to close.
				cmd := exec.Command(exe, "serve", "--socket", sock, "--idle", remoteIdle.String())
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive this ssh session
				if err := cmd.Start(); err != nil {
					return err
				}
				cmd.Process.Release()
				for i := 0; i < 250 && !listening(sock); i++ {
					time.Sleep(20 * time.Millisecond)
				}
				if !listening(sock) {
					return fmt.Errorf("started box serve but it never listened on %s", sock)
				}
			}
			fmt.Println(sock)
			return nil
		},
	}
}

func listening(sock string) bool {
	c, err := net.Dial("unix", sock)
	if err == nil {
		c.Close()
	}
	return err == nil
}
