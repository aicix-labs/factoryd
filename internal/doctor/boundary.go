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
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: p.uid, Gid: p.gid, NoSetGroups: true}}
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
