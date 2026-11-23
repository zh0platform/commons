// Package keycloak is test support for a Keycloak started from keycloak.yaml: where it is, a client that trusts it,
// what a realm file declares, and a sign-in through Keycloak's own login form with an HTTP client. It names nothing of
// the project using it.
package keycloak

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The variables the test runner sets for a run with Keycloak.
const (
	URLVariable = "KEYCLOAK_TEST_URL" // https://host:port
	CAVariable  = "KEYCLOAK_TEST_CA"  // the authority of its certificate, PEM
)

var formAction = regexp.MustCompile(`<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)

// Realm is what a realm file declares.
type Realm struct {
	Name    string `json:"realm"`
	Clients []struct {
		ID           string   `json:"clientId"`
		Secret       string   `json:"secret"`
		RedirectURIs []string `json:"redirectUris"`
	} `json:"clients"`
	Users []struct {
		Name        string `json:"username"`
		Credentials []struct {
			Type       string `json:"type"`
			Value      string `json:"value"`      // a password's
			SecretData string `json:"secretData"` // an OTP credential's, JSON with its base32 secret as "value"
		} `json:"credentials"`
	} `json:"users"`
}

// ReadRealm reads a realm file.
func ReadRealm(t *testing.T, path string) Realm {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var realm Realm
	if err := json.Unmarshal(raw, &realm); err != nil {
		t.Fatal(err)
	}
	return realm
}

// URL is the running Keycloak's address and the PEM of its authority; the run must have started it.
func URL(t *testing.T) (string, string) {
	t.Helper()
	address, authority := os.Getenv(URLVariable), os.Getenv(CAVariable)
	if address == "" || authority == "" {
		t.Fatalf("%s and %s are not set: run the test through the runner with Keycloak", URLVariable, CAVariable)
	}
	return address, authority
}

// Browser is an HTTP client that trusts the authority, keeps cookies and follows no redirects, as the browser's part
// of a sign-in needs.
func Browser(t *testing.T, authority string) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(authority)) {
		t.Fatal("the authority is not PEM")
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
		Jar:       jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// SignIn opens the authorization URL, signs the user in through Keycloak's login form (unless Keycloak's session of this
// browser still holds and it answers at once) and returns the callback URL Keycloak sends the browser to.
func SignIn(t *testing.T, browser *http.Client, authorization, user, password string) *url.URL {
	t.Helper()
	page := do(t, browser, http.MethodGet, authorization, nil)
	if page.status == http.StatusFound { // Keycloak's own session of this browser still holds: no form
		callback, err := url.Parse(page.location)
		if err != nil {
			t.Fatal(err)
		}
		return callback
	}
	found := formAction.FindStringSubmatch(page.body)
	if page.status != http.StatusOK || found == nil {
		t.Fatalf("the login page answered %d without its form", page.status)
	}
	form := url.Values{"username": {user}, "password": {password}, "credentialId": {""}}
	answer := do(t, browser, http.MethodPost, html.UnescapeString(found[1]), form)
	callback, err := url.Parse(answer.location)
	if answer.status != http.StatusFound || err != nil {
		t.Fatalf("the login form answered %d, want a redirect to the callback", answer.status)
	}
	return callback
}

type page struct {
	status         int
	body, location string
}

func do(t *testing.T, browser *http.Client, method, address string, form url.Values) page {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, address, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := browser.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body strings.Builder
	_, _ = io.Copy(&body, response.Body)
	return page{status: response.StatusCode, body: body.String(), location: response.Header.Get("Location")}
}
