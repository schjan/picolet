package resolver

import (
	"errors"
	"fmt"

	"github.com/schjan/picolet/pkg/config"
)

// ErrSopsInUnitFile is returned (wrapped) when a Quadlet, a Raw Unit or a
// Hook definition calls readSopsSecret during its render, directly or
// through renderTemplate.
var ErrSopsInUnitFile = errors.New("SOPS values can't be rendered")

// unitRender is the top-level file whose render may not read SOPS values:
// a Quadlet or a Raw Unit (written world-readable, Environment= shown by
// `systemctl show`) or a Hook definition (logged and stored in state).
type unitRender struct {
	file   string
	kind   string
	reason string
}

// guardedRender returns the unitRender for a top-level file of the given
// destination, or nil when SOPS values may be rendered into it.
func guardedRender(file string, dest config.Destination) *unitRender {
	switch dest {
	case config.DestQuadlet:
		return &unitRender{file: file, kind: "a Quadlet", reason: "the file is world-readable and Environment= values show in `systemctl show`"}
	case config.DestSystemd:
		return &unitRender{file: file, kind: "a Raw Unit", reason: "the file is world-readable and Environment= values show in `systemctl show`"}
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

// sopsGuard is the SOPS provider's Guard: it fails a readSopsSecret call
// while a guarded file renders, before any reader sees the ref, so the
// keyless and the decrypting path refuse it alike.
func (r *Resolver) sopsGuard(funcName, ref string) error {
	g := r.rendering
	if g == nil {
		return nil
	}
	return fmt.Errorf("%s: %s %q: %w into %s (%s); use Secret= (or a secret mounted as a file) instead",
		g.file, funcName, ref, ErrSopsInUnitFile, g.kind, g.reason)
}
