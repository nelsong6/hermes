package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// requestWithCookie builds a minimal http.Request with the given Cookie
// header. The SessionResolver interface takes *http.Request so we wrap
// the cookie value here rather than constructing it inline at every
// call site.
func requestWithCookie(cookie string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	return r
}

func TestHasAppAccess(t *testing.T) {
	cases := []struct {
		name string
		json string
		key  string
		want bool
	}{
		{"empty json", "", "hermes", false},
		{"empty object", "{}", "hermes", false},
		{"missing key", `{"glimmung":{"access":true}}`, "hermes", false},
		{"explicit false", `{"hermes":{"access":false}}`, "hermes", false},
		{"explicit true", `{"hermes":{"access":true}}`, "hermes", true},
		{"true with siblings", `{"glimmung":{},"hermes":{"access":true,"plan":"pro"}}`, "hermes", true},
		{"malformed json", `{not json`, "hermes", false},
		{"non-bool access", `{"hermes":{"access":"yes"}}`, "hermes", false},
		{"missing access subkey", `{"hermes":{"role":"admin"}}`, "hermes", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasAppAccess(tc.json, tc.key)
			if got != tc.want {
				t.Fatalf("hasAppAccess(%q, %q) = %v, want %v", tc.json, tc.key, got, tc.want)
			}
		})
	}
}

func TestCookieDelegate_AdminBypassesAppCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "" {
			t.Fatal("expected cookie to be forwarded")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"a@b","name":"A","role":"admin","apps":"{}"}}`))
	}))
	t.Cleanup(srv.Close)

	d := NewCookieDelegate(srv.URL, "hermes")
	u, err := d.Resolve(context.Background(), requestWithCookie("x=1"))
	if err != nil {
		t.Fatalf("admin should pass without app grant: %v", err)
	}
	if u.Role != "admin" || u.Email != "a@b" {
		t.Fatalf("unexpected user: %+v", u)
	}
}

func TestCookieDelegate_UserNeedsAppGrant(t *testing.T) {
	cases := []struct {
		name string
		apps string
		ok   bool
	}{
		{"no grant", `{}`, false},
		{"grant present", `{"hermes":{"access":true}}`, true},
		{"grant for other app", `{"glimmung":{"access":true}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Apps is itself a JSON string (Better Auth additionalFields type=string).
				body := `{"user":{"id":"u1","email":"a@b","name":"A","role":"user","apps":` + jsonStringLiteral(tc.apps) + `}}`
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)

			d := NewCookieDelegate(srv.URL, "hermes")
			_, err := d.Resolve(context.Background(), requestWithCookie("x=1"))
			if tc.ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected forbidden, got nil")
			}
			if !tc.ok {
				ae := asAuthError(err)
				if ae.Status != http.StatusForbidden {
					t.Fatalf("expected 403, got %d (%s)", ae.Status, ae.Message)
				}
			}
		})
	}
}

func TestCookieDelegate_PendingRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"a@b","name":"A","role":"pending","apps":"{}"}}`))
	}))
	t.Cleanup(srv.Close)
	d := NewCookieDelegate(srv.URL, "hermes")
	_, err := d.Resolve(context.Background(), requestWithCookie("x=1"))
	ae := asAuthError(err)
	if ae.Status != http.StatusForbidden {
		t.Fatalf("pending should 403, got %d (%s)", ae.Status, ae.Message)
	}
}

func TestCookieDelegate_NullBodyTreatedAsUnauthed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("null"))
	}))
	t.Cleanup(srv.Close)
	d := NewCookieDelegate(srv.URL, "hermes")
	_, err := d.Resolve(context.Background(), requestWithCookie("x=1"))
	ae := asAuthError(err)
	if ae.Status != http.StatusUnauthorized {
		t.Fatalf("null body should 401, got %d (%s)", ae.Status, ae.Message)
	}
}

func TestCookieDelegate_NoCookieIs401(t *testing.T) {
	d := NewCookieDelegate("http://unused.invalid", "hermes")
	_, err := d.Resolve(context.Background(), requestWithCookie(""))
	ae := asAuthError(err)
	if ae.Status != http.StatusUnauthorized {
		t.Fatalf("empty cookie should 401, got %d", ae.Status)
	}
}

func TestCookieDelegate_Cached(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"a@b","name":"A","role":"admin","apps":"{}"}}`))
	}))
	t.Cleanup(srv.Close)

	d := NewCookieDelegate(srv.URL, "hermes")
	for i := 0; i < 5; i++ {
		if _, err := d.Resolve(context.Background(), requestWithCookie("same-cookie")); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 upstream call (cache hits), got %d", calls)
	}
}

// jsonStringLiteral escapes s as a JSON string literal (quoted, escaped).
// Used to embed an inner JSON-encoded string inside the outer JSON.
func jsonStringLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
