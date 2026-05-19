package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Test fixture: signs JWTs with a fresh RSA key and serves the matching
// JWKS so the delegate's verification path runs end-to-end. The actor
// helper builds a canonical pod-stable service-exchange JWT shape
// matching nelsong6/auth's mintAuthJwt output.

type signer struct {
	key *rsa.PrivateKey
	kid string
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{key: key, kid: "test-kid-1"}
}

func (s *signer) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = s.kid
	str, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	return str
}

func (s *signer) jwksJSON() string {
	pub := s.key.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	e := base64.RawURLEncoding.EncodeToString(eBytes)
	return fmt.Sprintf(`{"keys":[{"kid":%q,"kty":"RSA","n":%q,"e":%q}]}`, s.kid, n, e)
}

func (s *signer) jwksServer(t *testing.T) *httptest.Server {
	t.Helper()
	body := s.jwksJSON()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
}

func canonicalServiceClaims(actor string) jwt.MapClaims {
	now := time.Now().Unix()
	return jwt.MapClaims{
		"iss":         "https://auth.romaine.life",
		"sub":         "svc:tank-operator:orchestrator",
		"email":       actor,
		"actor_email": actor,
		"name":        "Service: tank-operator pod-orchestrator",
		"role":        "service",
		"iat":         now,
		"exp":         now + 900, // 15 min, matches Better Auth default
	}
}

func requestWithBearer(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// ─── happy path ─────────────────────────────────────────────────────────

func TestServiceJWT_HappyPath_AcceptsCanonicalOrchestratorJWT(t *testing.T) {
	s := newSigner(t)
	jwks := s.jwksServer(t)
	t.Cleanup(jwks.Close)

	d := NewServiceJWTDelegate(jwks.URL, "https://auth.romaine.life",
		[]string{"service.tank-operator.romaine.life"})

	token := s.sign(t, canonicalServiceClaims("pod-orchestrator@service.tank-operator.romaine.life"))
	user, err := d.Resolve(context.Background(), requestWithBearer(token))
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if user.Role != "service" {
		t.Errorf("user.Role = %q, want service", user.Role)
	}
	if user.Email != "pod-orchestrator@service.tank-operator.romaine.life" {
		t.Errorf("user.Email = %q, want pod-orchestrator@service.tank-operator.romaine.life", user.Email)
	}
}

func TestServiceJWT_RedirectOnUnauthedIsFalse(t *testing.T) {
	d := NewServiceJWTDelegate("", "", []string{"x.example"})
	if d.RedirectOnUnauthed() {
		t.Error("service-jwt mode must not redirect on unauthed")
	}
}

// ─── rejection cases ────────────────────────────────────────────────────

func TestServiceJWT_RejectsMissingAuthorization(t *testing.T) {
	s := newSigner(t)
	jwks := s.jwksServer(t)
	t.Cleanup(jwks.Close)
	d := NewServiceJWTDelegate(jwks.URL, "https://auth.romaine.life", []string{"service.tank-operator.romaine.life"})

	r := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	_, err := d.Resolve(context.Background(), r)
	if err == nil {
		t.Fatal("expected error for missing Authorization")
	}
	ae := asAuthError(err)
	if ae.Status != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", ae.Status)
	}
}

func TestServiceJWT_RejectsNonBearerAuthorization(t *testing.T) {
	d := NewServiceJWTDelegate("", "", []string{"x.example"})
	r := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwdw==")
	_, err := d.Resolve(context.Background(), r)
	if err == nil {
		t.Fatal("expected error for non-Bearer scheme")
	}
	ae := asAuthError(err)
	if ae.Status != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", ae.Status)
	}
}

func TestServiceJWT_RejectsWrongIssuer(t *testing.T) {
	s := newSigner(t)
	jwks := s.jwksServer(t)
	t.Cleanup(jwks.Close)
	d := NewServiceJWTDelegate(jwks.URL, "https://auth.romaine.life", []string{"service.tank-operator.romaine.life"})

	claims := canonicalServiceClaims("pod-orchestrator@service.tank-operator.romaine.life")
	claims["iss"] = "https://evil.example.com"
	token := s.sign(t, claims)

	_, err := d.Resolve(context.Background(), requestWithBearer(token))
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
	ae := asAuthError(err)
	if ae.Status != http.StatusUnauthorized || !strings.Contains(ae.Message, "unexpected issuer") {
		t.Errorf("unexpected error: %+v", ae)
	}
}

func TestServiceJWT_RejectsNonServiceRole(t *testing.T) {
	s := newSigner(t)
	jwks := s.jwksServer(t)
	t.Cleanup(jwks.Close)
	d := NewServiceJWTDelegate(jwks.URL, "https://auth.romaine.life", []string{"service.tank-operator.romaine.life"})

	for _, role := range []string{"admin", "user", "pending", ""} {
		t.Run("role="+role, func(t *testing.T) {
			claims := canonicalServiceClaims("nelson@romaine.life")
			claims["role"] = role
			claims["actor_email"] = "nelson@romaine.life"
			token := s.sign(t, claims)
			_, err := d.Resolve(context.Background(), requestWithBearer(token))
			if err == nil {
				t.Fatalf("expected rejection for role=%q", role)
			}
			ae := asAuthError(err)
			if ae.Status != http.StatusForbidden {
				t.Errorf("status=%d, want 403 (role gate)", ae.Status)
			}
		})
	}
}

func TestServiceJWT_RejectsActorEmailDomainOutsideAllowlist(t *testing.T) {
	// This is the load-bearing test: nelsong6/auth issues role=service
	// JWTs to multiple consumers (sessions, mcp-* shared servers,
	// hermes itself, the orchestrator). Hermes' API must only accept
	// the orchestrator's JWT; the rest must 403 even though they're
	// otherwise legitimate.
	s := newSigner(t)
	jwks := s.jwksServer(t)
	t.Cleanup(jwks.Close)
	d := NewServiceJWTDelegate(jwks.URL, "https://auth.romaine.life",
		[]string{"service.tank-operator.romaine.life"})

	otherActors := []string{
		"pod-42@service.tank.romaine.life",                // a session JWT
		"pod-mcp-k8s@service.mcp-k8s.romaine.life",        // mcp-k8s
		"pod-hermes@service.hermes.romaine.life",          // hermes-self
		"pod-mcp-argocd@service.mcp-argocd.romaine.life",  // mcp-argocd
		"alice@romaine.life",                              // somehow a human
	}
	for _, actor := range otherActors {
		t.Run(actor, func(t *testing.T) {
			claims := canonicalServiceClaims(actor)
			token := s.sign(t, claims)
			_, err := d.Resolve(context.Background(), requestWithBearer(token))
			if err == nil {
				t.Fatalf("expected rejection for actor=%q", actor)
			}
			ae := asAuthError(err)
			if ae.Status != http.StatusForbidden {
				t.Errorf("status=%d, want 403; msg=%q", ae.Status, ae.Message)
			}
		})
	}
}

func TestServiceJWT_RejectsExpired(t *testing.T) {
	s := newSigner(t)
	jwks := s.jwksServer(t)
	t.Cleanup(jwks.Close)
	d := NewServiceJWTDelegate(jwks.URL, "https://auth.romaine.life", []string{"service.tank-operator.romaine.life"})

	claims := canonicalServiceClaims("pod-orchestrator@service.tank-operator.romaine.life")
	claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(-1 * time.Hour).Unix() // well past leeway
	token := s.sign(t, claims)

	_, err := d.Resolve(context.Background(), requestWithBearer(token))
	if err == nil {
		t.Fatal("expected expired-token rejection")
	}
	ae := asAuthError(err)
	if ae.Status != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", ae.Status)
	}
}

func TestServiceJWT_RejectsWrongSignature(t *testing.T) {
	signA := newSigner(t)
	signB := newSigner(t) // different key
	jwks := signA.jwksServer(t)
	t.Cleanup(jwks.Close)
	d := NewServiceJWTDelegate(jwks.URL, "https://auth.romaine.life", []string{"service.tank-operator.romaine.life"})

	// Sign with B's key but publish A's JWKS — verification must fail.
	token := signB.sign(t, canonicalServiceClaims("pod-orchestrator@service.tank-operator.romaine.life"))
	_, err := d.Resolve(context.Background(), requestWithBearer(token))
	if err == nil {
		t.Fatal("expected signature rejection")
	}
	ae := asAuthError(err)
	if ae.Status != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", ae.Status)
	}
}

func TestServiceJWT_RejectsMissingKid(t *testing.T) {
	s := newSigner(t)
	jwks := s.jwksServer(t)
	t.Cleanup(jwks.Close)
	d := NewServiceJWTDelegate(jwks.URL, "https://auth.romaine.life", []string{"service.tank-operator.romaine.life"})

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, canonicalServiceClaims("pod-orchestrator@service.tank-operator.romaine.life"))
	// Deliberately do NOT set kid.
	str, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Resolve(context.Background(), requestWithBearer(str))
	if err == nil {
		t.Fatal("expected missing-kid rejection")
	}
	ae := asAuthError(err)
	if ae.Status != http.StatusUnauthorized || !strings.Contains(ae.Message, "kid") {
		t.Errorf("unexpected error: %+v", ae)
	}
}

// ─── load-config coverage ───────────────────────────────────────────────

func TestLoadConfig_ServiceJWTMode_RequiresAllowedActorDomains(t *testing.T) {
	get := func(key string) string {
		switch key {
		case "UPSTREAM_URL":
			return "http://127.0.0.1:8642"
		case "AUTH_MODE":
			return "service-jwt"
		default:
			return ""
		}
	}
	_, err := loadConfig(get)
	if err == nil {
		t.Fatal("expected error: ALLOWED_ACTOR_EMAIL_DOMAINS is required")
	}
	if !strings.Contains(err.Error(), "ALLOWED_ACTOR_EMAIL_DOMAINS") {
		t.Errorf("error mentions wrong thing: %v", err)
	}
}

func TestLoadConfig_ServiceJWTMode_ParsesAllowedDomains(t *testing.T) {
	get := func(key string) string {
		switch key {
		case "UPSTREAM_URL":
			return "http://127.0.0.1:8642"
		case "AUTH_MODE":
			return "service-jwt"
		case "ALLOWED_ACTOR_EMAIL_DOMAINS":
			return "service.tank-operator.romaine.life, service.other.example "
		default:
			return ""
		}
	}
	cfg, err := loadConfig(get)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthMode != "service-jwt" {
		t.Errorf("AuthMode = %q, want service-jwt", cfg.AuthMode)
	}
	if len(cfg.AllowedActorEmailDomains) != 2 {
		t.Fatalf("AllowedActorEmailDomains = %v, want 2 entries", cfg.AllowedActorEmailDomains)
	}
	if cfg.AllowedActorEmailDomains[0] != "service.tank-operator.romaine.life" {
		t.Errorf("first entry = %q, want service.tank-operator.romaine.life (whitespace must be trimmed)",
			cfg.AllowedActorEmailDomains[0])
	}
}

func TestLoadConfig_RejectsUnknownAuthMode(t *testing.T) {
	get := func(key string) string {
		switch key {
		case "UPSTREAM_URL":
			return "http://127.0.0.1:8642"
		case "AUTH_MODE":
			return "magic-mode"
		default:
			return ""
		}
	}
	_, err := loadConfig(get)
	if err == nil || !strings.Contains(err.Error(), "AUTH_MODE") {
		t.Fatalf("expected error mentioning AUTH_MODE, got: %v", err)
	}
}

// Silence the unused-import warning if json/base64 aren't pulled in by
// the test cases as the file shape evolves.
var _ = json.Marshal
