// Package standin is the key side of an OpenID provider for tests: ES256 keys made in the process, the key set served
// at a profile's path, and tokens signed with those keys or with a caller's own. A test rotates the keys, makes key
// fetches fail, wait or answer other bytes, and counts the fetches. Every token's protected header is the one its
// caller states; nothing adds a type or a key the caller did not name, apart from the stand-in's own key ID.
package standin

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// The audience and subject of Claims by default; they name no product. A product that needs its own keeps its own.
const (
	Audience = "https://api.example"
	Subject  = "user-1"
)

// StandIn is one stand-in's keys and key set.
type StandIn struct {
	// Profile is the profile the stand-in was made with, "keycloak" or "auth0"; the stand-in does not read it back.
	Profile string
	// Issuer is the issuer the stand-in was made for, the "iss" of Claims.
	Issuer string
	// TrustPEM is the certificate a client trusts to reach a stand-in made by Start; empty with New, whose caller owns
	// the server and its certificate.
	TrustPEM string

	mu        sync.Mutex
	signers   map[string]*ecdsa.PrivateKey // keys by ID
	current   string                       // the ID of the key tokens are signed with
	fetches   int
	failing   bool
	held      chan struct{} // while set, key fetches wait until it is closed
	published []byte        // when set, the key set's answer instead of the keys
}

// New makes a stand-in of profile ("keycloak" or "auth0") for issuer with one key and serves its key set on mux at
// the profile's path under the issuer's path: "<path>/protocol/openid-connect/certs" for keycloak, whose issuer does
// not end in a slash, and "<path>.well-known/jwks.json" for auth0, whose issuer does. It refuses an unknown profile;
// an issuer that is not https with a host name, that carries a user, a query or a fragment (even an empty "?" or "#"),
// or an escaped path segment (the mux would serve the decoded path); and an issuer in the other profile's form. It
// starts no server.
func New(profile, issuer string, mux *http.ServeMux) (*StandIn, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		strings.Contains(issuer, "#") || u.RawPath != "" {
		return nil, fmt.Errorf("stand-in issuer %q: want https with a host name and no user, query, fragment or escaped path", issuer)
	}
	form, known := profiles[profile]
	switch {
	case !known:
		return nil, fmt.Errorf("stand-in profile %q: want keycloak or auth0", profile)
	case strings.HasSuffix(issuer, "/") != form.slash:
		return nil, fmt.Errorf("stand-in issuer %q: a %s issuer %s", issuer, profile, form.slashRule)
	}
	s := &StandIn{Profile: profile, Issuer: issuer, signers: map[string]*ecdsa.PrivateKey{}}
	s.Rotate()
	mux.HandleFunc(u.Path+form.keySet, s.keys)
	return s, nil
}

// profiles is each profile's issuer form: whether it ends in a slash, the key set's path after the issuer's, and the
// issuer path Start serves it under.
var profiles = map[string]struct {
	slash          bool
	slashRule      string
	keySet, issuer string
}{
	"keycloak": {false, "does not end in a slash", "/protocol/openid-connect/certs", "/realms/test"},
	"auth0":    {true, "ends in a slash", ".well-known/jwks.json", "/"},
}

// Start runs a stand-in of profile on a TLS test server on a loopback port until the test ends: issuer
// "<server>/realms/test" for keycloak, "<server>/" for auth0, with TrustPEM set.
func Start(t testing.TB, profile string) *StandIn {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	s, err := New(profile, server.URL+profiles[profile].issuer, mux)
	if err != nil {
		t.Fatal(err)
	}
	s.TrustPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	return s
}

// Rotate publishes a new P-256 key and signs the following tokens with it; earlier keys stay published. It returns
// the new key's ID.
func (s *StandIn) Rotate() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err) // crypto/rand's reader does not fail
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = rand.Text()
	s.signers[s.current] = key
	return s.current
}

// Fail makes every key fetch answer 503 while on is true; it wins over Answer.
func (s *StandIn) Fail(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = on
}

// Answer replaces the key set's answer with a copy of body, whatever it is (an empty, non-nil body answers nothing),
// until Answer(nil).
func (s *StandIn) Answer(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published = bytes.Clone(body)
}

// Hold makes key fetches wait until release is called. A held fetch answers with the state it finds when it resumes.
// The test's cleanup releases too, so a failed test cannot hang a server whose close was registered before; a second
// Hold while one is active fails the test.
func (s *StandIn) Hold(t testing.TB) (release func()) {
	t.Helper()
	held := make(chan struct{})
	s.mu.Lock()
	if s.held != nil {
		s.mu.Unlock()
		t.Fatalf("stand-in: Hold while an earlier hold is active; release it first")
		return func() {}
	}
	s.held = held
	s.mu.Unlock()
	release = sync.OnceFunc(func() {
		s.mu.Lock()
		s.held = nil
		s.mu.Unlock()
		close(held)
	})
	t.Cleanup(release)
	return release
}

// Fetches counts the key set requests the stand-in received, a held one from its arrival.
func (s *StandIn) Fetches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

// Claims is the stand-in's default claims (issuer, Audience, Subject, iat now and exp in five minutes) with
// overrides on top; a nil override removes the claim.
func (s *StandIn) Claims(overrides map[string]any) map[string]any {
	now := time.Now()
	claims := map[string]any{"iss": s.Issuer, "aud": Audience, "sub": Subject, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	for name, value := range overrides {
		if value == nil {
			delete(claims, name)
			continue
		}
		claims[name] = value
	}
	return claims
}

// Mint returns payload, byte for byte, as a compact token signed with the current key under ES256. Its protected
// header is header's members plus "alg" and the key's "kid", which replace members of those names; header itself is
// not changed.
func (s *StandIn) Mint(header map[string]any, payload []byte) (string, error) {
	s.mu.Lock()
	id, key := s.current, s.signers[s.current]
	s.mu.Unlock()
	members := make(map[string]any, len(header)+1)
	maps.Copy(members, header)
	members["kid"] = id
	return SignPayload(key, string(jose.ES256), members, payload)
}

// Token returns Claims(overrides) minted with header; an error fails the test.
func (s *StandIn) Token(t testing.TB, header, overrides map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(s.Claims(overrides))
	if err != nil {
		t.Fatal(err)
	}
	return s.TokenOf(t, header, payload)
}

// TokenOf returns payload minted with header; an error fails the test.
func (s *StandIn) TokenOf(t testing.TB, header map[string]any, payload []byte) string {
	t.Helper()
	token, err := s.Mint(header, payload)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Signed reports whether raw is a compact ES256 token that one of the stand-in's keys signed.
func (s *StandIn) Signed(raw string) bool {
	parsed, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.signers {
		if _, err := parsed.Verify(&key.PublicKey); err == nil {
			return true
		}
	}
	return false
}

// Sign returns claims as a compact token signed with key under algorithm (a JOSE name such as "ES256"), with
// header as its protected header plus "alg".
func Sign(key crypto.Signer, algorithm string, header, claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return SignPayload(key, algorithm, header, payload)
}

// SignPayload returns payload, byte for byte, as a compact token signed with key under algorithm, with header as its
// protected header plus "alg", which replaces a member of that name; any JSON value is kept, null included, and header
// itself is not changed. An algorithm the key cannot sign under is an error.
func SignPayload(key crypto.Signer, algorithm string, header map[string]any, payload []byte) (string, error) {
	members := make(map[jose.HeaderKey]any, len(header))
	for name, value := range header {
		if name != "alg" {
			members[jose.HeaderKey(name)] = value
		}
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.SignatureAlgorithm(algorithm), Key: key},
		&jose.SignerOptions{ExtraHeaders: members})
	if err != nil {
		return "", err
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return signed.CompactSerialize()
}

func (s *StandIn) keys(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.fetches++
	held := s.held
	s.mu.Unlock()
	if held != nil {
		<-held
	}
	s.mu.Lock()
	failing, answer := s.failing, s.published
	if !failing && answer == nil {
		answer = s.keySet()
	}
	s.mu.Unlock()
	if failing {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(answer)
}

// keySet is the published keys as a JWKS; s.mu is held.
func (s *StandIn) keySet() []byte {
	set := jose.JSONWebKeySet{}
	for id, key := range s.signers {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &key.PublicKey, KeyID: id, Algorithm: string(jose.ES256), Use: "sig"})
	}
	answer, err := json.Marshal(set)
	if err != nil {
		panic(err) // public P-256 keys always marshal
	}
	return answer
}
