package validator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/schjan/picolet/pkg/config"
	"github.com/schjan/picolet/pkg/resolver"
)

// validateAgentBundles renders each Host's Agent bundle as bootstrap's Agent
// resolution does: strictly, keyless, with placeholder file secrets. A
// provider ref the bundle needs at bootstrap (a template-function call or a
// direct provider `secrets:` ref) fails here instead of at the next
// `bootstrap machine`. Hosts without an assigned Agent bundle are skipped.
func validateAgentBundles(ctx context.Context, fsys fs.FS, cfg *config.Config, log *slog.Logger) error {
	var errs []error
	for _, hostname := range cfg.SortedHostnames() {
		host, ok := cfg.FindHost(hostname)
		if !ok {
			continue // unreachable: hostname comes from cfg's own Hosts
		}
		// The only error is ErrAgentNotAssigned: no Agent bundle to render.
		service, err := cfg.AssignedAgentService(host)
		if err != nil {
			continue
		}
		if err := renderAgentBundle(ctx, fsys, cfg, hostname, service, log); err != nil {
			errs = append(errs, fmt.Errorf("host %s: Agent bundle %s: %w", hostname, service, err))
		}
	}
	return errors.Join(errs...)
}

// renderAgentBundle resolves service for hostname in strict mode with no
// provider readers and placeholder file secrets.
func renderAgentBundle(ctx context.Context, fsys fs.FS, cfg *config.Config, hostname, service string, log *slog.Logger) error {
	r, err := resolver.New(resolver.Config{
		FS:           fsys,
		Config:       cfg,
		SecretReader: func(string) (string, error) { return unresolvedSecretPlaceholder, nil },
		Strict:       true,
		Logger:       log,
	})
	if err != nil {
		return fmt.Errorf("creating resolver: %w", err)
	}
	if _, err := r.ResolveServicesForHost(ctx, hostname, []string{service}); err != nil {
		return err
	}
	return nil
}
