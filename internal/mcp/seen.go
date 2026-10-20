package mcp

import (
	"crypto/sha256"
	"fmt"
	"sync"
)

// seen remembers, per session, a hash of the last content the model holds
// for a key (a file, a fork's diff, the sandbox list). It caches no data:
// every read still goes to the sandbox, and only when the fresh result is
// identical to what the model already holds is it answered with a short note
// in place of the content. A stale answer is impossible by construction.
type seen struct {
	mu   sync.Mutex
	last map[string][32]byte
}

func newSeen() *seen { return &seen{last: map[string][32]byte{}} }

// check reports whether content is what the model last held for key, and
// records it as held. full asks for the content regardless.
func (s *seen) check(key string, content []byte, full bool) (unchanged bool) {
	sum := sha256.Sum256(content)
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.last[key]
	s.last[key] = sum
	return ok && prev == sum && !full
}

// know records content the model supplied itself, such as a file it wrote:
// it holds that content without having been shown it.
func (s *seen) know(key string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last[key] = sha256.Sum256(content)
}

func fileKey(sandbox, path string) string { return "file\x00" + sandbox + "\x00" + path }
func diffKey(sandbox string) string       { return "diff\x00" + sandbox }

const listKey = "list"

func unchangedNote(what string, n int) string {
	return fmt.Sprintf("unchanged: %s is identical to the last time you saw it (%d bytes). "+
		"Pass full: true to see it again.", what, n)
}
