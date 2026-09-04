//go:build unix

package health

import (
	"fmt"
	"syscall"

	"github.com/aicix-labs/factoryd/internal/state"
)

// HostProbes reads the real host.
type HostProbes struct{}

func (HostProbes) Alive(rs state.RoleState) (bool, error) {
	if rs.Supervisor == nil {
		return false, fmt.Errorf("no supervisor handle")
	}
	return rs.Supervisor.Alive()
}

func (HostProbes) Statfs(path string) (Volume, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return Volume{}, err
	}
	total := fs.Blocks * uint64(fs.Bsize)
	free := fs.Bavail * uint64(fs.Bsize) // what an unprivileged writer can use
	v := Volume{Path: path, TotalBytes: total, FreeBytes: free}
	if total > 0 {
		v.FreePercent = 100 * float64(free) / float64(total)
	}
	return v, nil
}

func (HostProbes) DeviceID(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}
