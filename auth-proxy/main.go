// main.go — entry point + config.
//
// Configured entirely via env vars; failing fast on bad config rather
// than booting with bad defaults that 5xx in production.
//
// Shared (both modes):
//
//	UPSTREAM_URL       absolute URL the proxy forwards authenticated
//	                   traffic to. e.g. http://127.0.0.1:9119
//	PUBLIC_HOSTNAME    externally-visible hostname this proxy is
//	                   reached at; used for the post-sign-in callback.
//	                   e.g. hermes.romaine.life
//	LISTEN_ADDR        optional. default :8080
//	AUTH_MODE          optional. "cookie" (default) or "service-jwt".
//	                   Selects the SessionResolver implementation; see
//	                   below for per-mode env vars.
//
// AUTH_MODE=cookie (dashboard / human callers):
//
//	AUTH_SESSION_URL   optional. default
//	                   https://auth.romaine.life/api/auth/get-session
//	AUTH_SIGNIN_URL    optional. default
//	                   https://auth.romaine.life/api/auth/sign-in/social/microsoft
//	REQUIRED_APP_KEY   optional. key in the auth.romaine.life user's
//	                   `apps` JSON column to require with .access=true.
//	                   Empty → admin-only. e.g. "hermes"
//
// AUTH_MODE=service-jwt (Hermes API / service-to-service):
//
//	ALLOWED_ACTOR_EMAIL_DOMAINS  required. Comma-separated list of
//	                             actor_email domain suffixes that may
//	                             drive this proxy. A JWT with
//	                             role=service but actor_email in a
//	                             different domain is rejected. Today's
//	                             only legitimate value for Hermes' API
//	                             is `service.tank-operator.romaine.life`.
//	AUTH_JWKS_URL                optional. default
//	                             https://auth.romaine.life/api/auth/jwks
//	AUTH_ISSUER                  optional. default https://auth.romaine.life
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	var delegate SessionResolver
	switch cfg.AuthMode {
	case "cookie", "":
		log.Printf("hermes auth-proxy starting (cookie mode): listen=%s upstream=%s public=%s app=%s",
			cfg.ListenAddr, cfg.UpstreamURL, cfg.PublicHostname, cfg.RequiredAppKey)
		delegate = NewCookieDelegate(cfg.AuthSessionURL, cfg.RequiredAppKey)
	case "service-jwt":
		log.Printf("hermes auth-proxy starting (service-jwt mode): listen=%s upstream=%s allowed_actor_domains=%v jwks=%s",
			cfg.ListenAddr, cfg.UpstreamURL, cfg.AllowedActorEmailDomains, cfg.AuthJWKSURL)
		delegate = NewServiceJWTDelegate(cfg.AuthJWKSURL, cfg.AuthIssuer, cfg.AllowedActorEmailDomains)
	default:
		log.Fatalf("config: unknown AUTH_MODE %q (expected cookie or service-jwt)", cfg.AuthMode)
	}

	proxy, err := NewProxy(cfg.UpstreamURL, cfg.PublicHostname, cfg.AuthSigninURL, delegate, log.Default())
	if err != nil {
		log.Fatalf("proxy: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		close(idleConnsClosed)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
	<-idleConnsClosed
	log.Print("bye")
}

type config struct {
	UpstreamURL    string
	PublicHostname string
	ListenAddr     string
	AuthMode       string

	// cookie-mode
	AuthSessionURL string
	AuthSigninURL  string
	RequiredAppKey string

	// service-jwt mode
	AllowedActorEmailDomains []string
	AuthJWKSURL              string
	AuthIssuer               string
}

func loadConfig(get func(string) string) (config, error) {
	cfg := config{
		UpstreamURL:    get("UPSTREAM_URL"),
		PublicHostname: get("PUBLIC_HOSTNAME"),
		AuthSessionURL: get("AUTH_SESSION_URL"),
		AuthSigninURL:  get("AUTH_SIGNIN_URL"),
		RequiredAppKey: get("REQUIRED_APP_KEY"),
		ListenAddr:     get("LISTEN_ADDR"),
		AuthMode:       strings.TrimSpace(get("AUTH_MODE")),
		AuthJWKSURL:    get("AUTH_JWKS_URL"),
		AuthIssuer:     get("AUTH_ISSUER"),
	}
	if cfg.UpstreamURL == "" {
		return cfg, errors.New("UPSTREAM_URL is required")
	}
	// PublicHostname is required for cookie-mode redirects but optional
	// for service-jwt (no redirects ever). Validate per mode below.
	switch cfg.AuthMode {
	case "", "cookie":
		if cfg.PublicHostname == "" {
			return cfg, errors.New("PUBLIC_HOSTNAME is required for cookie mode")
		}
		if cfg.AuthSessionURL == "" {
			cfg.AuthSessionURL = "https://auth.romaine.life/api/auth/get-session"
		}
		if cfg.AuthSigninURL == "" {
			cfg.AuthSigninURL = "https://auth.romaine.life/api/auth/sign-in/social/microsoft"
		}
	case "service-jwt":
		raw := strings.TrimSpace(get("ALLOWED_ACTOR_EMAIL_DOMAINS"))
		if raw == "" {
			return cfg, errors.New("ALLOWED_ACTOR_EMAIL_DOMAINS is required for service-jwt mode " +
				"(comma-separated list; today's only valid entry for Hermes' API is " +
				"`service.tank-operator.romaine.life`)")
		}
		for _, d := range strings.Split(raw, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				cfg.AllowedActorEmailDomains = append(cfg.AllowedActorEmailDomains, d)
			}
		}
		if len(cfg.AllowedActorEmailDomains) == 0 {
			return cfg, errors.New("ALLOWED_ACTOR_EMAIL_DOMAINS contained only empty values")
		}
		// AuthJWKSURL / AuthIssuer fall back to defaults inside the delegate.
	default:
		return cfg, fmt.Errorf("unknown AUTH_MODE %q (expected cookie or service-jwt)", cfg.AuthMode)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	return cfg, nil
}
