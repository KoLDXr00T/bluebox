package mcp

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// DefaultOutputBudget is how much of one stream a tool result shows before
// the middle is elided. Command output is mostly read for its first and last
// lines (what ran, how it ended), so the cut keeps both ends.
const DefaultOutputBudget = 8 << 10

// spoolLimit bounds the full outputs a session keeps for the output tool.
// They stay in memory only: command output can hold secrets.
const spoolLimit = 64 << 20

// ansiRe matches terminal escapes: CSI sequences (colours, cursor moves),
// OSC sequences (titles, hyperlinks) and the remaining two-byte escapes.
var ansiRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// clean turns raw command output into what a reader of a terminal would have
// seen: escapes removed, carriage-return redraws (progress bars) resolved to
// their final state, and runs of identical lines collapsed.
func clean(b []byte) string {
	if bytes.IndexByte(b, 0) >= 0 {
		return fmt.Sprintf("[%d bytes of binary output not shown]", len(b))
	}
	s := ansiRe.ReplaceAllString(strings.ToValidUTF8(string(b), "�"), "")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		line := redraw(lines[i])
		j := i + 1
		for j < len(lines) && redraw(lines[j]) == line {
			j++
		}
		// Collapsing costs a marker line, so only runs of three or more.
		if n := j - i; n >= 3 {
			out = append(out, line, fmt.Sprintf("[previous line repeated %d more times]", n-1))
		} else {
			for k := i; k < j; k++ {
				out = append(out, line)
			}
		}
		i = j
	}
	return strings.Join(out, "\n")
}

// redraw keeps what is left on a line after carriage returns redraw it.
func redraw(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		return line[i+1:]
	}
	return line
}

// clipped is a stream cut to a budget: the text shown and, when something
// was elided, which lines.
type clipped struct {
	text          string
	elided        bool
	from, to, of  int // 1-based lines elided, of the total
	bytesNotShown int
}

// clipEnds keeps about two thirds of budget from the start of s and one third
// from the end, cutting at line boundaries.
func clipEnds(s string, budget int) clipped {
	if len(s) <= budget {
		return clipped{text: s}
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // s ended with a newline, not with an empty line
	}
	headBudget, tailBudget := budget*2/3, budget-budget*2/3
	var head, tail []string
	used := 0
	for _, l := range lines {
		if used+len(l) > headBudget {
			break
		}
		head = append(head, l)
		used += len(l)
	}
	used = 0
	for i := len(lines) - 1; i >= len(head); i-- {
		if used+len(lines[i]) > tailBudget {
			break
		}
		tail = append([]string{lines[i]}, tail...)
		used += len(lines[i])
	}
	// One enormous line: show its two ends rather than nothing.
	if len(head) == 0 && len(tail) == 0 {
		h, t := runeCut(s, headBudget, false), runeCut(s, tailBudget, true)
		return clipped{text: h + "\n" + markerPlaceholder + "\n" + t, elided: true,
			from: 1, to: 1, of: 1, bytesNotShown: len(s) - len(h) - len(t)}
	}
	shown := strings.Join(head, "") + strings.Join(tail, "")
	c := clipped{elided: true, from: len(head) + 1, to: len(lines) - len(tail), of: len(lines),
		bytesNotShown: len(s) - len(shown)}
	c.text = strings.Join(head, "") + markerPlaceholder + "\n" + strings.Join(tail, "")
	return c
}

// markerPlaceholder stands where the elision marker goes; the marker names
// the spooled output, which is only known once the stream is stored.
const markerPlaceholder = "\x00elided\x00"

// runeCut takes n bytes from the start (or end) of s without splitting a rune.
func runeCut(s string, n int, fromEnd bool) string {
	if fromEnd {
		i := len(s) - n
		for i < len(s) && !utf8.RuneStart(s[i]) {
			i++
		}
		return s[i:]
	}
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// spool keeps the cleaned full text of outputs that were clipped, so the
// model can ask for a part it was not shown. Oldest entries go first.
type spool struct {
	mu      sync.Mutex
	next    int
	order   []string
	entries map[string]map[string]string // id -> stream -> text
	size    int
}

func newSpool() *spool { return &spool{entries: map[string]map[string]string{}} }

func (p *spool) put(streams map[string]string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	id := fmt.Sprintf("o%d", p.next)
	p.entries[id] = streams
	p.order = append(p.order, id)
	for _, s := range streams {
		p.size += len(s)
	}
	for p.size > spoolLimit && len(p.order) > 1 {
		old := p.order[0]
		p.order = p.order[1:]
		for _, s := range p.entries[old] {
			p.size -= len(s)
		}
		delete(p.entries, old)
	}
	return id
}

func (p *spool) get(id, stream string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[id]
	if !ok {
		return "", false
	}
	s, ok := e[stream]
	return s, ok
}

// section is one rendered stream.
type section struct{ name, text string }

// render cleans (when cleanIt) and clips each named stream to budget. When
// anything was cut, the full text of every stream is spooled and the cut is
// marked with the id that retrieves it. A file is not cleaned, since the
// model may edit what it is shown; command output is.
func (s *Server) render(names []string, raw map[string][]byte, cleanIt bool, budget int) []section {
	full := map[string]string{}
	clips := map[string]clipped{}
	spoolIt := false
	for _, n := range names {
		if len(raw[n]) == 0 {
			continue
		}
		if cleanIt {
			full[n] = clean(raw[n])
		} else {
			full[n] = strings.ToValidUTF8(string(raw[n]), "\uFFFD")
		}
		clips[n] = clipEnds(full[n], budget)
		spoolIt = spoolIt || clips[n].elided
	}
	id := ""
	if spoolIt {
		id = s.spool.put(full)
	}
	var out []section
	for _, n := range names {
		c, ok := clips[n]
		if !ok {
			continue
		}
		text := c.text
		if c.elided {
			marker := fmt.Sprintf("[… lines %d-%d of %d (%d bytes) not shown; see them with output id=%s stream=%s]",
				c.from, c.to, c.of, c.bytesNotShown, id, n)
			text = strings.Replace(text, markerPlaceholder, marker, 1)
		}
		out = append(out, section{n, text})
	}
	return out
}

// output answers the output tool: numbered lines of a spooled text, a range
// or those matching a pattern, clipped like anything else.
func (s *Server) output(a map[string]any) (string, bool) {
	stream := str(a, "stream")
	if stream == "" {
		stream = "stdout"
	}
	text, ok := s.spool.get(str(a, "id"), stream)
	if !ok {
		return fmt.Sprintf("no output id=%s stream=%s: it was never elided, or is too old and was dropped", str(a, "id"), stream), true
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	from, to := num(a, "from_line"), num(a, "to_line")
	if from < 1 {
		from = 1
	}
	if to < 1 || to > len(lines) {
		to = len(lines)
	}
	var re *regexp.Regexp
	if g := str(a, "grep"); g != "" {
		var err error
		if re, err = regexp.Compile(g); err != nil {
			return "bad grep pattern: " + err.Error(), true
		}
	}
	var b strings.Builder
	matched := 0
	for i := from; i <= to; i++ {
		l := lines[i-1]
		if re != nil && !re.MatchString(strings.TrimSuffix(l, "\n")) {
			continue
		}
		matched++
		fmt.Fprintf(&b, "%d: %s", i, l)
		if !strings.HasSuffix(l, "\n") {
			b.WriteString("\n")
		}
	}
	if matched == 0 {
		return fmt.Sprintf("no lines (of %d) match", len(lines)), false
	}
	c := clipEnds(b.String(), maxText)
	if !c.elided {
		return c.text, false
	}
	return strings.Replace(c.text, markerPlaceholder,
		fmt.Sprintf("[… %d of these lines not shown; narrow the range or the pattern]", c.to-c.from+1), 1), false
}
