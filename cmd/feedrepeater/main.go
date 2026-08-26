// Command feedrepeater posts RSS and Atom entries to Mastodon, Bluesky,
// Discord, Slack, ntfy, and webhooks.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	// Embeds the IANA timezone database, so an account's timezone resolves the
	// same way wherever the binary runs. Without it a host with no tzdata reads
	// every zone as UTC, which is a wrong date rather than a visible failure.
	_ "time/tzdata"

	"feedrepeater.com/internal/config"
	"feedrepeater.com/internal/feed"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/publisher"
	"feedrepeater.com/internal/safehttp"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
	"feedrepeater.com/internal/web"
	"feedrepeater.com/internal/worker"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "genkey":
			fmt.Println(secret.GenerateRootKey())
			return
		case "version":
			fmt.Println(version())
			return
		default:
			fmt.Fprintf(os.Stderr, "usage: feedrepeater [genkey|version]\n")
			os.Exit(2)
		}
	}

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	keys, err := secret.NewKeyring(cfg.SecretKey)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	hc := safehttp.New(safehttp.Options{
		UserAgent:    cfg.UserAgent,
		Timeout:      25 * time.Second,
		MaxBytes:     5 << 20,
		AllowPrivate: cfg.AllowPrivateNetworks,
	})
	if cfg.AllowPrivateNetworks {
		log.Warn("private network access is enabled; do not run this way in production")
	}

	md := mastodon.New(hc)
	fetcher := feed.NewFetcher(hc)
	pub := publisher.New(st, keys, hc, md, log)

	// Built before the server so its Notify can be handed over: a handler that
	// queues something says so directly, which is what lets the delivery loop
	// idle for minutes instead of waking every ten seconds to find nothing.
	wrk := worker.New(cfg, st, fetcher, pub, log)

	srv, err := web.NewServer(web.Deps{
		Config:    cfg,
		Store:     st,
		Keyring:   keys,
		HTTP:      hc,
		Mastodon:  md,
		Fetcher:   fetcher,
		Publisher: pub,
		Logger:    log,
		Notify:    wrk.Notify,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		wrk.Run(ctx)
	}()

	err = srv.Serve(ctx)
	stop()
	wg.Wait()

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
}

func version() string {
	return "feedrepeater 0.1.0"
}
