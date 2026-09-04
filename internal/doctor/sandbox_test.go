package doctor_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/doctor"
)

// The sandbox check is proved from inside against a listener doctor opens:
// sandboxed must not reach it, unsandboxed must. Each way the proof can be
// wrong is a failure with its own words.
func TestNoNetworkProbeNeedsBothHalves(t *testing.T) {
	cases := map[string]struct {
		reach    func(ctx context.Context, spec config.RoleSpec, addr string, sandboxed bool) (bool, error)
		wantOK   bool
		wantWord string
	}{
		"holds": {func(_ context.Context, _ config.RoleSpec, _ string, sandboxed bool) (bool, error) {
			return !sandboxed, nil
		}, true, ""},
		"sandboxed turn still reaches": {func(_ context.Context, _ config.RoleSpec, _ string, _ bool) (bool, error) { return true, nil }, false, "not holding"},
		"control cannot reach either":  {func(_ context.Context, _ config.RoleSpec, _ string, _ bool) (bool, error) { return false, nil }, false, "probe is broken"},
		"sandbox cannot be applied": {func(_ context.Context, _ config.RoleSpec, _ string, sandboxed bool) (bool, error) {
			if sandboxed {
				return false, errors.New("needs root")
			}
			return true, nil
		}, false, "cannot be applied"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := fixture(t)
			cfg.Roles.Producer.Sandbox = &config.Sandbox{NoNetwork: true}
			deps := healthyDeps(cfg, nil)
			deps.Reach = c.reach
			r := doctor.RunWith(context.Background(), cfg, deps)
			var got *doctor.Check
			for i := range r.Checks {
				if r.Checks[i].Name == "sandbox producer" {
					got = &r.Checks[i]
				}
			}
			if got == nil {
				t.Fatalf("no sandbox check in report:\n%s", r)
			}
			if got.OK != c.wantOK {
				t.Fatalf("ok=%v err=%v, want ok=%v", got.OK, got.Err, c.wantOK)
			}
			if !c.wantOK && !strings.Contains(got.Err.Error(), c.wantWord) {
				t.Fatalf("err=%v, want %q", got.Err, c.wantWord)
			}
		})
	}
}

// Without a sandbox declared there is no sandbox check: the report must not
// print an "ok" for something it did not probe.
func TestNoSandboxNoCheck(t *testing.T) {
	cfg := fixture(t)
	r := doctor.RunWith(context.Background(), cfg, healthyDeps(cfg, nil))
	for _, c := range r.Checks {
		if strings.HasPrefix(c.Name, "sandbox ") {
			t.Fatalf("a sandbox check appeared with none declared: %+v", c)
		}
	}
}
