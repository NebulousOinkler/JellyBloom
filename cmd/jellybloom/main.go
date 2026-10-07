package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/index"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

func main() {
	path := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	if err := run(*path); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	store, err := index.Open(filepath.Join(cfg.DataDir, "jellybloom.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	var names []string
	for _, u := range cfg.Upstreams {
		names = append(names, u.Name)
	}
	resolver, err := identity.New(store, identity.Mode{
		Op:        identity.Op(cfg.Merge),
		Upstreams: names,
		Base:      cfg.Base,
		Subtract:  cfg.Subtract,
	}, nil)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resolver.Run(ctx, time.Second)
	}()
	for _, u := range cfg.Upstreams {
		client, err := upstream.New(u, upstream.Options{})
		if err != nil {
			return err
		}
		syncer := index.NewSyncer(store, client, index.SyncerOptions{OnChange: resolver.Notify})
		wg.Add(1)
		go func() {
			defer wg.Done()
			syncer.Run(ctx)
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ServerName":"` + cfg.ServerName + `","ProductName":"JellyBloom"}`))
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	slog.Info("JellyBloom listening", "addr", cfg.Listen, "upstreams", len(cfg.Upstreams), "merge", cfg.Merge)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	wg.Wait()
	return nil
}
