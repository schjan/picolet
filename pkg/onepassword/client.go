package onepassword

import (
	"context"
	"fmt"
	"os"
	"strings"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/schjan/picolet/pkg/version"
)

// Client wraps the 1Password SDK for secret resolution.
type Client struct {
	secrets op.SecretsAPI
}

// NewClient creates a 1Password SDK client with the given service account token.
func NewClient(ctx context.Context, token string) (*Client, error) {
	client, err := op.NewClient(ctx,
		op.WithServiceAccountToken(token),
		op.WithIntegrationInfo("picolet", version.Version),
	)
	if err != nil {
		return nil, fmt.Errorf("creating 1password client: %w", err)
	}
	return &Client{secrets: client.Secrets()}, nil
}

// Resolve fetches a secret value by its 1Password reference (e.g. "op://vault/item/field").
func (c *Client) Resolve(ctx context.Context, ref string) (string, error) {
	val, err := c.secrets.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolving 1password secret %q: %w", ref, err)
	}
	return val, nil
}

// NewReaderFromTokenFile reads a service account token from disk and returns
// a secret reader closure. Returns (nil, nil) when tokenPath is empty.
// The init context is used only for SDK client creation; each call receives its own context.
//
//nolint:nilnil // nil reader signals "1Password not configured"; callers check for nil
func NewReaderFromTokenFile(ctx context.Context, tokenPath string) (func(context.Context, string) (string, error), error) {
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
	return func(ctx context.Context, ref string) (string, error) {
		return client.Resolve(ctx, ref)
	}, nil
}
