package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8788", "listen `address` for the proxy and the login page")
	state := fs.String("state", "", "state directory")
	upstream := fs.String("upstream", apiBaseURL, "Anthropic API base URL")
	clientID := fs.String("client-id", defaultClientID, "OAuth client id")
	scope := fs.String("scope", defaultScope, "OAuth scopes to request")
	ua := fs.String("user-agent", defaultUA, "User-Agent sent upstream")
	publicURL := fs.String("public-url", "", "URL shown on the login page (informational)")
	_ = fs.Parse(args)

	store, err := NewStore(stateDir(nil, *state))
	if err != nil {
		return err
	}

	if changed, err := store.ImportFromClaudeCode(true); err != nil {
		log.Printf("import from claude CLI: %v", err)
	} else if changed {
		log.Printf("imported credentials from the Claude Code CLI")
	}

	logins := newLoginStore(store.Dir() + "/pending.json")
	showURL := *publicURL
	if showURL == "" {
		showURL = "http://127.0.0.1" + portOf(*addr) + "/"
	}

	proxy := &proxyHandler{store: store, upstream: *upstream, ua: *ua}
	web := &webHandler{store: store, logins: logins, clientID: *clientID, scope: *scope, proxyURL: "http://" + hostPort(*addr) + "/v1/messages"}

	mux := http.NewServeMux()
	mux.Handle("/v1/", proxy)
	// Shelley stores a custom model's endpoint verbatim, so some
	// registrations point straight at the service root.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && (r.URL.Path == "/" || r.URL.Path == "/messages") {
			proxy.ServeHTTP(w, r)
			return
		}
		web.ServeHTTP(w, r)
	}))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 20 * time.Second,
	}

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		log.Printf("shutting down")
		_ = srv.Close()
	}()

	log.Printf("listening on %s", *addr)
	log.Printf("login page: %s", showURL)
	log.Printf("proxy endpoint for Shelley: http://%s/v1/messages", hostPort(*addr))
	log.Printf("state dir: %s", store.Dir())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func hostPort(addr string) string {
	host, port := "127.0.0.1", "8788"
	i := len(addr) - 1
	for i >= 0 && addr[i] != ':' {
		i--
	}
	if i >= 0 {
		port = addr[i+1:]
		host = addr[:i]
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return host + ":" + port
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":8788"
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	state := fs.String("state", "", "state directory")
	_ = fs.Parse(args)

	store, err := NewStore(stateDir(nil, *state))
	if err != nil {
		return err
	}
	if changed, err := store.ImportFromClaudeCode(true); err == nil && changed {
		fmt.Println("imported credentials from the Claude Code CLI")
	}
	t := store.Snapshot()
	if t.RefreshToken == "" {
		fmt.Println("not signed in")
		fmt.Printf("proxy endpoint: http://%s/v1/messages\n", "127.0.0.1:8788")
		return nil
	}
	fmt.Println("signed in")
	fmt.Printf("  access token: %s\n", mask(t.AccessToken))
	fmt.Printf("  expires:      %s\n", t.ExpiresAt.Local().Format(time.RFC1123))
	if t.Subscription != "" {
		fmt.Printf("  plan:         %s\n", t.Subscription)
	}
	if e := emailOf(t.Account); e != "" {
		fmt.Printf("  account:      %s\n", e)
	}
	if t.Source != "" {
		fmt.Printf("  source:       %s\n", t.Source)
	}
	return nil
}

func runLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	state := fs.String("state", "", "state directory")
	_ = fs.Parse(args)
	store, err := NewStore(stateDir(nil, *state))
	if err != nil {
		return err
	}
	if err := store.Clear(); err != nil {
		return err
	}
	fmt.Println("signed out (Claude Code CLI credentials, if any, are untouched)")
	return nil
}

func runModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	state := fs.String("state", "", "state directory")
	_ = fs.Parse(args)

	store, err := NewStore(stateDir(nil, *state))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids, err := store.UpstreamModels(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	return nil
}

func runUseToken(args []string) error {
	fs := flag.NewFlagSet("use-token", flag.ExitOnError)
	state := fs.String("state", "", "state directory")
	_ = fs.Parse(args)

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return fmt.Errorf("pass the token on stdin, e.g. claude setup-token | claude-sub use-token")
	}
	store, err := NewStore(stateDir(nil, *state))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids, err := verifyAccessToken(ctx, tok)
	if err != nil {
		return err
	}
	if err := store.SetTokens(Tokens{
		AccessToken: strings.TrimPrefix(tok, "Bearer "),
		ExpiresAt:   time.Now().Add(365 * 24 * time.Hour).UTC(),
		Scopes:      defaultScope,
		ClientID:    defaultClientID,
		Source:      "pasted-token",
	}); err != nil {
		return err
	}
	store.setSubscription(defaultClientID, strings.Join(ids, " "))
	fmt.Printf("token accepted; %d models available\\n", len(ids))
	return nil
}
