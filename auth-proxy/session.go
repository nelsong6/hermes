// Package main implements the hermes auth-proxy sidecar.
//
// session.go: cookie-delegate to auth.romaine.life. Lifted from
// nelsong6/glimmung's internal/auth/cookie_delegate.go (the existing
// in-prod pattern) and adapted for per-app gating via the `apps` JSON
// column on the auth.romaine.life user record.
//
// The proxy forwards the inbound `.romaine.life` session cookie to
// auth.romaine.life's `/api/auth/get-session`, caches the result for
// 60s per cookie value, and decides allow/deny based on:
//
//   - role == "admin"  → allow (bypass; admins have read-write
//     access to everything in the system already via the Tyrell
//     Console)
//   - apps[<requiredAppKey>].access == true → allow
//   - anything else → deny (401 if no/bad cookie, 403 if signed in
//     but lacking the app grant)
//
// The cache stores errors too — a 401 doesn't get re-pounded on every
// request — but only within the TTL window.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultSessionTTL     = 60 * time.Second
	defaultCacheMaxSize   = 200
	defaultHTTPTimeout    = 10 * time.Second
	maxSessionResponseLen = 64 * 1024
)

// User is the resolved identity. Email is lowercased + trimmed.
type User struct {
	Sub   string
	Email string
	Name  string
	Role  string
}

// AuthError carries an HTTP status so the proxy can pick redirect vs 403
// without re-interpreting an error string. Status is one of 401, 403,
// 500, 503.
type AuthError struct {
	Status  int
	Message string
}

func (e AuthError) Error() string { return e.Message }

// SessionResolver looks up a user from a cookie header.
type SessionResolver interface {
	Resolve(ctx context.Context, cookie string) (User, error)
}

// CookieDelegate calls auth.romaine.life's /api/auth/get-session and
// gates on the apps JSON column.
type CookieDelegate struct {
	endpoint       string // e.g. https://auth.romaine.life/api/auth/get-session
	requiredAppKey string // e.g. "hermes" — checks apps[<key>].access == true
	httpClient     *http.Client
	cache          *sessionCache
}

// NewCookieDelegate constructs a delegate that forwards cookies to
// `endpoint` and requires either role=admin OR apps[appKey].access=true.
// If appKey is empty, the apps-column check is skipped (admin-only).
func NewCookieDelegate(endpoint, appKey string) *CookieDelegate {
	return &CookieDelegate{
		endpoint:       endpoint,
		requiredAppKey: appKey,
		httpClient:     &http.Client{Timeout: defaultHTTPTimeout},
		cache:          newSessionCache(defaultSessionTTL, defaultCacheMaxSize),
	}
}

// Resolve takes the raw Cookie header value and returns the user or an
// AuthError. The same error gets cached as the success path so we don't
// re-pound auth.romaine.life on every unauthenticated request.
func (d *CookieDelegate) Resolve(ctx context.Context, cookie string) (User, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "no session cookie"}
	}

	if cached, ok := d.cache.get(cookie); ok {
		return cached.user, cached.err
	}

	user, err := d.fetchAndCheck(ctx, cookie)
	d.cache.put(cookie, sessionResult{user: user, err: err})
	return user, err
}

func (d *CookieDelegate) fetchAndCheck(ctx context.Context, cookie string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.endpoint, nil)
	if err != nil {
		return User{}, AuthError{Status: http.StatusInternalServerError, Message: "build session request: " + err.Error()}
	}
	req.Header.Set("Cookie", cookie)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "auth.romaine.life unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: fmt.Sprintf("auth.romaine.life returned %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSessionResponseLen))
	if err != nil {
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "session read: " + err.Error()}
	}

	// Better Auth returns `null` for unauthenticated requests.
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "null" {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "not signed in"}
	}

	// The apps column comes back as a JSON-encoded STRING (not an
	// object) because Better Auth's `additionalFields` declared it as
	// `type: "string"`. We parse it manually after extracting.
	var parsed struct {
		User *struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
			Role  string `json:"role"`
			Apps  string `json:"apps"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return User{}, AuthError{Status: http.StatusServiceUnavailable, Message: "session parse: " + err.Error()}
	}
	if parsed.User == nil {
		return User{}, AuthError{Status: http.StatusUnauthorized, Message: "not signed in"}
	}

	u := User{
		Sub:   parsed.User.ID,
		Email: strings.ToLower(strings.TrimSpace(parsed.User.Email)),
		Name:  parsed.User.Name,
		Role:  parsed.User.Role,
	}

	// Admin bypass — matches the org-wide convention from glimmung.
	if u.Role == "admin" {
		return u, nil
	}

	// Pending / unknown roles are firmly rejected — admin must promote
	// to `user` first via the Tyrell Console.
	if u.Role != "user" {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "role not approved: " + u.Role}
	}

	// `user` role still needs explicit per-app grant.
	if d.requiredAppKey == "" {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "admin required"}
	}
	if !hasAppAccess(parsed.User.Apps, d.requiredAppKey) {
		return User{}, AuthError{Status: http.StatusForbidden, Message: "user missing apps." + d.requiredAppKey + ".access grant"}
	}
	return u, nil
}

// hasAppAccess parses the stringified apps JSON and checks
// apps[key].access === true. Returns false on any parse error / missing
// key / wrong type — defaults to deny.
func hasAppAccess(appsJSON, key string) bool {
	if appsJSON == "" {
		return false
	}
	var apps map[string]json.RawMessage
	if err := json.Unmarshal([]byte(appsJSON), &apps); err != nil {
		return false
	}
	raw, ok := apps[key]
	if !ok {
		return false
	}
	var entry struct {
		Access bool `json:"access"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	return entry.Access
}

// sessionCache is a tiny TTL'd map of cookie value -> last-known
// session result. Errors are cached too so a 401 doesn't get retried
// every request inside the TTL window.
type sessionCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	entries map[string]sessionCacheEntry
}

type sessionCacheEntry struct {
	expiry time.Time
	sessionResult
}

type sessionResult struct {
	user User
	err  error
}

func newSessionCache(ttl time.Duration, maxSize int) *sessionCache {
	return &sessionCache{ttl: ttl, maxSize: maxSize, entries: make(map[string]sessionCacheEntry)}
}

func (c *sessionCache) get(key string) (sessionResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return sessionResult{}, false
	}
	if time.Now().After(entry.expiry) {
		delete(c.entries, key)
		return sessionResult{}, false
	}
	return entry.sessionResult, true
}

func (c *sessionCache) put(key string, value sessionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = sessionCacheEntry{
		expiry:        time.Now().Add(c.ttl),
		sessionResult: value,
	}
	if len(c.entries) > c.maxSize {
		now := time.Now()
		for k, e := range c.entries {
			if now.After(e.expiry) {
				delete(c.entries, k)
			}
		}
	}
}

// asAuthError returns the embedded AuthError if err is one (or wraps one);
// otherwise returns a generic 500. Lets the proxy choose redirect vs
// 403 vs 503 by inspecting Status.
func asAuthError(err error) AuthError {
	var ae AuthError
	if errors.As(err, &ae) {
		return ae
	}
	return AuthError{Status: http.StatusInternalServerError, Message: err.Error()}
}
