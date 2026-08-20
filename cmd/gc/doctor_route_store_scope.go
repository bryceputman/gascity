package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// routeStoreScopeCheck verifies that beads with gc.routed_to set to a
// rig-scoped agent (format "rig:NAME/agent") are not misrouted into the city
// store. Cross-scope routing leaves work invisible to the target agent.
type routeStoreScopeCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newRouteStoreScopeCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *routeStoreScopeCheck {
	return &routeStoreScopeCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *routeStoreScopeCheck) Name() string             { return "route-store-scope" }
func (c *routeStoreScopeCheck) CanFix() bool             { return false }
func (c *routeStoreScopeCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *routeStoreScopeCheck) WarmupEligible() bool     { return false }

func (c *routeStoreScopeCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.newStore == nil {
		return okCheck(c.Name(), "store factory unavailable; skipped")
	}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		return &doctor.CheckResult{
			Name:     c.Name(),
			Status:   doctor.StatusWarning,
			Severity: doctor.SeverityAdvisory,
			Message:  fmt.Sprintf("could not open city store: %v", err),
			FixHint:  "ensure the city bead store is accessible and rerun gc doctor",
		}
	}

	items, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		return &doctor.CheckResult{
			Name:     c.Name(),
			Status:   doctor.StatusWarning,
			Severity: doctor.SeverityAdvisory,
			Message:  fmt.Sprintf("could not list city beads: %v", err),
			FixHint:  "ensure the city bead store is accessible and rerun gc doctor",
		}
	}

	var violations []string
	for _, b := range items {
		routedTo := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
		if routedTo == "" {
			continue
		}
		// A bead in the city store routed to "rig:NAME/agent" is cross-scope.
		if strings.HasPrefix(routedTo, "rig:") {
			violations = append(violations, fmt.Sprintf("%s: gc.routed_to=%q in city store", b.ID, routedTo))
		}
	}

	if len(violations) == 0 {
		return okCheck(c.Name(), "no route-store-scope violations detected")
	}
	sort.Strings(violations)
	return &doctor.CheckResult{
		Name:     c.Name(),
		Status:   doctor.StatusWarning,
		Severity: doctor.SeverityAdvisory,
		Message:  fmt.Sprintf("%d bead(s) routed cross-scope (city store → rig agent)", len(violations)),
		FixHint:  "move these beads to the correct rig store or update their gc.routed_to metadata",
		Details:  violations,
	}
}

// inheritedRigSplitBrainCheck detects rigs whose .beads/config.yaml has an
// explicit Dolt endpoint that conflicts with the city-canonical endpoint.
// This split-brain state causes the rig to connect to a different Dolt server
// than the rest of the city, silently creating two divergent databases.
type inheritedRigSplitBrainCheck struct {
	cfg      *config.City
	cityPath string
}

func newInheritedRigSplitBrainCheck(cfg *config.City, cityPath string) *inheritedRigSplitBrainCheck {
	return &inheritedRigSplitBrainCheck{cfg: cfg, cityPath: cityPath}
}

func (c *inheritedRigSplitBrainCheck) Name() string             { return "inherited-rig-split-brain" }
func (c *inheritedRigSplitBrainCheck) CanFix() bool             { return false }
func (c *inheritedRigSplitBrainCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *inheritedRigSplitBrainCheck) WarmupEligible() bool     { return false }

func (c *inheritedRigSplitBrainCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck(c.Name(), "no city config; skipped")
	}
	if !workspaceUsesManagedBdStoreContract(c.cityPath, c.cfg.Rigs) {
		return okCheck(c.Name(), "workspace does not use managed Dolt; inherited rig split-brain not applicable")
	}

	cityConfigPath := filepath.Join(c.cityPath, ".beads", "config.yaml")
	cityState, _, err := contract.ReadConfigState(fsys.OSFS{}, cityConfigPath)
	if err != nil {
		return &doctor.CheckResult{
			Name:     c.Name(),
			Status:   doctor.StatusWarning,
			Severity: doctor.SeverityAdvisory,
			Message:  fmt.Sprintf("could not read city .beads/config.yaml: %v", err),
			FixHint:  "ensure the city .beads/config.yaml is readable and rerun gc doctor",
		}
	}
	cityUsesCanonical := cityState.EndpointOrigin == contract.EndpointOriginCityCanonical

	var conflicts []string
	for _, rig := range c.cfg.Rigs {
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			continue
		}
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(c.cityPath, rigPath)
		}
		rigConfigPath := filepath.Join(rigPath, ".beads", "config.yaml")
		if _, statErr := os.Stat(rigConfigPath); os.IsNotExist(statErr) {
			continue
		}
		rigState, ok, readErr := contract.ReadConfigState(fsys.OSFS{}, rigConfigPath)
		if readErr != nil || !ok {
			continue
		}
		// Split-brain: city uses canonical Dolt but rig has an explicit endpoint
		// pointing elsewhere, or the rig's endpoint disagrees with the city's.
		if rigState.EndpointOrigin == contract.EndpointOriginExplicit && cityUsesCanonical {
			if rigState.DoltHost != cityState.DoltHost || rigState.DoltPort != cityState.DoltPort {
				conflicts = append(conflicts, fmt.Sprintf(
					"rig %q: explicit endpoint %s:%s conflicts with city canonical %s:%s",
					rig.Name, rigState.DoltHost, rigState.DoltPort,
					cityState.DoltHost, cityState.DoltPort,
				))
			}
		}
	}

	if len(conflicts) == 0 {
		return okCheck(c.Name(), "no inherited-rig split-brain conditions detected")
	}
	sort.Strings(conflicts)
	return &doctor.CheckResult{
		Name:     c.Name(),
		Status:   doctor.StatusWarning,
		Severity: doctor.SeverityAdvisory,
		Message:  fmt.Sprintf("%d rig(s) have split-brain Dolt endpoint config", len(conflicts)),
		FixHint:  "run `gc beads dolt reconcile` or remove the stale endpoint config from the affected rig's .beads/config.yaml",
		Details:  conflicts,
	}
}
