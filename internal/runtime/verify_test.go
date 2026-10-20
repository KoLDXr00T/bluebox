package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"box/internal/sandbox"
)

// The token cache is what lets a run skip the double VM boot, so its
// invalidation is the part that must be right: a matching identity is trusted,
// any other identity (a changed runtime) is not, and a missing cache never
// reads as verified.
func TestIsolationCache(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())

	if isolationCurrent("podman", "A") {
		t.Fatal("empty cache must not read as verified")
	}

	storeToken("podman", "A", "6.12.0", "6.18.0")
	if !isolationCurrent("podman", "A") {
		t.Error("the stored identity should be current")
	}
	if isolationCurrent("podman", "B") {
		t.Error("a different runtime identity must not be trusted")
	}

	// A later verification of a new runtime replaces the old one.
	storeToken("podman", "B", "6.12.0", "6.18.0")
	if isolationCurrent("podman", "A") {
		t.Error("the superseded identity must no longer be current")
	}
	if !isolationCurrent("podman", "B") {
		t.Error("the newly stored identity should be current")
	}
}

// A proof made on one backend says nothing about another: they boot guests
// through different toolchains, and one can be broken while the other works.
func TestIsolationProofIsPerBackend(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	storeToken("firecracker", "same-id", "6.1.155", "6.17.0")
	if isolationCurrent("podman", "same-id") || isolationCurrent("krun", "same-id") {
		t.Fatal("a firecracker proof must not clear podman or krun")
	}
	storeToken("podman", "p-id", "6.12.0", "6.17.0")
	if !isolationCurrent("firecracker", "same-id") {
		t.Error("proving podman must not drop the firecracker proof")
	}
	if !isolationCurrent("podman", "p-id") {
		t.Error("the podman proof should be current")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	t.Setenv("BOX_HOME", t.TempDir())
	storeToken("krun", "id-123", "guest-k", "base-k")
	got, ok := loadToken("krun")
	if !ok {
		t.Fatal("token should load after store")
	}
	if got.Identity != "id-123" || got.Guest != "guest-k" || got.Baseline != "base-k" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.VerifiedAt == "" {
		t.Error("VerifiedAt should be stamped")
	}
	if _, ok := loadToken("podman"); ok {
		t.Error("another backend has no token")
	}
}

// A corrupt cache, or one in the format from before tokens were per backend,
// must degrade to "not verified" (forcing a fresh check), never crash or
// falsely clear a run.
func TestUnreadableCacheIsNotCurrent(t *testing.T) {
	for name, body := range map[string]string{
		"corrupt":    "not json",
		"old format": `{"identity":"A","guest":"6.12.0","baseline":"6.18.0","verified_at":"2026-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("BOX_HOME", t.TempDir())
			p, err := sandbox.VerifyCachePath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, ok := loadToken("podman"); ok {
				t.Error("an unreadable cache must not load")
			}
			if isolationCurrent("podman", "A") {
				t.Error("an unreadable cache must not read as verified")
			}
			storeToken("podman", "A", "g", "b")
			if !isolationCurrent("podman", "A") {
				t.Error("a fresh proof should replace the unreadable cache")
			}
		})
	}
}

// fakeBins puts executables answering --version with the given text on a
// PATH of their own, so the identity sees exactly these tools.
func fakeBins(t *testing.T, bins map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, version := range bins {
		script := "#!/bin/sh\necho '" + version + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

// A host with only the firecracker backend has no krun, and must not need
// it: firecracker never runs krun.
func TestFirecrackerIdentityNeedsNoKrun(t *testing.T) {
	if goruntime.GOOS == "darwin" {
		t.Skip("macOS fingerprints the podman machine")
	}
	fakeBins(t, map[string]string{"crun": "crun version 1.14", "podman": "podman version 4.9.3"})
	fc, err := RuntimeIdentity("firecracker")
	if err != nil {
		t.Fatalf("firecracker identity without krun: %v", err)
	}
	if _, err := RuntimeIdentity("podman"); err == nil || !strings.Contains(err.Error(), "krun") {
		t.Errorf("podman needs krun, and should say so: %v", err)
	}
	if _, err := RuntimeIdentity("krun"); err == nil {
		t.Error("krun needs krun on PATH")
	}

	// An upgraded crun is a different runtime for firecracker.
	fakeBins(t, map[string]string{"crun": "crun version 1.15", "podman": "podman version 4.9.3"})
	if fc2, _ := RuntimeIdentity("firecracker"); fc2 == fc {
		t.Error("a changed crun must change the firecracker identity")
	}
}

// With krun present, each backend still gets an identity of its own, so no
// cached proof can be read as another's.
func TestIdentitiesDifferPerBackend(t *testing.T) {
	dir := fakeBins(t, map[string]string{"crun": "crun version 1.14 +LIBKRUN", "podman": "podman version 4.9.3"})
	if err := os.Symlink(filepath.Join(dir, "crun"), filepath.Join(dir, "krun")); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, b := range []string{"podman", "krun", "firecracker"} {
		id, err := RuntimeIdentity(b)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if other, dup := seen[id]; dup {
			t.Errorf("%s and %s share an identity", b, other)
		}
		seen[id] = b
	}
}
