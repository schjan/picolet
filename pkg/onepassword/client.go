package onepassword

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/schjan/picolet/pkg/version"
)

// Client wraps the 1Password SDK for secret resolution.
//
// opClient is retained for the lifetime of this struct. The SDK registers a
// runtime finalizer on *op.Client (onepassword-sdk-go client_builder.go) that
// calls core.ReleaseClient and invalidates the underlying client ID. Storing
// only the SecretsAPI interface — which does not back-reference *op.Client —
// allows GC to reclaim the SDK client prematurely, producing "invalid client
// id" errors on subsequent Resolve calls. Do not remove this field.
type Client struct {
	opClient *op.Client
}

// onepassword-sdk-go v0.3.x initializes a shared global core without
// synchronization. Guard client creation to avoid concurrent initialization.
var newClientMu sync.Mutex

// NewClient creates a 1Password SDK client with the given service account token.
func NewClient(ctx context.Context, token string) (*Client, error) {
	newClientMu.Lock()
	defer newClientMu.Unlock()

	c, err := op.NewClient(ctx,
		op.WithServiceAccountToken(token),
		op.WithIntegrationInfo("picolet", version.Version),
	)
	if err != nil {
		return nil, fmt.Errorf("creating 1password client: %w", err)
	}
	return &Client{opClient: c}, nil
}

// ResolveAll fetches multiple secrets in a single SDK call.
// Returns successfully resolved secrets and any per-reference errors separately,
// so callers can use partial results (e.g. skip a broken secret while still
// resolving the git token).
func (c *Client) ResolveAll(ctx context.Context, refs []string) (map[string]string, error) {
	if len(refs) == 0 {
		return nil, nil //nolint:nilnil // empty input → no work
	}
	resp, err := c.opClient.Secrets().ResolveAll(ctx, refs)
	if err != nil {
		return nil, fmt.Errorf("resolving 1password secrets: %w", err)
	}
	results := make(map[string]string, len(resp.IndividualResponses))
	var errs []error
	for ref, r := range resp.IndividualResponses {
		if r.Error != nil {
			errs = append(errs, fmt.Errorf("resolving 1password secret %q: %s", ref, r.Error.Type))
			continue
		}
		if r.Content == nil {
			errs = append(errs, fmt.Errorf("resolving 1password secret %q: empty response", ref))
			continue
		}
		results[ref] = r.Content.Secret
	}
	return results, errors.Join(errs...)
}

// NewReaderFromTokenFile reads a service account token from disk and returns
// a batch secret reader closure. Returns (nil, nil) when tokenPath is empty.
// The init context is used only for SDK client creation; each call receives its own context.
//
//nolint:nilnil // nil reader signals "1Password not configured"; callers check for nil
func NewReaderFromTokenFile(ctx context.Context, tokenPath string) (func(context.Context, []string) (map[string]string, error), error) {
	if tokenPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading 1password token: %w", err)
	}
	client, err := NewClient(ctx, strings.TrimSpace(string(data)))
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, refs []string) (map[string]string, error) {
		return client.ResolveAll(ctx, refs)
	}, nil
}
