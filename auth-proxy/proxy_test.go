package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type stubResolver struct {
	user User
	err  error
}

func (s *stubResolver) Resolve(_ context.Context, _ string) (User, error) {
	return s.user, s.err
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

func TestProxy_HealthBypassesAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be hit for /healthz")
	}))
	t.Cleanup(upstream.Close)

	p, err := NewProxy(upstream.URL, "hermes.romaine.life", "https://auth.romaine.life/signin", &stubResolver{err: AuthError{Status: 401, Message: "no cookie"}}, discardLogger{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestProxy_BrowserGetRedirectsToSignin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be hit when unauthed")
	}))
	t.Cleanup(upstream.Close)

	p, err := NewProxy(upstream.URL, "hermes.romaine.life", "https://auth.romaine.life/api/auth/sign-in/social/microsoft", &stubResolver{err: AuthError{Status: 401, Message: "no cookie"}}, discardLogger{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/settings?foo=bar", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://auth.romaine.life/api/auth/sign-in/social/microsoft?") {
		t.Fatalf("unexpected Location: %s", loc)
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	cb := parsed.Query().Get("callbackURL")
	want := "https://hermes.romaine.life/admin/settings?foo=bar"
	if cb != want {
		t.Fatalf("callbackURL = %q, want %q", cb, want)
	}
}

func TestProxy_ApiCallerGets401NotRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not hit upstream")
	}))
	t.Cleanup(upstream.Close)

	p, _ := NewProxy(upstream.URL, "hermes.romaine.life", "https://auth.romaine.life/signin", &stubResolver{err: AuthError{Status: 401, Message: "no cookie"}}, discardLogger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/things", nil)
	req.Header.Set("Accept", "application/json")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestProxy_ForbiddenSurfacesAs403(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not hit upstream")
	}))
	t.Cleanup(upstream.Close)
	p, _ := NewProxy(upstream.URL, "hermes.romaine.life", "https://auth.romaine.life/signin", &stubResolver{err: AuthError{Status: 403, Message: "no app grant"}}, discardLogger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestProxy_AuthenticatedRequestReachesUpstream_WithIdentityHeaders(t *testing.T) {
	var seen *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	t.Cleanup(upstream.Close)

	p, _ := NewProxy(upstream.URL, "hermes.romaine.life", "https://auth.romaine.life/signin", &stubResolver{
		user: User{Sub: "u1", Email: "nelson@example.com", Name: "N", Role: "admin"},
	}, discardLogger{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Header.Set("Cookie", "better-auth.session_token=abc; analytics=keep")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello from upstream" {
		t.Fatalf("body=%q", rec.Body.String())
	}
	if seen == nil {
		t.Fatal("upstream not hit")
	}
	if seen.Header.Get("X-Forwarded-User") != "nelson@example.com" {
		t.Fatalf("missing X-Forwarded-User: %v", seen.Header)
	}
	if seen.Header.Get("X-Forwarded-User-Role") != "admin" {
		t.Fatalf("missing role header")
	}
	// The .romaine.life cookie should have been stripped; the unrelated
	// analytics cookie should pass through.
	cookieHdr := seen.Header.Get("Cookie")
	if strings.Contains(cookieHdr, "better-auth.session_token") {
		t.Fatalf("expected better-auth cookie stripped, got %q", cookieHdr)
	}
	if !strings.Contains(cookieHdr, "analytics=keep") {
		t.Fatalf("expected analytics cookie preserved, got %q", cookieHdr)
	}
}
