package sops_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/schjan/picolet/internal/testsops"
)

// TestCheckIgnoresAzureCredentials checks a rule naming a versionless Azure
// Key Vault URL, at the top level and in key_groups, with garbage Azure
// credential settings in the environment: Check never builds the Azure
// backend, so the result does not depend on them, nor on the network.
//
// t.Setenv rules out t.Parallel(), so this test has no subtests.
func TestCheckIgnoresAzureCredentials(t *testing.T) {
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "garbage")
	t.Setenv("AZURE_CLIENT_ID", "garbage")
	t.Setenv("AZURE_TENANT_ID", "garbage")
	connections := trapNetwork(t)
	srv1, srv2, op := testsops.Recipient(t, "srv-1"), testsops.Recipient(t, "srv-2"), testsops.Recipient(t, "operator-1")
	azure := "https://unroutable.vault.azure.net/keys/k"

	topLevel := "creation_rules:\n  - path_regex: ^secrets/\n    age: " + srv1 + "," + srv2 + "," + op + "\n    azure_keyvault: " + azure + "\n"
	require.NoError(t, rulesOf(t, topLevel).Check("secrets/db.sops.yaml", inspectSOPS(t, "values.sops.yaml")))

	groups := "creation_rules:\n  - path_regex: ^secrets/\n    key_groups:\n      - age: [" + srv1 + ", " + srv2 + ", " + op + "]\n" +
		"        azure_keyvault: [{vaultUrl: \"https://unroutable.vault.azure.net\", key: k}]\n"
	require.NoError(t, rulesOf(t, groups).Check("secrets/db.sops.yaml", inspectSOPS(t, "values.sops.yaml")))

	require.Zero(t, connections.Load(), "Check attempted a network connection")
}
