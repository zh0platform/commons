//go:build keycloak

package keycloak_test

import (
	"net/url"
	"testing"

	"git.kloudbox.de/commons/commons/keycloak"
)

// The module's pinned Keycloak starts with a realm file, serves the run's certificate and signs a user in through its
// own form, back to the client's callback: a pin change that breaks the start or the form fails here.
func TestSignInThroughTheRealmsForm(t *testing.T) {
	address, authority := keycloak.URL(t)
	realm := keycloak.ReadRealm(t, "../internal/selftest/realms/test.json")
	user := realm.Users[0]
	query := url.Values{
		"client_id": {realm.Clients[0].ID}, "response_type": {"code"}, "scope": {"openid"},
		"redirect_uri": {realm.Clients[0].RedirectURIs[0]}, "state": {"state-1"},
	}
	authorization := address + "/realms/" + realm.Name + "/protocol/openid-connect/auth?" + query.Encode()
	callback := keycloak.SignIn(t, keycloak.Browser(t, authority), authorization, user.Name, user.Credentials[0].Value)
	if callback.Host != "app.test" || callback.Query().Get("code") == "" || callback.Query().Get("state") != "state-1" {
		t.Errorf("SignIn(%s) = %s, want the callback with a code and the state", user.Name, callback)
	}
}
