package supervise

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/aicix-labs/factoryd/internal/config"
)

// ProbeReach starts `exe _netprobe addr` exactly as a turn of spec would be
// started -- same credential, same sandbox when sandboxed is true -- and
// reports whether it reached addr. It exists for doctor: "the sandbox is
// configured" is not "the sandbox holds", and the only way to know the
// namespace takes the network away is to try to use it from inside.
func ProbeReach(ctx context.Context, spec config.RoleSpec, exe, addr string, sandboxed bool) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "_netprobe", addr)
	cmd.Env = []string{"PATH=" + spec.Env["PATH"]}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if sandboxed {
		if spec.Sandbox == nil || !spec.Sandbox.NoNetwork {
			return false, fmt.Errorf("the role declares no sandbox to probe")
		}
		if err := applyNoNetwork(cmd.SysProcAttr); err != nil {
			return false, err
		}
	}
	if spec.RunAs != nil && spec.RunAs.User != "" {
		cred, err := credentialFor(spec.RunAs.User)
		if err != nil {
			return false, err
		}
		if !isSelf(cred) {
			cmd.SysProcAttr.Credential = cred
		}
	}
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return false, nil // the probe ran and could not connect
	}
	return false, fmt.Errorf("probe did not run: %w", err)
}
