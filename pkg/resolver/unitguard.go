package resolver

import (
	"errors"
	"fmt"

	"github.com/schjan/picolet/pkg/config"
)

// The unit-file errors are returned (wrapped) when a Quadlet, a Raw Unit or
// a Hook definition calls a provider's template function during its render,
// directly or through renderTemplate: one per scheme.
var (
	// ErrSopsInUnitFile is the unit-file error of readSopsSecret and
	// readSopsData.
	ErrSopsInUnitFile = errors.New("SOPS values can't be rendered")
	// ErrOpInUnitFile is the unit-file error of readOpSecret.
	ErrOpInUnitFile = errors.New("1Password values can't be rendered")
	// ErrProtonPassInUnitFile is the unit-file error of readProtonPassSecret.
	ErrProtonPassInUnitFile = errors.New("Proton Pass values can't be rendered") //nolint:staticcheck // ST1005: "Proton Pass" is a product name
)

// unitFileReason is why a Quadlet or a Raw Unit may not carry secret values.
const unitFileReason = "the file is world-readable and Environment= values show in `systemctl show`"

// unitRender is the top-level file whose render may not read provider
// values: a Quadlet or a Raw Unit (written world-readable, Environment=
// shown by `systemctl show`) or a Hook definition (logged and stored in
// state).
type unitRender struct {
	file   string
	kind   string
	reason string
}

// guardedRender returns the unitRender for a top-level file of the given
// destination, or nil when provider values may be rendered into it.
func guardedRender(file string, dest config.Destination) *unitRender {
	switch dest {
	case config.DestQuadlet:
		return &unitRender{file: file, kind: "a Quadlet", reason: unitFileReason}
	case config.DestSystemd:
		return &unitRender{file: file, kind: "a Raw Unit", reason: unitFileReason}
	case config.DestData, config.DestSecret:
	}
	return nil
}

// hookRender is the unitRender of a Service Bundle's Hook definition file.
func hookRender(file string) *unitRender {
	return &unitRender{file: file, kind: "a Hook definition", reason: "Hook definitions are logged and stored in state"}
}

// withRender runs render with g as the guarded top-level file (nil: none).
// Template rendering within one resolve is serial (see renderTemplate), and
// each resolve works on its own Resolver copy, so one field suffices.
func (r *Resolver) withRender(g *unitRender, render func()) {
	prev := r.rendering
	r.rendering = g
	defer func() { r.rendering = prev }()
	render()
}

// guarded sets p's Guard to the unit-file guard: it fails a call of p's
// functions while a guarded file renders, with p's unitFileErr, before any
// reader or cache sees the ref, so the ref is never collected and every
// path (with or without a reader, strict or not) refuses it alike.
func (r *Resolver) guarded(p ProviderTemplate) ProviderTemplate {
	p.Guard = func(funcName, ref string) error {
		g := r.rendering
		if g == nil {
			return nil
		}
		return fmt.Errorf("%s: %s %q: %w into %s (%s); use Secret= (or a secret mounted as a file) instead",
			g.file, funcName, refForError(ref), p.unitFileErr, g.kind, g.reason)
	}
	return p
}

// guardedSopsProvider is SopsProvider over this host resolve's SOPS reader
// with the unit-file guard, which also counts the provider's calls
// (sopsCalls), since it runs on every one.
func (r *Resolver) guardedSopsProvider() ProviderTemplate {
	p := r.guarded(SopsProvider(r.resolveSopsReader(), r.noSopsHint))
	guard := p.Guard
	p.Guard = func(funcName, ref string) error {
		r.sopsCalls++
		return guard(funcName, ref)
	}
	return p
}
