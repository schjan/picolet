package machine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/schjan/picolet/pkg/onepassword"
	"github.com/schjan/picolet/pkg/protonpass"
)

// RefReader resolves Secret References in one batch: the values it could
// resolve, and an error naming those it could not.
type RefReader func(ctx context.Context, refs []string) (map[string]string, error)

// Providers opens a secret provider's RefReader with the operator's token.
// A provider is opened only when a Host's bootstrap: lists a reference of it.
type Providers struct {
	// OnePassword opens 1Password with the service-account token in
	// tokenFile.
	OnePassword func(ctx context.Context, tokenFile string) (RefReader, error)
	// ProtonPass opens Proton Pass with the PAT in patFile, pass-cli keeping
	// its session (and local.key) in sessionDir.
	ProtonPass func(ctx context.Context, patFile, sessionDir string) (RefReader, error)
}

// provider is a secret provider the operator can pass the Machine's token
// of.
type provider struct {
	name string
	// flag names the operator's token file.
	flag string
	// scheme prefixes the provider's Secret References.
	scheme string
	// tokenName is the token's file name in a Host's secrets directory: the
	// token_path/pat_path the reference Agent config reads.
	tokenName string
}

var (
	//nolint:gosec // G101: names of a flag and a token file, not a credential
	onePassword = &provider{name: "1Password", flag: "--onepassword-token-file", scheme: onepassword.Prefix, tokenName: "op-service-account-token"}
	protonPass  = &provider{name: "Proton Pass", flag: "--protonpass-pat-file", scheme: protonpass.Prefix, tokenName: "pp-pat"}
	providers   = []*provider{onePassword, protonPass}
)

// providerOf is the provider of ref, by its scheme. Loading the Fleet has
// checked every bootstrap: reference is one of a provider.
func providerOf(ref string) *provider {
	for _, p := range providers {
		if strings.HasPrefix(ref, p.scheme) {
			return p
		}
	}
	return nil
}

// ProviderToken is the Machine's provider token the operator passed.
type ProviderToken struct {
	provider *provider
	// Path is the token file, symlinks resolved, checked to lie outside the
	// Fleet checkout.
	Path string
	// Content is the token: a credential, never printed.
	Content []byte
}

// Resolved is what the operator's provider resolved of the Machine's
// bootstrap: references.
type Resolved struct {
	// Values maps a resolved reference to its value, never printed.
	Values map[string]string
	// Missing maps a reference not resolved to why.
	Missing map[string]string
	// Warning is the provider's error for the references it could not
	// resolve; empty when it resolved them all.
	Warning string
}

// tokenFlag is the provider the operator passed a token for and the file;
// nil when none. Two providers are an error: the Machine has one token.
func (c Config) tokenFlag() (*provider, string, error) {
	switch {
	case c.OnePasswordTokenFile != "" && c.ProtonPassPATFile != "":
		return nil, "", errors.New("--onepassword-token-file and --protonpass-pat-file exclude each other: " +
			"pass the one provider token of the Machine")
	case c.OnePasswordTokenFile != "":
		return onePassword, c.OnePasswordTokenFile, nil
	case c.ProtonPassPATFile != "":
		if !c.Env.PassCLI {
			return nil, "", errors.New("pass-cli is not installed (no pass-cli on PATH): --protonpass-pat-file needs the " +
				"Proton Pass CLI, see https://protonpass.github.io/pass-cli/; bootstrap machine installs no packages")
		}
		return protonPass, c.ProtonPassPATFile, nil
	}
	return nil, "", nil
}

// resolveRefs resolves the bootstrap: references of hosts in one batch
// through the provider of token, opened with it. A reference of another
// provider, or any when token is nil, is missing. Opening the provider
// failing is an error.
func resolveRefs(ctx context.Context, open Providers, token *ProviderToken, hosts []Host) (Resolved, error) {
	res := Resolved{Values: map[string]string{}, Missing: map[string]string{}}
	var prov *provider
	if token != nil {
		prov = token.provider
	}
	refs := batchRefs(prov, hosts, res.Missing)
	if len(refs) == 0 {
		return res, nil
	}
	values, readErr, err := readBatch(ctx, open, token, refs)
	if err != nil {
		return Resolved{}, err
	}
	for _, ref := range refs {
		if v, ok := values[ref]; ok {
			res.Values[ref] = v
		} else {
			res.Missing[ref] = "not resolved by " + prov.name
		}
	}
	if readErr != nil && len(res.Values) < len(refs) {
		res.Warning = prov.name + ": " + redact(readErr.Error(), token, slices.Collect(maps.Values(values)))
	}
	return res, nil
}

// redact replaces the token and every value in msg with <redacted>: a
// provider's error text is shown for its diagnosis, never a credential
// bootstrap holds, whatever the provider put into it.
func redact(msg string, token *ProviderToken, values []string) string {
	secrets := slices.Concat(values, []string{string(token.Content), strings.TrimSpace(string(token.Content))})
	// Longest first, so a secret containing another is replaced whole.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "<redacted>")
		}
	}
	return msg
}

// batchRefs is every reference of hosts' bootstrap: blocks prov resolves,
// sorted, each once; a reference of another provider goes to missing with
// the flag it needs.
func batchRefs(prov *provider, hosts []Host, missing map[string]string) []string {
	batch := map[string]bool{}
	for _, h := range hosts {
		for _, ref := range h.Bootstrap {
			if p := providerOf(ref); p != prov {
				missing[ref] = "needs " + p.flag
				continue
			}
			batch[ref] = true
		}
	}
	return slices.Sorted(maps.Keys(batch))
}

// readBatch opens token's provider with it and reads refs in one call. A
// Proton Pass session lives in a private temporary directory, removed
// before readBatch returns whatever happened, so no session or local.key is
// left behind. readErr is the reader's error for the references it could
// not resolve; err is opening, or cleaning up, failing.
func readBatch(ctx context.Context, open Providers, token *ProviderToken, refs []string) (values map[string]string, readErr, err error) {
	prov := token.provider
	var reader RefReader
	switch prov {
	case onePassword:
		reader, err = open.OnePassword(ctx, token.Path)
	case protonPass:
		sessionDir, mkErr := os.MkdirTemp("", "picolet-protonpass-")
		if mkErr != nil {
			return nil, nil, fmt.Errorf("creating the Proton Pass session directory: %w", mkErr)
		}
		defer func() {
			if rmErr := os.RemoveAll(sessionDir); rmErr != nil {
				err = errors.Join(err, fmt.Errorf("removing the Proton Pass session directory: %w", rmErr))
			}
		}()
		reader, err = open.ProtonPass(ctx, token.Path, sessionDir)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s with %s: %s", prov.name, prov.flag, redact(err.Error(), token, nil))
	}
	values, readErr = reader(ctx, refs)
	return values, readErr, nil
}
