package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeHost answers like one box serve with the given sandboxes, and records
// which per-sandbox requests reached it.
type fakeHost struct {
	sandboxes []map[string]any
	down      bool // answers every request 502, like an unreachable remote

	mu   sync.Mutex
	hits []string
}

func (f *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"connection refused","code":"unreachable"}`))
		return
	}
	switch r.URL.Path {
	case "/v1/sandboxes":
		json.NewEncoder(w).Encode(f.sandboxes)
		return
	case "/v1/version":
		w.Write([]byte(`{"version":"v-test","api":"v1"}`))
		return
	}
	f.mu.Lock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/fork") {
		var b struct{ As string }
		json.NewDecoder(r.Body).Decode(&b)
		parent := strings.Split(r.URL.Path, "/")[3]
		fmt.Fprintf(w, `{"fork":%q,"parent":%q}`, b.As, parent)
		return
	}
	w.Write([]byte(`{"ok":true}`))
}

func (f *fakeHost) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

func sb(name string, up bool, warm int) map[string]any {
	return map[string]any{"name": name, "up": up, "warm": warm}
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	var obj map[string]any
	var arr []map[string]any
	if json.Unmarshal(b, &obj) != nil {
		json.Unmarshal(b, &arr)
	}
	return rec.Code, obj, arr
}

func TestListQualifiesNamesAndSurvivesADownHost(t *testing.T) {
	local := &fakeHost{sandboxes: []map[string]any{sb("dev", true, 0)}}
	gpu := &fakeHost{sandboxes: []map[string]any{sb("train", false, 2), sb("dev", false, 0)}}
	gone := &fakeHost{down: true}
	rt := New("v", []Host{{Name: Local, API: local}, {Name: "gpu1", API: gpu}, {Name: "gone", API: gone}})

	code, _, arr := do(t, rt, "GET", "/v1/sandboxes", "")
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	var names []string
	for _, it := range arr {
		names = append(names, it["name"].(string)+"@"+it["host"].(string))
	}
	if got := strings.Join(names, " "); got != "dev@local gpu1/train@gpu1 gpu1/dev@gpu1" {
		t.Errorf("names = %s", got)
	}

	_, _, hosts := do(t, rt, "GET", "/v1/hosts", "")
	if len(hosts) != 3 || hosts[2]["error"] == nil || hosts[0]["error"] != nil || hosts[1]["warm"].(float64) != 2 {
		t.Errorf("hosts = %v", hosts)
	}
}

func TestResolveAndForward(t *testing.T) {
	local := &fakeHost{sandboxes: []map[string]any{sb("dev", false, 0)}}
	a := &fakeHost{sandboxes: []map[string]any{sb("dev", false, 0), sb("only-a", false, 0), sb("twin", true, 0)}}
	b := &fakeHost{sandboxes: []map[string]any{sb("twin", false, 3)}}
	rt := New("v", []Host{{Name: Local, API: local}, {Name: "a", API: a}, {Name: "b", API: b}})

	for _, c := range []struct {
		method, path string
		want         int
		host         *fakeHost
		hit          string
	}{
		{"POST", "/v1/sandboxes/dev/exec", 200, local, "POST /v1/sandboxes/dev/exec"}, // local first
		{"POST", "/v1/sandboxes/a/dev/exec", 200, a, "POST /v1/sandboxes/dev/exec"},   // named host
		{"POST", "/v1/sandboxes/only-a/up", 200, a, "POST /v1/sandboxes/only-a/up"},   // the only host with it
		{"POST", "/v1/sandboxes/twin/exec", 200, a, "POST /v1/sandboxes/twin/exec"},   // where it is up
		{"POST", "/v1/sandboxes/twin/run", 200, b, "POST /v1/sandboxes/twin/run"},     // where it is warm
		{"GET", "/v1/sandboxes/b/twin/diff", 200, b, "GET /v1/sandboxes/twin/diff"},
	} {
		code, obj, _ := do(t, rt, c.method, c.path, `{}`)
		if code != c.want {
			t.Errorf("%s %s = %d %v", c.method, c.path, code, obj)
			continue
		}
		if hits := c.host.got(); len(hits) == 0 || hits[len(hits)-1] != c.hit {
			t.Errorf("%s %s: host got %v, want %s", c.method, c.path, hits, c.hit)
		}
	}

	for _, c := range []struct {
		method, path, code string
		status             int
	}{
		{"GET", "/v1/sandboxes/twin/diff", "ambiguous", 409}, // state of one copy: must be named
		{"POST", "/v1/sandboxes/ghost/exec", "no_sandbox", 404},
		{"POST", "/v1/sandboxes/nohost/dev/exec", "no_host", 404},
		{"POST", "/v1/sandboxes/..%2Fx/exec", "bad_name", 400},
		{"POST", "/v1/sandboxes/-x/dev/exec", "bad_name", 400},
		{"POST", "/v1/sandboxes/dev/nope", "not_found", 404},
		{"GET", "/v1/sandboxes/dev/exec", "bad_request", 405},
		{"POST", "/v1/sandboxes/a/b/c/exec", "not_found", 404},
	} {
		status, obj, _ := do(t, rt, c.method, c.path, `{}`)
		if status != c.status || obj["code"] != c.code {
			t.Errorf("%s %s = %d %v, want %d %s", c.method, c.path, status, obj, c.status, c.code)
		}
	}
}

// up on a name several remotes share goes to the least loaded host under
// its cap, and is refused when every host is at its cap.
func TestPlacement(t *testing.T) {
	busy := &fakeHost{sandboxes: []map[string]any{sb("w", false, 0), sb("x", true, 0), sb("y", true, 0)}}
	idle := &fakeHost{sandboxes: []map[string]any{sb("w", false, 0), sb("z", true, 0)}}
	rt := New("v", []Host{{Name: "busy", API: busy}, {Name: "idle", API: idle}})
	if code, obj, _ := do(t, rt, "POST", "/v1/sandboxes/w/up", ""); code != 200 || len(idle.got()) != 1 {
		t.Fatalf("up should go to the less loaded host: %d %v, idle got %v", code, obj, idle.got())
	}

	rt = New("v", []Host{{Name: "busy", API: busy, MaxUp: 2}, {Name: "idle", API: idle, MaxUp: 1}})
	if code, obj, _ := do(t, rt, "POST", "/v1/sandboxes/w/up", ""); code != 409 || obj["code"] != "capacity" {
		t.Errorf("every host at max_up: %d %v", code, obj)
	}
	// Naming the host is the human's choice and is not capped.
	if code, _, _ := do(t, rt, "POST", "/v1/sandboxes/busy/w/up", ""); code != 200 {
		t.Errorf("an explicit host should not be capped: %d", code)
	}
}

func TestForkIsQualifiedOnItsHost(t *testing.T) {
	a := &fakeHost{sandboxes: []map[string]any{sb("dev", false, 0)}}
	rt := New("v", []Host{{Name: Local, API: &fakeHost{}}, {Name: "a", API: a}})
	code, obj, _ := do(t, rt, "POST", "/v1/sandboxes/a/dev/fork", `{"as":"try1"}`)
	if code != 200 || obj["fork"] != "a/try1" || obj["parent"] != "a/dev" {
		t.Errorf("fork = %d %v", code, obj)
	}
	if hits := a.got(); len(hits) != 1 || hits[0] != "POST /v1/sandboxes/dev/fork" {
		t.Errorf("fork reached %v", hits)
	}
}

// A request that changes state drops the cached lists, so the next lookup
// sees, for instance, a sandbox that just came up.
func TestMutationInvalidatesLists(t *testing.T) {
	a := &fakeHost{sandboxes: []map[string]any{sb("dev", false, 0)}}
	rt := New("v", []Host{{Name: "a", API: a}})
	do(t, rt, "GET", "/v1/sandboxes", "")
	a.sandboxes = append(a.sandboxes, sb("new", false, 0))
	if _, _, arr := do(t, rt, "GET", "/v1/sandboxes", ""); len(arr) != 1 {
		t.Fatalf("within the TTL the list should be cached, got %d", len(arr))
	}
	do(t, rt, "POST", "/v1/sandboxes/dev/up", "")
	if _, _, arr := do(t, rt, "GET", "/v1/sandboxes", ""); len(arr) != 2 {
		t.Errorf("after a POST the list should be fresh, got %d", len(arr))
	}
}
