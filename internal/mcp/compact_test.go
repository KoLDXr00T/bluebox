package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[31mred\x1b[0m":   "red",
		"\x1b]0;title\x07text": "text",
		"10%\r50%\r100%\ndone": "100%\ndone",
		"crlf\r\nline":         "crlf\nline",
		"a\na\nb":              "a\na\nb",
		"x\nx\nx\nx\ny":        "x\n[previous line repeated 3 more times]\ny",
		"\x1b[2K\rDownloading 1/3\r\x1b[2K\rDownloading 3/3\n": "Downloading 3/3\n",
	} {
		if got := clean([]byte(in)); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clean([]byte("ELF\x00\x01")); !strings.Contains(got, "binary") {
		t.Errorf("binary output should be summarised: %q", got)
	}
}

func TestClipEndsKeepsBothEnds(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	c := clipEnds(b.String(), 600)
	if !c.elided || !strings.HasPrefix(c.text, "line 1\n") || !strings.HasSuffix(c.text, "line 1000\n") {
		t.Fatalf("head and tail should survive: %q", c.text)
	}
	if c.of != 1000 || c.from < 2 || c.to > 999 || c.from > c.to {
		t.Errorf("elided range %d-%d of %d", c.from, c.to, c.of)
	}
	if len(c.text) > 600+len(markerPlaceholder)+1 {
		t.Errorf("over budget: %d bytes", len(c.text))
	}
	if c := clipEnds("short\n", 600); c.elided || c.text != "short\n" {
		t.Errorf("short text must pass through: %+v", c)
	}
	// A single huge line still shows both ends, without splitting a rune.
	huge := strings.Repeat("é", 2000)
	c = clipEnds(huge, 100)
	if !c.elided || !strings.HasPrefix(c.text, "é") || !strings.HasSuffix(c.text, "é") {
		t.Errorf("one long line: %q", c.text)
	}
}

// execAPI answers exec with whatever stdout the test sets, and remembers
// what write_file sent.
type execAPI struct {
	stdout  map[string]string // by argv[0], e.g. "cat", "sh"
	written string
	calls   int
}

func (f *execAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	var body struct {
		Argv  []string `json:"argv"`
		Stdin string   `json:"stdin"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if r.URL.Path == "/v1/sandboxes" {
		w.Write([]byte(`[{"name":"ok","up":true,"warm":0,"warm_target":0,"network":"none","base":"alpine"}]`))
		return
	}
	if r.URL.Path == "/v1/hosts" {
		w.Write([]byte(`[{"name":"local"},{"name":"gpu1","error":"ssh: connection refused"}]`))
		return
	}
	if len(body.Argv) > 0 && body.Argv[0] == "sh" && strings.Contains(body.Argv[2], "cat >") {
		in, _ := base64.StdEncoding.DecodeString(body.Stdin)
		f.written = string(in)
	}
	out := ""
	if len(body.Argv) > 0 {
		out = f.stdout[body.Argv[0]]
	}
	fmt.Fprintf(w, `{"exit_code":0,"stdout":%q,"stderr":"","duration_ms":1}`, base64.StdEncoding.EncodeToString([]byte(out)))
}

// calls runs tools/call requests against api and returns each result's text.
func calls(t *testing.T, api http.Handler, opt Options, reqs ...string) []string {
	t.Helper()
	var lines []string
	for i, r := range reqs {
		lines = append(lines, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":%s}`, i+1, r))
	}
	var out bytes.Buffer
	if err := New(api, "test", opt).Serve(strings.NewReader(strings.Join(lines, "\n")), &out); err != nil {
		t.Fatal(err)
	}
	var texts []string
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := dec.Decode(&m); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		texts = append(texts, m.Result.Content[0].Text)
	}
	return texts
}

func TestLongOutputIsElidedAndRetrievable(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "row %d\n", i)
	}
	api := &execAPI{stdout: map[string]string{"sh": b.String()}}
	got := calls(t, api, Options{OutputBudget: 1024},
		`{"name":"exec","arguments":{"sandbox":"ok","command":"seq"}}`,
		`{"name":"output","arguments":{"id":"o1","grep":"^row 4242$"}}`,
		`{"name":"output","arguments":{"id":"o1","from_line":10,"to_line":11}}`,
		`{"name":"output","arguments":{"id":"o9"}}`,
	)
	if !strings.Contains(got[0], "row 1\n") || !strings.Contains(got[0], "row 5000") ||
		!strings.Contains(got[0], "output id=o1 stream=stdout") || len(got[0]) > 1400 {
		t.Errorf("exec result should keep both ends and name the spool (%d bytes):\n%s", len(got[0]), got[0])
	}
	if got[1] != "4242: row 4242\n" {
		t.Errorf("grep = %q", got[1])
	}
	if got[2] != "10: row 10\n11: row 11\n" {
		t.Errorf("range = %q", got[2])
	}
	if !strings.Contains(got[3], "no output id=o9") {
		t.Errorf("unknown id = %q", got[3])
	}
}

func TestRereadSaysUnchanged(t *testing.T) {
	api := &execAPI{stdout: map[string]string{"cat": "print('hi')\n"}}
	read := `{"name":"read_file","arguments":{"sandbox":"ok","path":"/data/a.py"}}`
	got := calls(t, api, Options{},
		read,
		read,
		`{"name":"read_file","arguments":{"sandbox":"ok","path":"/data/a.py","full":true}}`,
		`{"name":"read_file","arguments":{"sandbox":"ok","path":"/data/b.py"}}`,
	)
	if got[0] != "print('hi')\n" {
		t.Errorf("first read = %q", got[0])
	}
	if !strings.HasPrefix(got[1], "unchanged:") {
		t.Errorf("second read should be unchanged: %q", got[1])
	}
	if got[2] != "print('hi')\n" {
		t.Errorf("full: true must show the content: %q", got[2])
	}
	if got[3] != "print('hi')\n" {
		t.Errorf("another path is not the same file: %q", got[3])
	}
	if api.calls != 4 {
		t.Errorf("every read must reach the sandbox; %d of 4 did", api.calls)
	}
}

// After the model writes a file, a read that finds what it wrote is
// unchanged; one that finds anything else is shown.
func TestWriteThenRead(t *testing.T) {
	api := &execAPI{stdout: map[string]string{"cat": "v1\n"}}
	got := calls(t, api, Options{},
		`{"name":"read_file","arguments":{"sandbox":"ok","path":"/data/f"}}`,
		`{"name":"write_file","arguments":{"sandbox":"ok","path":"/data/f","content":"v2\n"}}`,
		// The fake still serves v1: the file was reverted behind the model's back.
		`{"name":"read_file","arguments":{"sandbox":"ok","path":"/data/f"}}`,
	)
	if api.written != "v2\n" {
		t.Fatalf("write_file sent %q", api.written)
	}
	if got[2] != "v1\n" {
		t.Errorf("the model last held v2, so v1 must be shown, got %q", got[2])
	}
	api2 := &execAPI{stdout: map[string]string{"cat": "v2\n"}}
	got = calls(t, api2, Options{},
		`{"name":"write_file","arguments":{"sandbox":"ok","path":"/data/f","content":"v2\n"}}`,
		`{"name":"read_file","arguments":{"sandbox":"ok","path":"/data/f"}}`,
	)
	if !strings.HasPrefix(got[1], "unchanged:") {
		t.Errorf("reading back what was written should be unchanged: %q", got[1])
	}
}

func TestListIsCompactAndNamesUnreachableHosts(t *testing.T) {
	got := calls(t, &execAPI{}, Options{},
		`{"name":"list_sandboxes","arguments":{}}`,
		`{"name":"list_sandboxes","arguments":{}}`,
	)
	if !strings.Contains(got[0], "ok: up, warm 0/0, network none, base alpine") ||
		!strings.Contains(got[0], "host gpu1 unreachable") || strings.Contains(got[0], "local unreachable") {
		t.Errorf("list = %q", got[0])
	}
	if !strings.HasPrefix(got[1], "unchanged:") {
		t.Errorf("an identical list should be unchanged: %q", got[1])
	}
}
