// Package router puts several hosts' box APIs behind one: the local box and
// any number of remote machines running box. It speaks the same /v1 API as
// box serve, so the MCP server works through it unchanged.
//
// A sandbox on another host is named host/name; a bare name means the local
// sandbox of that name, or the only host that has it. When a bare name exists
// on several remote hosts, up and run are placed (where it is already up or
// warm, then the least loaded host); anything that acts on a particular
// sandbox's state must name the host.
//
//	GET  /v1/version
//	GET  /v1/sandboxes          every host's, names qualified, plus "host"
//	GET  /v1/hosts              reachability and load per host
//	*    /v1/sandboxes/[host/]{name}/{verb}  forwarded to that host
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"box/internal/sandbox"
)

// Local is the name of the host box itself runs on.
const Local = "local"

const (
	listTTL     = 2 * time.Second // absorbs a burst of lists and name lookups
	hostTimeout = 3 * time.Second // per host, for lists and status
)

// Host is one box API behind the router.
type Host struct {
	Name string
	API  http.Handler
	// MaxUp caps how many sandboxes placement keeps up on this host at
	// once; 0 is no cap. Naming the host explicitly is not capped.
	MaxUp int
}

// Router is an http.Handler over several hosts.
type Router struct {
	version string
	hosts   []Host

	mu   sync.Mutex
	snap *snapshot
}

type snapshot struct {
	at    time.Time
	hosts []hostList // in config order
}

type hostList struct {
	host  *Host
	items []map[string]any
	err   error
}

// New returns a router over hosts, which are consulted in order. version is
// what /v1/version reports for the router itself.
func New(version string, hosts []Host) *Router {
	return &Router{version: version, hosts: hosts}
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, format string, args ...any) {
	reply(w, status, apiError{Error: fmt.Sprintf(format, args...), Code: code})
}

// verbs are the per-sandbox routes of box serve, and the methods they take.
var verbs = map[string]string{
	"up": "POST", "down": "POST", "exec": "POST", "run": "POST",
	"fork": "POST", "diff": "GET", "apply": "POST", "discard": "POST",
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/v1/version" && r.Method == "GET":
		reply(w, http.StatusOK, map[string]any{"version": rt.version, "api": "v1", "router": true})
	case path == "/v1/sandboxes" && r.Method == "GET":
		rt.handleList(w, r)
	case path == "/v1/hosts" && r.Method == "GET":
		rt.handleHosts(w, r)
	case strings.HasPrefix(path, "/v1/sandboxes/"):
		rt.handleSandbox(w, r, strings.Split(strings.TrimPrefix(path, "/v1/sandboxes/"), "/"))
	default:
		fail(w, http.StatusNotFound, "not_found", "no route %s %s", r.Method, path)
	}
}

// qualify names a sandbox as the list shows it: bare for local, host/name
// for anywhere else.
func qualify(host, name string) string {
	if host == Local {
		return name
	}
	return host + "/" + name
}

func (rt *Router) handleList(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, hl := range rt.lists(r.Context()).hosts {
		for _, it := range hl.items {
			m := make(map[string]any, len(it)+1)
			for k, v := range it {
				m[k] = v
			}
			name, _ := it["name"].(string)
			m["name"] = qualify(hl.host.Name, name)
			m["host"] = hl.host.Name
			out = append(out, m)
		}
	}
	reply(w, http.StatusOK, out)
}

type hostStatus struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Sandboxes int    `json:"sandboxes"`
	Up        int    `json:"up"`
	Warm      int    `json:"warm"`
	MaxUp     int    `json:"max_up,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (rt *Router) handleHosts(w http.ResponseWriter, r *http.Request) {
	snap := rt.lists(r.Context())
	out := make([]hostStatus, len(snap.hosts))
	var wg sync.WaitGroup
	for i, hl := range snap.hosts {
		st := hostStatus{Name: hl.host.Name, Sandboxes: len(hl.items), MaxUp: hl.host.MaxUp}
		st.Up, st.Warm = load(hl.items)
		if hl.err != nil {
			st.Error = hl.err.Error()
			out[i] = st
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), hostTimeout)
			defer cancel()
			var v struct{ Version string }
			if _, err := call(ctx, hl.host, "GET", "/v1/version", &v); err != nil {
				st.Error = err.Error()
			}
			st.Version = v.Version
			out[i] = st
		}()
	}
	wg.Wait()
	reply(w, http.StatusOK, out)
}

// load counts a host's sandboxes that are up and its warm VMs.
func load(items []map[string]any) (up, warm int) {
	for _, it := range items {
		if u, _ := it["up"].(bool); u {
			up++
		}
		if n, _ := it["warm"].(float64); n > 0 {
			warm += int(n)
		}
	}
	return up, warm
}

func (rt *Router) handleSandbox(w http.ResponseWriter, r *http.Request, parts []string) {
	var hostName, name, verb string
	switch len(parts) {
	case 2:
		name, verb = parts[0], parts[1]
	case 3:
		hostName, name, verb = parts[0], parts[1], parts[2]
	default:
		fail(w, http.StatusNotFound, "not_found", "no route %s %s", r.Method, r.URL.Path)
		return
	}
	method, ok := verbs[verb]
	if !ok {
		fail(w, http.StatusNotFound, "not_found", "no route %s %s", r.Method, r.URL.Path)
		return
	}
	if r.Method != method {
		fail(w, http.StatusMethodNotAllowed, "bad_request", "%s takes %s", verb, method)
		return
	}
	// Both become path segments on the host's API: neither may be anything
	// but a single safe component.
	names := []string{name}
	if hostName != "" {
		names = append(names, hostName)
	}
	for _, n := range names {
		if err := sandbox.ValidName(n); err != nil {
			fail(w, http.StatusBadRequest, "bad_name", "%v", err)
			return
		}
	}
	var h *Host
	if hostName != "" {
		if h = rt.host(hostName); h == nil {
			fail(w, http.StatusNotFound, "no_host", "no host %q; configured: %s", hostName, rt.hostNames())
			return
		}
	} else {
		var status int
		var code, msg string
		if h, status, code, msg = rt.resolve(r.Context(), name, verb); h == nil {
			fail(w, status, code, "%s", msg)
			return
		}
	}
	rt.forward(w, r, h, name, verb)
}

func (rt *Router) host(name string) *Host {
	for i := range rt.hosts {
		if rt.hosts[i].Name == name {
			return &rt.hosts[i]
		}
	}
	return nil
}

func (rt *Router) hostNames() string {
	var names []string
	for _, h := range rt.hosts {
		names = append(names, h.Name)
	}
	return strings.Join(names, ", ")
}

// resolve picks the host for a bare name, or says why it cannot.
func (rt *Router) resolve(ctx context.Context, name, verb string) (*Host, int, string, string) {
	type cand struct {
		hl     hostList
		up     bool
		warm   int
		hostUp int
	}
	var cands []cand
	var unreachable []string
	for _, hl := range rt.lists(ctx).hosts {
		if hl.err != nil {
			unreachable = append(unreachable, hl.host.Name)
			continue
		}
		for _, it := range hl.items {
			if it["name"] != name {
				continue
			}
			c := cand{hl: hl}
			c.up, _ = it["up"].(bool)
			if n, ok := it["warm"].(float64); ok {
				c.warm = int(n)
			}
			c.hostUp, _ = load(hl.items)
			if hl.host.Name == Local {
				return hl.host, 0, "", "" // a bare name is the local one first
			}
			cands = append(cands, c)
		}
	}
	switch len(cands) {
	case 0:
		msg := fmt.Sprintf("no sandbox %q on any host", name)
		if len(unreachable) > 0 {
			msg += fmt.Sprintf(" (unreachable: %s)", strings.Join(unreachable, ", "))
		}
		return nil, http.StatusNotFound, "no_sandbox", msg
	case 1:
		return cands[0].hl.host, 0, "", ""
	}
	var where []string
	for _, c := range cands {
		where = append(where, qualify(c.hl.host.Name, name))
	}
	ambiguous := fmt.Sprintf("%q is on several hosts (%s); name one", name, strings.Join(where, ", "))

	// Where it is already up is where exec, down and up mean.
	var up []cand
	for _, c := range cands {
		if c.up {
			up = append(up, c)
		}
	}
	switch {
	case len(up) == 1 && (verb == "exec" || verb == "down" || verb == "up"):
		return up[0].hl.host, 0, "", ""
	case verb != "up" && verb != "run":
		// Each copy's state is its own: acting on one must be a choice.
		return nil, http.StatusConflict, "ambiguous", ambiguous
	}
	if verb == "run" {
		best := -1
		for i, c := range cands {
			if c.warm > 0 && (best < 0 || c.warm > cands[best].warm) {
				best = i
			}
		}
		if best >= 0 {
			return cands[best].hl.host, 0, "", ""
		}
	}
	best := -1
	for i, c := range cands {
		if c.hl.host.MaxUp > 0 && c.hostUp >= c.hl.host.MaxUp {
			continue
		}
		if best < 0 || c.hostUp < cands[best].hostUp {
			best = i
		}
	}
	if best < 0 {
		return nil, http.StatusConflict, "capacity", fmt.Sprintf("every host with %q is at its max_up; name one (%s)", name, strings.Join(where, ", "))
	}
	return cands[best].hl.host, 0, "", ""
}

// forward hands the request to h with the path rewritten to the bare name.
// A fork's answer names the fork and its parent as the list does.
func (rt *Router) forward(w http.ResponseWriter, r *http.Request, h *Host, name, verb string) {
	out := r.Clone(r.Context())
	out.URL.Path = "/v1/sandboxes/" + name + "/" + verb
	out.URL.RawPath = ""
	out.RequestURI = ""
	if r.Method != "GET" {
		defer rt.invalidate() // up, down, fork, run... change what a list shows
	}
	if verb != "fork" || h.Name == Local {
		h.API.ServeHTTP(w, out)
		return
	}
	rec := newRecorder()
	h.API.ServeHTTP(rec, out)
	var res map[string]any
	if rec.code < 300 && json.Unmarshal(rec.body.Bytes(), &res) == nil {
		for _, k := range []string{"fork", "parent"} {
			if v, ok := res[k].(string); ok {
				res[k] = qualify(h.Name, v)
			}
		}
		reply(w, rec.code, res)
		return
	}
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.code)
	w.Write(rec.body.Bytes())
}

func (rt *Router) invalidate() {
	rt.mu.Lock()
	rt.snap = nil
	rt.mu.Unlock()
}

// lists is every host's sandbox list, fetched in parallel and kept briefly.
// A host that fails is reported in its entry, not as a failure of the whole.
func (rt *Router) lists(ctx context.Context) *snapshot {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.snap != nil && time.Since(rt.snap.at) < listTTL {
		return rt.snap
	}
	snap := &snapshot{at: time.Now(), hosts: make([]hostList, len(rt.hosts))}
	var wg sync.WaitGroup
	for i := range rt.hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hctx, cancel := context.WithTimeout(ctx, hostTimeout)
			defer cancel()
			hl := hostList{host: &rt.hosts[i]}
			_, hl.err = call(hctx, hl.host, "GET", "/v1/sandboxes", &hl.items)
			snap.hosts[i] = hl
		}()
	}
	wg.Wait()
	rt.snap = snap
	return snap
}

// call makes one request to a host and decodes its JSON answer into v.
func call(ctx context.Context, h *Host, method, path string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://box"+path, http.NoBody)
	if err != nil {
		return 0, err
	}
	rec := newRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.API.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return 0, fmt.Errorf("host %s: %w", h.Name, ctx.Err())
	}
	if rec.code >= 400 {
		var e apiError
		json.Unmarshal(rec.body.Bytes(), &e)
		if e.Error == "" {
			e.Error = http.StatusText(rec.code)
		}
		return rec.code, fmt.Errorf("%s", e.Error)
	}
	if err := json.Unmarshal(rec.body.Bytes(), v); err != nil {
		return rec.code, fmt.Errorf("host %s: unreadable answer to %s", h.Name, path)
	}
	return rec.code, nil
}

// recorder is an in-memory http.ResponseWriter.
type recorder struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	code   int
}

func newRecorder() *recorder { return &recorder{header: http.Header{}, code: http.StatusOK} }

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(c int) { r.code = c }
