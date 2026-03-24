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

// ValidateReference checks whether ref looks like a 1Password secret reference.
func ValidateReference(ref string) bool {
	return strings.HasPrefix(ref, "op://")
}

// NewReaderFromTokenFile reads a service account token from disk and returns
// a secret reader closure. Returns (nil, nil) when tokenPath is empty.
func NewReaderFromTokenFile(ctx context.Context, tokenPath string) (func(string) (string, error), error) {
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
	return func(ref string) (string, error) {
		//nolint:contextcheck // template functions don't receive context; SDK calls are fast
		return client.Resolve(ctx, ref)
	}, nil
}
