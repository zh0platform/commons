package standin_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"git.kloudbox.de/commons/commons/standin"
)

// keySetPath is where each profile serves its key set below the issuer.
var keySetPath = map[string]string{"keycloak": "/protocol/openid-connect/certs", "auth0": ".well-known/jwks.json"}

// get GETs the stand-in's key set the way a verifier would, trusting only its certificate.
func get(s *standin.StandIn, profile string) (int, []byte, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(s.TrustPEM)) {
		return 0, nil, errors.New("TrustPEM holds no certificate")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	response, err := client.Get(s.Issuer + keySetPath[profile])
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, body, err
}

func fetch(t *testing.T, s *standin.StandIn, profile string) (int, []byte) {
	t.Helper()
	status, body, err := get(s, profile)
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

// verified returns the claims of token if a key of the fetched set signed it under ES256, naming the token's kid.
func verified(t *testing.T, set []byte, token string) (map[string]any, bool) {
	t.Helper()
	var keys jose.JSONWebKeySet
	if err := json.Unmarshal(set, &keys); err != nil {
		t.Fatal(err)
	}
	parsed, err := jose.ParseSignedCompact(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys.Key(parsed.Signatures[0].Header.KeyID) {
		if payload, err := parsed.Verify(key.Key); err == nil {
			var claims map[string]any
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			return claims, true
		}
	}
	return nil, false
}

// protectedHeader decodes a compact token's protected header.
func protectedHeader(t *testing.T, token string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatal(err)
	}
	return header
}

// An access-token user's parameters: per profile, typed tokens with its own claims verify against the served set,
// across a rotation, and an outage is answered and counted.
func TestStandInServesAnAccessTokenUser(t *testing.T) {
	for _, profile := range []string{"keycloak", "auth0"} {
		t.Run(profile, func(t *testing.T) {
			s := standin.Start(t, profile)
			typed := map[string]any{"typ": "at+jwt"}
			first := s.Token(t, typed, map[string]any{"tenant": "t-1", "exp": nil})
			if status, set := fetch(t, s, profile); status != http.StatusOK {
				t.Fatalf("key set = %d, want 200", status)
			} else if claims, ok := verified(t, set, first); !ok || claims["tenant"] != "t-1" || claims["iss"] != s.Issuer ||
				claims["aud"] != standin.Audience || claims["sub"] != standin.Subject || claims["exp"] != nil {
				t.Fatalf("verified(first) = %v, %v, want the overrides on the defaults", claims, ok)
			}
			s.Rotate()
			second := s.Token(t, typed, nil)
			_, set := fetch(t, s, profile)
			if _, ok := verified(t, set, second); !ok {
				t.Fatal("a token of the rotated key does not verify against the set fetched after the rotation")
			}
			if _, ok := verified(t, set, first); !ok || !s.Signed(first) {
				t.Fatal("a token of the earlier key no longer verifies after a rotation")
			}
			s.Fail(true)
			if status, _ := fetch(t, s, profile); status != http.StatusServiceUnavailable || s.Fetches() != 3 {
				t.Fatalf("failing key set = %d after %d fetches, want 503 after 3", status, s.Fetches())
			}
		})
	}
}

// An ID-token user's parameters: a stand-in on the caller's own mux and TLS server under another realm, minting
// untyped and logout tokens.
func TestStandInServesOnTheCallersMux(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	s, err := standin.New("keycloak", server.URL+"/realms/other", mux)
	if err != nil {
		t.Fatal(err)
	}
	if s.Issuer != server.URL+"/realms/other" || s.TrustPEM != "" {
		t.Fatalf("New = Issuer %q, TrustPEM %q, want the caller's issuer and no certificate", s.Issuer, s.TrustPEM)
	}
	response, err := server.Client().Get(s.Issuer + keySetPath["keycloak"])
	if err != nil {
		t.Fatal(err)
	}
	set, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	for _, typ := range []any{nil, "logout+jwt"} {
		header := map[string]any{}
		if typ != nil {
			header["typ"] = typ
		}
		token, err := s.Mint(header, []byte(`{"sub":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := verified(t, set, token); !ok || protectedHeader(t, token)["typ"] != typ {
			t.Fatalf("Mint(typ %v) = %v, verified %v", typ, protectedHeader(t, token), ok)
		}
	}
}

// A token's protected header is exactly what its caller stated, plus alg and, for the stand-in's key, its kid, each
// replacing a member of that name; the caller's map is not changed.
func TestTokenHeaderIsTheCallersHeader(t *testing.T) {
	s := standin.Start(t, "keycloak")
	own, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	current := s.Rotate()
	cases := map[string]struct {
		header, want map[string]any
		stand        bool // minted with the stand-in's key
	}{
		"no members":         {map[string]any{}, map[string]any{"alg": "ES256"}, false},
		"kid and typ":        {map[string]any{"kid": "k", "typ": "JWT"}, map[string]any{"alg": "ES256", "kid": "k", "typ": "JWT"}, false},
		"crit and b64":       {map[string]any{"crit": []any{"b64"}, "b64": true}, map[string]any{"alg": "ES256", "crit": []any{"b64"}, "b64": true}, false},
		"a null jwk":         {map[string]any{"jwk": nil}, map[string]any{"alg": "ES256", "jwk": nil}, false},
		"a caller's alg":     {map[string]any{"alg": "none"}, map[string]any{"alg": "ES256"}, false},
		"stand-in, typed":    {map[string]any{"typ": "at+jwt"}, map[string]any{"alg": "ES256", "kid": current, "typ": "at+jwt"}, true},
		"stand-in, alg, kid": {map[string]any{"alg": "none", "kid": "other"}, map[string]any{"alg": "ES256", "kid": current}, true},
		"stand-in, untyped":  {map[string]any{}, map[string]any{"alg": "ES256", "kid": current}, true},
	}
	for name, c := range cases {
		before := maps.Clone(c.header)
		var token string
		if c.stand {
			token, err = s.Mint(c.header, []byte(`{}`))
		} else {
			token, err = standin.SignPayload(own, "ES256", c.header, []byte(`{}`))
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := protectedHeader(t, token); !reflect.DeepEqual(got, c.want) || !reflect.DeepEqual(c.header, before) {
			t.Errorf("%s: header = %v, caller's map %v, want %v and the map unchanged", name, got, c.header, c.want)
		}
	}
	if _, err := standin.Sign(own, "RS256", map[string]any{}, map[string]any{}); err == nil {
		t.Error("Sign(P-256 key, RS256) = nil error, want refused")
	}
}

// A held fetch waits for its release and answers with the state it finds then.
func TestHeldFetchAnswersTheStateAtItsRelease(t *testing.T) {
	cases := map[string]struct {
		during func(s *standin.StandIn) string // changes the state while held; returns a key ID the answer must hold
		status int
	}{
		"failing":  {func(s *standin.StandIn) string { s.Fail(true); return "" }, http.StatusServiceUnavailable},
		"rotated":  {func(s *standin.StandIn) string { return s.Rotate() }, http.StatusOK},
		"replaced": {func(s *standin.StandIn) string { s.Answer([]byte{}); return "" }, http.StatusOK},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := standin.Start(t, "auth0")
			release := s.Hold(t)
			type answer struct {
				status int
				body   []byte
				err    error
			}
			answered := make(chan answer, 1)
			go func() {
				status, body, err := get(s, "auth0")
				answered <- answer{status, body, err}
			}()
			for deadline := time.Now().Add(5 * time.Second); s.Fetches() == 0; runtime.Gosched() {
				if time.Now().After(deadline) {
					t.Fatal("the fetch never arrived")
				}
			}
			select {
			case <-answered:
				t.Fatal("a held fetch answered before its release")
			default:
			}
			id := c.during(s)
			release()
			release()
			got := <-answered
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.status != c.status || (name == "replaced" && len(got.body) != 0) || !strings.Contains(string(got.body), id) {
				t.Fatalf("held fetch = %d %q, want %d holding %q", got.status, got.body, c.status, id)
			}
		})
	}
}

// failure records a test's failure instead of failing it.
type failure struct {
	testing.TB
	failed bool
}

func (f *failure) Fatalf(string, ...any) { f.failed = true }

// A second Hold while one is active fails the test; the first hold stays.
func TestSecondHoldFailsTheTest(t *testing.T) {
	s := standin.Start(t, "keycloak")
	release := s.Hold(t)
	second := &failure{TB: t}
	s.Hold(second)
	if !second.failed {
		t.Fatal("a second active Hold did not fail the test")
	}
	release()
	s.Hold(t)()
}

// New refuses a profile it does not know and an issuer it cannot serve the profile's key set under.
func TestNewRefusesWhatItCannotServe(t *testing.T) {
	cases := map[string][2]string{
		"unknown profile":       {"okta", "https://idp.example/"},
		"plain http":            {"keycloak", "http://idp.example/realms/x"},
		"no host":               {"keycloak", "https:opaque"},
		"a user":                {"keycloak", "https://u@idp.example/realms/x"},
		"a query":               {"keycloak", "https://idp.example/realms/x?q=1"},
		"a fragment":            {"auth0", "https://idp.example/#f"},
		"keycloak with a slash": {"keycloak", "https://idp.example/realms/x/"},
		"auth0 without a slash": {"auth0", "https://idp.example"},
	}
	for name, c := range cases {
		if _, err := standin.New(c[0], c[1], http.NewServeMux()); err == nil {
			t.Errorf("New(%s) = nil error, want refused", name)
		}
	}
}
