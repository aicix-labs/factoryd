package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
)

// Prober runs a write attempt as the producer's OS identity. Injectable so the
// package can be tested without privilege; the real one needs it.
type Prober interface {
	// CanWrite reports whether the producer principal can create a file in
	// dir. It must attempt the write, not reason about permissions: a
	// reasoned answer is an assumption wearing a check's clothes.
	CanWrite(ctx context.Context, dir string) (bool, error)
	// CanRead reports whether the principal can read the file at path. Used
	// against credential files: a gate that can read the reviewer's token has
	// the two-party model in its hands.
	CanRead(ctx context.Context, path string) (bool, error)
	// CanExec reports whether the principal can execute the file at path,
	// traversal included. An execute bit is not executability: a root-owned
	// 0700 binary has one, and doctor running as root can run it, and the
	// producer or gate cannot.
	CanExec(ctx context.Context, path string) (bool, error)
	// Own makes path writable by the principal -- what submit does for each
	// declared gate path when it creates it. A no-op when not privileged.
	Own(path string) error
	// Describe names the principal, for the report.
	Describe() string
}

// setuidProber forks a child that switches to the producer's uid/gid and tries
// to create a file. It needs CAP_SETUID; when factoryd does not have it, the
// probe fails rather than falling back to reasoning about mode bits.
type setuidProber struct {
	name string
	uid  uint32
	gid  uint32
}

// NewProber resolves the configured producer user. A user that does not exist
// is a configuration error named here, not a numeric id nobody recognises.
func NewProber(ra *config.RunAs) (Prober, error) {
	if ra == nil || ra.User == "" {
		return nil, fmt.Errorf("roles.producer.run_as.user is empty")
	}
	u, err := user.Lookup(ra.User)
	if err != nil {
		return nil, fmt.Errorf("run_as user %q: %w", ra.User, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	return &setuidProber{name: ra.User, uid: uint32(uid), gid: uint32(gid)}, nil
}

func (p *setuidProber) Describe() string {
	return fmt.Sprintf("%s (uid %d)", p.name, p.uid)
}

// CanWrite attempts the write as the producer, via a child process that
// switches identity before touching anything. The parent keeps its own
// identity, so doctor's other checks are unaffected.
func (p *setuidProber) CanWrite(ctx context.Context, dir string) (bool, error) {
	if uint32(os.Geteuid()) == p.uid {
		// Same identity as doctor itself: there is no boundary to probe.
		return false, fmt.Errorf("doctor runs as uid %d, the same identity as the producer; there is no boundary to verify", p.uid)
	}
	probe := filepath.Join(dir, fmt.Sprintf(".factoryd-probe-%d", time.Now().UnixNano()))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// A shell one-liner, so no factoryd code runs as the producer.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `touch "$1" && rm -f "$1"`, "probe", probe)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	// Groups explicitly empty: with NoSetGroups the child keeps factoryd's
	// supplementary groups -- root's -- and answers the question for the
	// wrong principal. The live probe reported "CAN write .git" while sudo -u
	// was refused, which is how this was found.
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: p.uid, Gid: p.gid, Groups: []uint32{}}}
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if asExitError(err, &ee) {
		return false, nil // the write was refused: that is an answer
	}
	// Could not even start the child -- most often EPERM from setuid without
	// privilege. That is undecided, which is not the same as satisfied.
	return false, fmt.Errorf("could not run the probe as %s: %w (factoryd needs CAP_SETUID or root to switch user)", p.Describe(), err)
}

// CanRead attempts a read as the principal, by the same mechanism as CanWrite.
func (p *setuidProber) CanRead(ctx context.Context, path string) (bool, error) {
	if uint32(os.Geteuid()) == p.uid {
		return false, fmt.Errorf("doctor runs as uid %d, the same identity as %s; there is no boundary to verify", p.uid, p.name)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `head -c 1 "$1" >/dev/null`, "probe", path)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: p.uid, Gid: p.gid, Groups: []uint32{}}}
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if asExitError(err, &ee) {
		return false, nil
	}
	return false, fmt.Errorf("could not run the read probe as %s: %w (factoryd needs CAP_SETUID or root to switch user)", p.Describe(), err)
}

// CanExec attempts test -x as the principal, by the same mechanism as the
// other probes. Traversal is exercised by the child's own path lookup.
func (p *setuidProber) CanExec(ctx context.Context, path string) (bool, error) {
	if uint32(os.Geteuid()) == p.uid {
		return false, fmt.Errorf("doctor runs as uid %d, the same identity as %s; there is no boundary to verify", p.uid, p.name)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `test -x "$1"`, "probe", path)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: p.uid, Gid: p.gid, Groups: []uint32{}}}
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if asExitError(err, &ee) {
		return false, nil
	}
	return false, fmt.Errorf("could not run the exec probe as %s: %w (factoryd needs CAP_SETUID or root to switch user)", p.Describe(), err)
}

// Own chowns path to the principal. Without privilege it does nothing and
// returns nil: the subsequent write probe then reports whether the host is
// already arranged correctly, which is the honest answer an unprivileged doctor
// can give.
func (p *setuidProber) Own(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	// Never through a link. Chown follows symlinks, so a declared path that
	// is a link would hand its TARGET to the gate. The guard refuses such a
	// path before this runs; this is the second lock on the same door.
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to give away its target", path)
	}
	return os.Chown(path, int(p.uid), int(p.gid))
}

func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
