package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"box/internal/boxfile"
	"box/internal/sandbox"
)

// CheckIsolation boots the sandbox two ways and confirms the guest kernel
// differs from the baseline a plain container sees. A shared kernel means
// podman fell back to a container, so it is reported as an error rather than a
// silent pass. Comparing against the baseline rather than the host's own uname
// is what keeps this honest on macOS, where every container kernel differs from
// the Darwin host whether or not a microVM is involved.
func CheckIsolation(name string, s boxfile.Spec) (guest, baseline string, err error) {
	guest, err = GuestKernel(name, s)
	if err != nil {
		return "", "", fmt.Errorf("could not start the sandbox (is the image built?): %w", err)
	}
	if guest == "" {
		return "", "", fmt.Errorf("could not start the sandbox; is the image built?")
	}
	baseline, err = BaselineKernel(name, s)
	if err != nil {
		return "", "", fmt.Errorf("could not read the baseline kernel: %w", err)
	}
	if baseline == "" {
		return "", "", fmt.Errorf("could not read the baseline kernel")
	}
	if guest == baseline {
		return guest, baseline, fmt.Errorf("not isolated: the sandbox shares kernel %s.\n"+
			"This is a container, not a microVM", baseline)
	}
	return guest, baseline, nil
}

// RuntimeIdentity fingerprints the parts of the toolchain that decide whether a
// sandbox on backend actually gets its own kernel. When any of them changes --
// the krun symlink is re-pointed, crun is upgraded, libkrun support is dropped,
// the pinned Firecracker build changes, podman changes -- the fingerprint
// changes and a cached verification no longer applies. It is cheap (a couple
// of --version calls and a stat), so it can gate every run without the cost
// of booting a VM.
//
// Each backend has its own: what proves the krun path says nothing about
// Firecracker, and the reverse. It covers only what that backend runs, so a
// host without krun can still use firecracker.
func RuntimeIdentity(backend string) (string, error) {
	parts := []string{goruntime.GOOS + "/" + goruntime.GOARCH, "backend:" + backend}

	switch {
	case goruntime.GOOS == "darwin":
		// Isolation lives inside the podman machine VM, so its identity, not a
		// host-side krun, is what matters.
		out, err := exec.Command("podman", "machine", "inspect", "--format",
			"{{.Name}} {{.Rootful}}").Output()
		if err != nil {
			return "", fmt.Errorf("no podman machine to fingerprint: %w", err)
		}
		parts = append(parts, "machine:"+strings.TrimSpace(string(out)))
	case backend == "firecracker":
		// The VMM and guest kernel are pinned by hash and checked before every
		// use (fcAssets), so the pins identify them; crun is what confines and
		// runs the VMM.
		parts = append(parts, "fc:"+fcVersion+":"+fcBinarySHA, "fckernel:"+fcKernelSHA, "fctools:"+fcToolsImage)
		p, err := binIdentity("crun")
		if err != nil {
			return "", err
		}
		parts = append(parts, p...)
	default:
		// The krun symlink and the crun binary it resolves to are the whole
		// isolation path; the version string carries the +LIBKRUN tag, and
		// losing it is exactly the degradation this guards against.
		p, err := binIdentity("krun")
		if err != nil {
			return "", err
		}
		parts = append(parts, p...)
	}
	if out, err := exec.Command("podman", "--version").Output(); err == nil {
		parts = append(parts, "podman:"+strings.TrimSpace(string(out)))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// binIdentity is where bin resolves on PATH, through any symlink, and that
// file's size, mtime and version: a re-pointed link or an upgrade changes it.
func binIdentity(bin string) ([]string, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("no '%s' on PATH", bin)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	parts := []string{bin + ":" + path + "->" + real}
	if fi, err := os.Stat(real); err == nil {
		parts = append(parts, fmt.Sprintf("%s-file:%d:%d", bin, fi.Size(), fi.ModTime().UnixNano()))
	}
	if out, err := exec.Command(bin, "--version").Output(); err == nil {
		parts = append(parts, bin+"-version:"+strings.TrimSpace(string(out)))
	}
	return parts, nil
}

// verifyToken is the cached result of a successful isolation check. A run
// trusts it only while Identity still matches the current runtime.
type verifyToken struct {
	Identity   string `json:"identity"`
	Guest      string `json:"guest"`
	Baseline   string `json:"baseline"`
	VerifiedAt string `json:"verified_at"`
}

// verifyCache holds one token per backend. A check passed on one backend
// never clears another: each boots its guest a different way.
type verifyCache struct {
	Backends map[string]verifyToken `json:"backends"`
}

// loadCache reads the cache. A missing, corrupt or older-format file (a
// single token, from before tokens were per backend) reads as empty, which
// costs one re-verify and never clears a run.
func loadCache() verifyCache {
	c := verifyCache{Backends: map[string]verifyToken{}}
	p, err := sandbox.VerifyCachePath()
	if err != nil {
		return c
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c
	}
	var got verifyCache
	if json.Unmarshal(b, &got) != nil || got.Backends == nil {
		return c
	}
	return got
}

func loadToken(backend string) (verifyToken, bool) {
	t, ok := loadCache().Backends[backend]
	return t, ok && t.Identity != ""
}

// storeToken records a verification for backend. It is best-effort: a
// cache-write failure only costs a re-verify next time, so it must never
// fail a build or run.
func storeToken(backend, id, guest, baseline string) {
	p, err := sandbox.VerifyCachePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	c := loadCache()
	c.Backends[backend] = verifyToken{
		Identity:   id,
		Guest:      guest,
		Baseline:   baseline,
		VerifiedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if b, err := json.Marshal(c); err == nil {
		// Beside and renamed, so a concurrent reader never sees half a file.
		tmp := p + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, p)
		}
	}
}

// isolationCurrent reports whether backend's cached token already cleared
// runtime id.
func isolationCurrent(backend, id string) bool {
	t, ok := loadToken(backend)
	return ok && t.Identity == id
}

// MarkVerified caches a successful isolation check of a sandbox with spec s,
// for its backend and the current runtime, so later runs can trust it without
// booting two more VMs. Callers that have just proven isolation (build, the
// verify command) use it to prime the cache.
func MarkVerified(s boxfile.Spec, guest, baseline string) {
	b, err := backendFor(s)
	if err != nil {
		return
	}
	if id, err := RuntimeIdentity(b.Name()); err == nil {
		storeToken(b.Name(), id, guest, baseline)
	}
}

// EnsureIsolated gates a run: it confirms the sandbox still gets its own kernel
// before the command executes. The full check boots two VMs, so it is skipped
// when the runtime is byte-for-byte the one a previous verification already
// cleared; a changed runtime forces a fresh check and a failed check refuses
// the run rather than executing on a shared kernel. It returns fresh=true when
// it had to re-verify, so the caller can explain the pause.
func EnsureIsolated(name string, s boxfile.Spec) (fresh bool, err error) {
	b, err := backendFor(s)
	if err != nil {
		return false, err
	}
	id, err := RuntimeIdentity(b.Name())
	if err != nil {
		return false, err
	}
	if isolationCurrent(b.Name(), id) {
		return false, nil // this exact runtime is already proven for this backend
	}
	guest, baseline, err := CheckIsolation(name, s)
	if err != nil {
		return true, err
	}
	storeToken(b.Name(), id, guest, baseline)
	return true, nil
}
