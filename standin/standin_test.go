package standin_test

import (
	"testing"

	"github.com/zh0labs/agent/test/contract"
	"github.com/zh0labs/agent/test/standin"
)

// The stand-in keeps the provider contract that Keycloak keeps in the integration test.
func TestStandInKeepsTheProviderContract(t *testing.T) {
	for _, profile := range []string{"keycloak", "auth0"} {
		t.Run(profile, func(t *testing.T) {
			provider := standin.Start(t, profile)
			contract.Run(t, contract.Provider{
				Environ: provider.Environ(),
				Token:   provider.Token(t, map[string]any{contract.TenantClaim: contract.Tenant}),
				Foreign: provider.Token(t, map[string]any{contract.TenantClaim: contract.Tenant, "aud": "https://other.example"}),
			})
		})
	}
}
