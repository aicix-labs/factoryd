package main

import (
	"testing"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/state"
)

// Both long-running CLI modes must write their own durable process handles,
// not merely rely on supervisor state. Doctor's stale-binary check has no
// useful subject otherwise.
func TestLongRunningServicesRegisterAndRelease(t *testing.T) {
	cfg := healthCfg(t)
	for _, service := range []state.Service{state.ServiceStatusServe, state.ServiceHealthLoop} {
		release, err := claimLongRunningService([]*config.Config{cfg}, service)
		if err != nil {
			t.Fatalf("claim %s: %v", service, err)
		}
		st, err := state.Load(cfg.StatePath(), cfg.Name)
		if err != nil {
			t.Fatal(err)
		}
		ref := st.Service(service)
		if ref == nil || ref.PID <= 0 || ref.StartToken == "" {
			t.Fatalf("%s service handle = %+v, want exact durable handle", service, ref)
		}
		if err := release(); err != nil {
			t.Fatalf("release %s: %v", service, err)
		}
		st, err = state.Load(cfg.StatePath(), cfg.Name)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Service(service); got != nil {
			t.Fatalf("released %s remains recorded: %+v", service, got)
		}
	}
}
