// proxy.go: HTTP reverse proxy that gates each request via
// SessionResolver, redirects unauthenticated callers to Entra via
// auth.romaine.life, and returns 403 to signed-in callers without the
// per-app grant.
package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Proxy fronts an upstream HTTP service (the hermes dashboard) and
// applies cookie-delegate auth on every request.
type Proxy struct {
	resolver       SessionResolver
	upstream       *httputil.ReverseProxy
	publicHostname string // e.g. hermes.romaine.life — used to build the post-login callback URL
	signinURL      string // e.g. https://auth.romaine.life/api/auth/sign-in/social/microsoft
	logger         logger
}

type logger interface {
	Printf(format string, args ...any)
}

// NewProxy wires a Proxy. The upstream must be an absolute URL (e.g.
// http://127.0.0.1:9119). publicHostname is the externally-visible
// hostname this proxy is reached at — used for building the
// post-sign-in callback URL.
func NewProxy(upstreamURL, publicHostname, signinURL string, resolver SessionResolver, log logger) (*Proxy, error) {
	target, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, err
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, errors.New("upstream URL must be absolute (e.g. http://127.0.0.1:9119)")
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	// httputil's default ErrorHandler logs to stderr; replace with our
	// logger + 502 so a dead upstream surfaces clearly.
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream error: %v", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	// httputil preserves the inbound Host header which would route the
	// upstream by the hermes.romaine.life vhost. The dashboard binds
	// 127.0.0.1 and serves a default vhost; rewriting Host to the
	// upstream's keeps the upstream's routing simple.
	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
		// Strip the inbound auth cookie before forwarding upstream —
		// the dashboard has no notion of auth.romaine.life cookies
		// and we don't want them leaking into Hermes' own state.
		stripCookieDomain(req, ".romaine.life")
		// Suppress the X-Forwarded-For that httputil.ReverseProxy adds
		// by default. The hermes dashboard binds to 127.0.0.1 and gates
		// every /api/{ws,events,pty} WebSocket on the client IP being
		// in {127.0.0.1, ::1, localhost, testclient}. Uvicorn's default
		// proxy_headers=True rewrites ws.client.host from XFF when the
		// TCP source is a trusted IP — and 127.0.0.1 (where the
		// reverse-proxy connects from) is uvicorn's default trust list.
		// Passing the browser's public IP through would make every WS
		// upgrade close with 4403 (Hermes' "non-loopback client") and
		// surface as a generic 'failed' in DevTools. The auth-proxy
		// IS the security boundary; the dashboard should see its
		// caller as the loopback proxy. The real user identity is
		// surfaced via X-Forwarded-User/-Sub/-Role above.
		//
		// IMPORTANT: this must be an explicit nil set, NOT Header.Del.
		// ReverseProxy.ServeHTTP appends X-Forwarded-For from
		// req.RemoteAddr AFTER the Director returns, gated by the
		// presence-of-nil sentinel:
		//   prior, ok := outreq.Header["X-Forwarded-For"]
		//   omit := ok && prior == nil
		//   if !omit { Header.Set(...) }
		// Header.Del removes the key entirely so ok=false → omit=false
		// → the auto-add still fires. Setting to nil leaves ok=true,
		// prior==nil → omit=true → no auto-add. (PR #8 used Del and
		// was a no-op; this PR is the actual fix.)
		req.Header["X-Forwarded-For"] = nil
	}

	return &Proxy{
		resolver:       resolver,
		upstream:       rp,
		publicHostname: publicHostname,
		signinURL:      signinURL,
		logger:         log,
	}, nil
}

// ServeHTTP implements http.Handler. Health and ready probes (/healthz,
// /readyz) bypass auth so kubelet probes work without a session.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Debug logging — temporary, see PR for context. Logs method, path,
	// whether the request is a WS upgrade, cookie presence, accept,
	// and a snippet of the user-agent. Helps diagnose why upgrades
	// fail through the proxy.
	isUpgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	cookiePresent := r.Header.Get("Cookie") != ""
	ua := r.Header.Get("User-Agent")
	if len(ua) > 40 {
		ua = ua[:40]
	}
	p.logger.Printf("req %s %s upgrade=%v cookie=%v accept=%q ua=%q",
		r.Method, r.URL.Path, isUpgrade, cookiePresent, r.Header.Get("Accept"), ua)

	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		return
	}

	user, err := p.resolver.Resolve(r.Context(), r.Header.Get("Cookie"))
	if err != nil {
		ae := asAuthError(err)
		switch ae.Status {
		case http.StatusUnauthorized:
			// WebSocket upgrades can't follow 302s — return 401 so
			// the browser surfaces it instead of attempting a
			// redirect. (For non-upgrade browser GETs, the redirect
			// is fine and even desirable.)
			if !isUpgrade && wantsHTML(r) && r.Method == http.MethodGet {
				p.logger.Printf("decision: redirect-to-signin (unauthed browser GET)")
				p.redirectToSignin(w, r)
				return
			}
			p.logger.Printf("decision: 401 unauthed (%s)", ae.Message)
			w.Header().Set("WWW-Authenticate", `Cookie realm="hermes.romaine.life"`)
			http.Error(w, "unauthorized: "+ae.Message, http.StatusUnauthorized)
			return
		case http.StatusForbidden:
			p.logger.Printf("decision: 403 forbidden (%s)", ae.Message)
			http.Error(w, "forbidden: "+ae.Message, http.StatusForbidden)
			return
		default:
			p.logger.Printf("session resolve failure: status=%d %s", ae.Status, ae.Message)
			http.Error(w, ae.Message, ae.Status)
			return
		}
	}

	// Attach identity headers for the upstream. Hermes doesn't read
	// these today but downstream observability and any future
	// in-upstream gating can rely on them.
	r.Header.Set("X-Forwarded-User", user.Email)
	r.Header.Set("X-Forwarded-User-Sub", user.Sub)
	r.Header.Set("X-Forwarded-User-Role", user.Role)
	r.Header.Set("X-Forwarded-Proto", "https")

	p.logger.Printf("decision: forward to upstream (user=%s upgrade=%v)", user.Email, isUpgrade)
	p.upstream.ServeHTTP(w, r)
}

// redirectToSignin sends a 302 to auth.romaine.life's
// sign-in-with-Microsoft route, asking it to redirect the browser back
// to the originally-requested URL on this proxy. The callbackURL is the
// FULL public URL of the request, not just the path, because Better
// Auth's social-sign-in validates origin against trustedOrigins.
func (p *Proxy) redirectToSignin(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	callback := "https://" + p.publicHostname + target

	q := url.Values{}
	q.Set("callbackURL", callback)
	dest := p.signinURL + "?" + q.Encode()

	// Tell browsers not to cache this redirect — the destination
	// changes per request via the callbackURL query param.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, dest, http.StatusFound)
}

func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		// curl with no Accept header is "anything"; treat as
		// non-browser to avoid surprise redirects.
		return false
	}
	return strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*") && strings.Contains(r.Header.Get("User-Agent"), "Mozilla")
}

// stripCookieDomain rewrites the inbound Cookie header to drop any
// cookies whose name starts with the given prefix. Better Auth's
// session cookie is named `better-auth.session_token` (and friends),
// scoped to `.romaine.life`. We don't want to forward those to the
// upstream — Hermes has no business reading them.
//
// `domain` is treated as a substring match against cookie names, not
// strict — we're filtering by convention, not by Set-Cookie attributes
// which aren't visible on the request side. In practice the only
// `.romaine.life`-scoped cookies the user has are Better Auth's, so
// the simpler shape is fine.
func stripCookieDomain(req *http.Request, _ string) {
	cookies := req.Cookies()
	if len(cookies) == 0 {
		return
	}
	kept := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if strings.HasPrefix(c.Name, "better-auth.") {
			continue
		}
		kept = append(kept, c.Name+"="+c.Value)
	}
	if len(kept) == 0 {
		req.Header.Del("Cookie")
		return
	}
	req.Header.Set("Cookie", strings.Join(kept, "; "))
}

// shutdownTimeout is the grace period for in-flight requests when the
// proxy receives SIGTERM. Kept generous because dashboard pages can
// have long-lived requests (analytics, log tails).
const shutdownTimeout = 30 * time.Second
