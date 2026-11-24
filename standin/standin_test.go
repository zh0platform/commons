package standin_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
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

// keySetPath is where each profile serves its key set below the issuer, as a verifier derives it.
var keySetPath = map[string]string{"keycloak": "/protocol/openid-connect/certs", "auth0": ".well-known/jwks.json"}

// trusting is a client that trusts only the stand-in's certificate, as a verifier would.
func trusting(t *testing.T, s *standin.StandIn) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(s.TrustPEM)) {
		t.Fatal("TrustPEM holds no certificate")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
}

// get GETs a key set.
func get(client *http.Client, url string) (int, []byte, error) {
	response, err := client.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, body, err
}

func fetch(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	status, body, err := get(client, url)
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

// verified returns the claims of token if the key of the fetched set its kid names signed it under ES256.
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
			if s.Profile != profile {
				t.Fatalf("Start(%s).Profile = %q", profile, s.Profile)
			}
			client, keys := trusting(t, s), s.Issuer+keySetPath[profile]
			typed := map[string]any{"typ": "at+jwt"}
			first := s.Token(t, typed, map[string]any{"tenant": "t-1", "exp": nil})
			if status, set := fetch(t, client, keys); status != http.StatusOK {
				t.Fatalf("key set = %d, want 200", status)
			} else if claims, ok := verified(t, set, first); !ok || claims["tenant"] != "t-1" || claims["iss"] != s.Issuer ||
				claims["aud"] != standin.Audience || claims["sub"] != standin.Subject || claims["exp"] != nil {
				t.Fatalf("verified(first) = %v, %v, want the overrides on the defaults", claims, ok)
			}
			s.Rotate()
			second := s.Token(t, typed, nil)
			_, set := fetch(t, client, keys)
			claims, ok := verified(t, set, second)
			if !ok || protectedHeader(t, second)["kid"] == protectedHeader(t, first)["kid"] {
				t.Fatal("a token of the rotated key does not verify against the set fetched after the rotation, under a new key ID")
			}
			exp, expOK := claims["exp"].(float64)
			iat, iatOK := claims["iat"].(float64)
			if !expOK || !iatOK || exp != iat+300 {
				t.Fatalf("Claims exp = %v, iat %v, want a lifetime of 300 s", claims["exp"], claims["iat"])
			}
			var served jose.JSONWebKeySet
			if err := json.Unmarshal(set, &served); err != nil || len(served.Keys) != 2 {
				t.Fatalf("key set = %s, %v, want both keys", set, err)
			}
			for _, key := range served.Keys {
				if !key.IsPublic() {
					t.Fatalf("the key set publishes a private key (%s)", key.KeyID)
				}
			}
			if _, ok := verified(t, set, first); !ok || !s.Signed(first) {
				t.Fatal("a token of the earlier key no longer verifies after a rotation")
			}
			s.Fail(true)
			if status, _ := fetch(t, client, keys); status != http.StatusServiceUnavailable || s.Fetches() != 3 {
				t.Fatalf("failing key set = %d after %d fetches, want 503 after 3", status, s.Fetches())
			}
		})
	}
}

// An ID-token user's parameters: a stand-in on the caller's own mux and TLS server under another realm, whose minted
// token verifies against the set served there.
func TestStandInServesOnTheCallersMux(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	s, err := standin.New("keycloak", server.URL+"/realms/other", mux)
	if err != nil {
		t.Fatal(err)
	}
	if s.Profile != "keycloak" || s.Issuer != server.URL+"/realms/other" || s.TrustPEM != "" {
		t.Fatalf("New = Profile %q, Issuer %q, TrustPEM %q, want keycloak, the caller's issuer and no certificate",
			s.Profile, s.Issuer, s.TrustPEM)
	}
	token, err := s.Mint(map[string]any{}, []byte(`{"sub":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, set := fetch(t, server.Client(), s.Issuer+keySetPath["keycloak"]); !s.Signed(token) {
		t.Fatal("Signed(minted token) = false")
	} else if _, ok := verified(t, set, token); !ok {
		t.Fatal("a minted token does not verify against the key set on the caller's mux")
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
	ownKey := func(header map[string]any) (string, error) {
		return standin.SignPayload(own, "ES256", header, []byte(`{}`))
	}
	standInKey := func(header map[string]any) (string, error) { return s.Mint(header, []byte(`{}`)) }
	claimsOwnKey := func(header map[string]any) (string, error) {
		return standin.Sign(own, "ES256", header, map[string]any{})
	}
	tokenWithHeader := func(header map[string]any) (string, error) { return s.Token(t, header, nil), nil }
	for name, c := range map[string]struct {
		sign         func(map[string]any) (string, error)
		header, want map[string]any
	}{
		"no members":         {ownKey, map[string]any{}, map[string]any{"alg": "ES256"}},
		"kid and typ":        {ownKey, map[string]any{"kid": "k", "typ": "JWT"}, map[string]any{"alg": "ES256", "kid": "k", "typ": "JWT"}},
		"crit and b64":       {ownKey, map[string]any{"crit": []any{"b64"}, "b64": true}, map[string]any{"alg": "ES256", "crit": []any{"b64"}, "b64": true}},
		"a null jwk":         {ownKey, map[string]any{"jwk": nil}, map[string]any{"alg": "ES256", "jwk": nil}},
		"a caller's alg":     {ownKey, map[string]any{"alg": "none"}, map[string]any{"alg": "ES256"}},
		"stand-in, typed":    {standInKey, map[string]any{"typ": "at+jwt"}, map[string]any{"alg": "ES256", "kid": current, "typ": "at+jwt"}},
		"stand-in, alg, kid": {standInKey, map[string]any{"alg": "none", "kid": "other"}, map[string]any{"alg": "ES256", "kid": current}},
		"stand-in, untyped":  {standInKey, map[string]any{}, map[string]any{"alg": "ES256", "kid": current}},
		"Sign, claims":       {claimsOwnKey, map[string]any{"kid": "k"}, map[string]any{"alg": "ES256", "kid": "k"}},
		"Token, logout":      {tokenWithHeader, map[string]any{"typ": "logout+jwt"}, map[string]any{"alg": "ES256", "kid": current, "typ": "logout+jwt"}},
	} {
		before := maps.Clone(c.header)
		token, err := c.sign(c.header)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := protectedHeader(t, token); !reflect.DeepEqual(got, c.want) || !reflect.DeepEqual(c.header, before) {
			t.Errorf("%s: header = %v, caller's map %v, want %v and the map unchanged", name, got, c.header, c.want)
		}
	}
}

// A key cannot sign under an algorithm of another key type.
func TestSignRefusesAnAlgorithmTheKeyCannotSign(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := standin.Sign(key, "RS256", map[string]any{}, map[string]any{}); err == nil {
		t.Error("Sign(P-256 key, RS256) = nil error, want refused")
	}
}

// A held fetch waits for its release and answers with the state it finds then.
func TestHeldFetchAnswersTheStateAtItsRelease(t *testing.T) {
	for name, during := range map[string]func(s *standin.StandIn) func(status int, body []byte) bool{
		"failing": func(s *standin.StandIn) func(int, []byte) bool {
			s.Fail(true)
			return func(status int, _ []byte) bool { return status == http.StatusServiceUnavailable }
		},
		"rotated": func(s *standin.StandIn) func(int, []byte) bool {
			id := s.Rotate()
			return func(status int, body []byte) bool {
				return status == http.StatusOK && strings.Contains(string(body), id)
			}
		},
		"replaced": func(s *standin.StandIn) func(int, []byte) bool {
			s.Answer([]byte{})
			return func(status int, body []byte) bool { return status == http.StatusOK && len(body) == 0 }
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := standin.Start(t, "auth0")
			client := trusting(t, s)
			release := s.Hold(t)
			type answer struct {
				status int
				body   []byte
				err    error
			}
			answered := make(chan answer, 1)
			go func() {
				status, body, err := get(client, s.Issuer+keySetPath["auth0"])
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
			want := during(s)
			release()
			release()
			got := <-answered
			if got.err != nil || !want(got.status, got.body) {
				t.Fatalf("held fetch = %d %q, %v, want the state set during the hold", got.status, got.body, got.err)
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
	for name, c := range map[string][2]string{
		"unknown profile":       {"okta", "https://idp.example/"},
		"plain http":            {"keycloak", "http://idp.example/realms/x"},
		"no host":               {"keycloak", "https:opaque"},
		"a user":                {"keycloak", "https://u@idp.example/realms/x"},
		"a query":               {"keycloak", "https://idp.example/realms/x?q=1"},
		"a fragment":            {"auth0", "https://idp.example/#f"},
		"an empty query":        {"keycloak", "https://idp.example/realms/x?"},
		"an empty fragment":     {"keycloak", "https://idp.example/realms/x#"},
		"a port without a host": {"keycloak", "https://:443/realms/x"},
		"an escaped slash":      {"keycloak", "https://idp.example/realms/a%2Fb"},
		"keycloak with a slash": {"keycloak", "https://idp.example/realms/x/"},
		"auth0 without a slash": {"auth0", "https://idp.example"},
	} {
		if _, err := standin.New(c[0], c[1], http.NewServeMux()); err == nil {
			t.Errorf("New(%s) = nil error, want refused", name)
		}
	}
}
