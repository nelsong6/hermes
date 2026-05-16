// main.go — entry point + config.
//
// Configured entirely via env vars; failing fast on bad config rather
// than booting with bad defaults that 5xx in production.
//
//	UPSTREAM_URL       absolute URL the proxy forwards authenticated
//	                   traffic to. e.g. http://127.0.0.1:9119
//	PUBLIC_HOSTNAME    externally-visible hostname this proxy is
//	                   reached at; used for the post-sign-in callback.
//	                   e.g. hermes.romaine.life
//	AUTH_SESSION_URL   optional. default
//	                   https://auth.romaine.life/api/auth/get-session
//	AUTH_SIGNIN_URL    optional. default
//	                   https://auth.romaine.life/api/auth/sign-in/social/microsoft
//	REQUIRED_APP_KEY   optional. key in the auth.romaine.life user's
//	                   `apps` JSON column to require with .access=true.
//	                   Empty → admin-only. e.g. "hermes"
//	LISTEN_ADDR        optional. default :8080
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("hermes auth-proxy starting: listen=%s upstream=%s public=%s app=%s",
		cfg.ListenAddr, cfg.UpstreamURL, cfg.PublicHostname, cfg.RequiredAppKey)

	delegate := NewCookieDelegate(cfg.AuthSessionURL, cfg.RequiredAppKey)
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
	UpstreamURL     string
	PublicHostname  string
	AuthSessionURL  string
	AuthSigninURL   string
	RequiredAppKey  string
	ListenAddr      string
}

func loadConfig(get func(string) string) (config, error) {
	cfg := config{
		UpstreamURL:    get("UPSTREAM_URL"),
		PublicHostname: get("PUBLIC_HOSTNAME"),
		AuthSessionURL: get("AUTH_SESSION_URL"),
		AuthSigninURL:  get("AUTH_SIGNIN_URL"),
		RequiredAppKey: get("REQUIRED_APP_KEY"),
		ListenAddr:     get("LISTEN_ADDR"),
	}
	if cfg.UpstreamURL == "" {
		return cfg, errors.New("UPSTREAM_URL is required")
	}
	if cfg.PublicHostname == "" {
		return cfg, errors.New("PUBLIC_HOSTNAME is required")
	}
	if cfg.AuthSessionURL == "" {
		cfg.AuthSessionURL = "https://auth.romaine.life/api/auth/get-session"
	}
	if cfg.AuthSigninURL == "" {
		cfg.AuthSigninURL = "https://auth.romaine.life/api/auth/sign-in/social/microsoft"
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	return cfg, nil
}
