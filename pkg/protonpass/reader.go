package protonpass

import (
	"context"
)

// NewReader builds a SecretRefReader-compatible closure backed by a Client.
// The returned function matches resolver.SecretRefReader and can be passed
// directly into the resolver via OpProvider/PPProvider.
//
// EnsureSession is invoked here so a misconfigured client fails fast at agent
// startup rather than on the first reconcile tick.
func NewReader(ctx context.Context, cfg ClientConfig) (func(context.Context, []string) (map[string]string, error), error) {
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := client.EnsureSession(ctx); err != nil {
		return nil, err
	}
	return client.ResolveAll, nil
}
