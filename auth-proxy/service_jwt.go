// service_jwt.go — auth-proxy delegate that gates inbound calls on an
// auth.romaine.life-issued `role=service` JWT, with a domain-pinned
// `actor_email` allowlist.
//
// The cookie-delegate path (session.go) handles humans hitting the
// dashboard. This file handles SERVICES hitting Hermes' OpenAI-
// compatible API server. Same upstream image, different sidecar
// container, different env (`AUTH_MODE=service-jwt`). See
// nelsong6/tank-operator#540 for the cross-repo design.
//
// Verification semantics (mirrors nelsong6/tank-operator's
// internal/auth/jwks_remote.go so the two services stay in sync):
//
//   - RS256 signature against auth.romaine.life's JWKS
//     (https://auth.romaine.life/api/auth/jwks). 10-minute TTL cache,
//     kid-keyed lookup, lazy refresh on miss.
//   - iss == https://auth.romaine.life
//   - role == "service" (not "admin" / "user" / "pending")
//   - actor_email's domain matches one of the configured allowlist
//     entries (env ALLOWED_ACTOR_EMAIL_DOMAINS, comma-separated)
//
// Today's only legitimate caller is the tank-operator orchestrator,
// whose service-exchange JWT carries
// actor_email=pod-orchestrator@service.tank-operator.romaine.life
// (nelsong6/auth#42's pod-stable consumer). The
// ALLOWED_ACTOR_EMAIL_DOMAINS env is set to `service.tank-operator.romaine.life`
// so a session pod's JWT, an mcp-* shared-service JWT, or a future
// /admin/service-tokens-minted admin token cannot drive Hermes' API
// even though they all have role=service.
package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Defaults match the orchestrator's nelsong6/tank-operator pattern so
// the verification semantics are uniform across the two repos.
const (
	defaultJWKSURL     = "https://auth.romaine.life/api/auth/jwks"
	defaultIssuer      = "https://auth.romaine.life"
	jwksCacheTTL       = 10 * time.Minute
	jwksFetchTimeout   = 10 * time.Second
	jwtClockSkewLeeway = 60 * time.Second
)

// ServiceJWTDelegate implements SessionResolver for the service-JWT
// path. Construct via NewServiceJWTDelegate.
type ServiceJWTDelegate struct {
	jwksURL              string
	issuer               string
	allowedActorDomains  []string
	jwks                 *jwksCache
}

// NewServiceJWTDelegate constructs a delegate. allowedActorDomains must
// be non-empty (the proxy fails fast otherwise) — accepting any
// `role=service` JWT without an actor_email-domain pin would mean any
// service principal in the cluster could drive Hermes' API.
func NewServiceJWTDelegate(jwksURL, issuer string, allowedActorDomains []string) *ServiceJWTDelegate {
	if jwksURL == "" {
		jwksURL = defaultJWKSURL
	}
	if issuer == "" {
		issuer = defaultIssuer
	}
	// Lowercase + trim once; check is case-insensitive on the domain
	// (email local-parts are case-preserving but domains are not).
	clean := make([]string, 0, len(allowedActorDomains))
	for _, d := range allowedActorDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			clean = append(clean, d)
		}
	}
	return &ServiceJWTDelegate{
		jwksURL:             jwksURL,
		issuer:              issuer,
		allowedActorDomains: clean,
		jwks: &jwksCache{
			httpClient: &http.Client{Timeout: jwksFetchTimeout},
		},
	}
}

// RedirectOnUnauthed is false: callers are services. A 302 to a sign-in
// page would be a category error.
func (*ServiceJWTDelegate) RedirectOnUnauthed() bool { return false }

// Resolve verifies the inbound Authorization: Bearer <jwt>. On success
// returns a User populated from JWT claims (Email/Sub/Name/Role/etc.).
func (d *ServiceJWTDelegate) Resolve(ctx context.Context, r *http.Request) (User, error) {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "missing Authorization header"}
	}
	if !strings.HasPrefix(authz, "Bearer ") {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "Authorization must be Bearer <jwt>"}
	}
	rawToken := strings.TrimSpace(authz[len("Bearer "):])
	if rawToken == "" {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "empty bearer token"}
	}

	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(rawToken, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != "RS256" {
			return nil, fmt.Errorf("unexpected alg %q (only RS256 accepted)", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("JWT missing kid header")
		}
		return d.jwks.getKey(ctx, d.jwksURL, kid)
	}, jwt.WithLeeway(jwtClockSkewLeeway))
	if err != nil || !token.Valid {
		if err == nil {
			err = errors.New("invalid token")
		}
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "JWT verify failed: " + err.Error()}
	}

	// iss pin
	iss, _ := claims["iss"].(string)
	if iss != d.issuer {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "unexpected issuer: " + iss}
	}

	// role == "service"
	role, _ := claims["role"].(string)
	if role != "service" {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "JWT role must be `service`, got: " + role}
	}

	// actor_email domain pin
	actor, _ := claims["actor_email"].(string)
	actor = strings.ToLower(strings.TrimSpace(actor))
	if actor == "" {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "JWT missing actor_email claim"}
	}
	at := strings.LastIndexByte(actor, '@')
	if at < 0 {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "JWT actor_email has no domain: " + actor}
	}
	actorDomain := actor[at+1:]
	if !d.actorDomainAllowed(actorDomain) {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "JWT actor_email domain not in allowlist: " + actorDomain}
	}

	email, _ := claims["email"].(string)
	sub, _ := claims["sub"].(string)
	name, _ := claims["name"].(string)

	return User{
		Sub:   sub,
		Email: strings.ToLower(strings.TrimSpace(email)),
		Name:  name,
		Role:  role,
	}, nil
}

func (d *ServiceJWTDelegate) actorDomainAllowed(domain string) bool {
	for _, allowed := range d.allowedActorDomains {
		if allowed == domain {
			return true
		}
	}
	return false
}

// ─── JWKS cache ────────────────────────────────────────────────────────
//
// Direct port of nelsong6/tank-operator/backend-go/internal/auth/jwks_remote.go's
// jwksCache. Same TTL, same kid-keyed map, same lazy-refresh semantics.
// The two services share verification posture — drift here is a bug.

type jwksKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksResponse struct {
	Keys []jwksKey `json:"keys"`
}

type jwksCache struct {
	mu         sync.RWMutex
	keys       map[string]*rsa.PublicKey
	fetchedAt  time.Time
	httpClient *http.Client
}

func (c *jwksCache) getKey(ctx context.Context, url, kid string) (*rsa.PublicKey, error) {
	c.mu.RLock()
	if time.Since(c.fetchedAt) < jwksCacheTTL {
		if key, ok := c.keys[kid]; ok {
			c.mu.RUnlock()
			return key, nil
		}
		c.mu.RUnlock()
		// Cache is fresh but doesn't contain this kid — could be a
		// key rotation in flight. Fall through to refresh.
	} else {
		c.mu.RUnlock()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.fetchedAt) < jwksCacheTTL {
		if key, ok := c.keys[kid]; ok {
			return key, nil
		}
	}
	if err := c.refresh(ctx, url); err != nil {
		return nil, err
	}
	if key, ok := c.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("unknown kid %q after refresh", kid)
}

func (c *jwksCache) refresh(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("JWKS request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("JWKS fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("JWKS read: %w", err)
	}
	var jwks jwksResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("JWKS parse: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := rsaPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	c.keys = keys
	c.fetchedAt = time.Now()
	return nil
}

func rsaPublicKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	decode := func(s string) ([]byte, error) {
		s = strings.ReplaceAll(s, "-", "+")
		s = strings.ReplaceAll(s, "_", "/")
		switch len(s) % 4 {
		case 2:
			s += "=="
		case 3:
			s += "="
		}
		return base64.StdEncoding.DecodeString(s)
	}
	nb, err := decode(nB64)
	if err != nil {
		return nil, err
	}
	eb, err := decode(eB64)
	if err != nil {
		return nil, err
	}
	eVal := 0
	for _, b := range eb {
		eVal = eVal<<8 | int(b)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: eVal}, nil
}
