package main

import (
	"errors"
	"fmt"

	"github.com/aicix-labs/factoryd/internal/config"
	"github.com/aicix-labs/factoryd/internal/proc"
	"github.com/aicix-labs/factoryd/internal/state"
)

// claimLongRunningService records this exact process for each factory it will
// keep serving. A status server can serve more than one factory, so every
// state document gets the same handle. No service work starts until every
// claim succeeds.
func claimLongRunningService(cfgs []*config.Config, service state.Service) (func() error, error) {
	holder, err := proc.Self("service " + string(service))
	if err != nil {
		return nil, fmt.Errorf("recording %s service process: %w", service, err)
	}
	claimed := make([]*config.Config, 0, len(cfgs))
	for _, cfg := range cfgs {
		if _, err := state.Update(cfg.StatePath(), cfg.Name, func(s *state.State) error {
			return s.ClaimService(service, holder)
		}); err != nil {
			// The process has not begun serving. Best-effort release leaves a
			// failed release conservative: the still-live caller remains visible
			// until it exits, after which a later service can reclaim the slot.
			_ = releaseLongRunningService(claimed, service, holder)
			return nil, fmt.Errorf("recording %s service for factory %q: %w", service, cfg.Name, err)
		}
		claimed = append(claimed, cfg)
	}
	return func() error { return releaseLongRunningService(claimed, service, holder) }, nil
}

// releaseLongRunningService clears only this process's registrations. Joining
// errors means an exit cannot look clean while a durable live handle might
// strand a later instance.
func releaseLongRunningService(cfgs []*config.Config, service state.Service, holder proc.Ref) error {
	var errs []error
	for _, cfg := range cfgs {
		if _, err := state.Update(cfg.StatePath(), cfg.Name, func(s *state.State) error {
			s.ReleaseService(service, holder)
			return nil
		}); err != nil {
			errs = append(errs, fmt.Errorf("clearing %s service for factory %q: %w", service, cfg.Name, err))
		}
	}
	return errors.Join(errs...)
}
