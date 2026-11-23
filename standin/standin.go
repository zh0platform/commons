// Package standin is an OpenID provider's key side in the test process: HTTPS on a loopback port, the key set at both
// profiles' paths, ES256 access tokens under P-256 keys made in the process. A test rotates keys, makes
// key fetches fail or wait, replaces the answer, and counts the fetches. Wrong signatures, algorithms and weak keys
// are built by the verifier tests themselves (Sign, SignPayload).
package standin

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// The audience and subject of the stand-in's tokens by default.
const (
	Audience = "https://agent.example"
	Subject  = "user-1"
)

// StandIn is one running stand-in.
type StandIn struct {
	Issuer   string
	trustPEM string // the server certificate clients trust
	profile  string

	mu        sync.Mutex
	signers   map[string]*ecdsa.PrivateKey // keys by ID
	current   string                       // the key ID tokens are signed with
	fetches   int
	failing   bool
	held      chan struct{} // while set, fetches wait until it is closed
	published []byte        // when set, the key set answer instead of the keys
}

// Start runs a stand-in of profile ("keycloak" or "auth0") with one key until the test ends.
func Start(t testing.TB, profile string) *StandIn {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	s := &StandIn{Issuer: server.URL + "/realms/agent", profile: profile, signers: map[string]*ecdsa.PrivateKey{},
		trustPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
	path := "/realms/agent/protocol/openid-connect/certs"
	if profile == "auth0" {
		s.Issuer, path = server.URL+"/", "/.well-known/jwks.json"
	}
	mux.HandleFunc(path, s.keys)
	s.Rotate(t)
	return s
}

// Environ is the provider settings for this stand-in, with its certificate as OIDC_TRUST_CA (ENVIRONMENT test).
func (s *StandIn) Environ() []string {
	return []string{"IDENTITY_PROVIDER=" + s.profile, "OIDC_ISSUER=" + s.Issuer, "OIDC_AUDIENCE=" + Audience,
		"OIDC_TRUST_CA=" + s.trustPEM, "ENVIRONMENT=test"}
}

// Rotate publishes a new key and signs the following tokens with it; earlier keys stay published. It
// returns the new key's ID.
func (s *StandIn) Rotate(t testing.TB) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = rand.Text()
	s.signers[s.current] = key
	return s.current
}

// Fail makes every key fetch answer 503 while on is true.
func (s *StandIn) Fail(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = on
}

// Answer replaces the key set answer with body (an empty set, weak keys, malformed JSON) until Answer(nil).
func (s *StandIn) Answer(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published = body
}

// Hold makes key fetches wait until release is called; the test's cleanup releases them too, so a failed test cannot
// hang the server's close.
func (s *StandIn) Hold(t *testing.T) (release func()) {
	t.Helper()
	held := make(chan struct{})
	s.mu.Lock()
	s.held = held
	s.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			s.mu.Lock()
			s.held = nil
			s.mu.Unlock()
			close(held)
		})
	}
	t.Cleanup(release)
	return release
}

// Fetches counts the key set requests the stand-in received.
func (s *StandIn) Fetches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetches
}

// Claims is the stand-in's default claims (issuer, audience, subject, iat and a five-minute exp from now) with
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

// Token returns Claims(overrides) as an access token signed with the current key, typ at+jwt.
func (s *StandIn) Token(t testing.TB, overrides map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(s.Claims(overrides))
	if err != nil {
		t.Fatal(err)
	}
	return s.TokenOf(t, payload)
}

// TokenOf returns payload, byte for byte, as an access token signed with the current key, typ at+jwt.
func (s *StandIn) TokenOf(t testing.TB, payload []byte) string {
	t.Helper()
	s.mu.Lock()
	id, key := s.current, s.signers[s.current]
	s.mu.Unlock()
	token, err := SignPayload(key, jose.ES256, map[jose.HeaderKey]any{"kid": id, "typ": "at+jwt"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Signed reports whether raw is a compact JWS that one of the stand-in's keys signed.
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

// Sign returns claims as a compact JWS signed with key under algorithm, naming the key ID, typ at+jwt.
func Sign(key crypto.Signer, algorithm jose.SignatureAlgorithm, id string, claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return SignPayload(key, algorithm, map[jose.HeaderKey]any{"kid": id, "typ": "at+jwt"}, payload)
}

// SignPayload returns payload as a compact JWS signed with key under algorithm, with the given protected header
// members besides the algorithm.
func SignPayload(key crypto.Signer, algorithm jose.SignatureAlgorithm, header map[jose.HeaderKey]any, payload []byte) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: algorithm, Key: key}, &jose.SignerOptions{ExtraHeaders: header})
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
	held, failing, published := s.held, s.failing, s.published
	answer := s.answer()
	s.mu.Unlock()
	if held != nil {
		<-held
	}
	if failing {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if published != nil {
		answer = published
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(answer)
}

// answer is the published set; s.mu is held.
func (s *StandIn) answer() []byte {
	set := jose.JSONWebKeySet{}
	for id, key := range s.signers {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &key.PublicKey, KeyID: id, Algorithm: "ES256", Use: "sig"})
	}
	answer, err := json.Marshal(set)
	if err != nil {
		panic(err) // public P-256 keys always marshal
	}
	return answer
}
