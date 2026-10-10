package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/NebulousOinkler/JellyBloom/internal/auth"
	"github.com/NebulousOinkler/JellyBloom/internal/catalog"
	"github.com/NebulousOinkler/JellyBloom/internal/config"
	"github.com/NebulousOinkler/JellyBloom/internal/identity"
	"github.com/NebulousOinkler/JellyBloom/internal/index"
	"github.com/NebulousOinkler/JellyBloom/internal/playback"
	"github.com/NebulousOinkler/JellyBloom/internal/proxy"
	"github.com/NebulousOinkler/JellyBloom/internal/server"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if err := hashPassword(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	path := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	if err := run(*path); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// hashPassword reads a password (hidden when on a terminal) and prints a
// bcrypt hash for the users section of the config.
func hashPassword() error {
	var pw string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		pw = string(b)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return err
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if pw == "" {
		return errors.New("empty password")
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
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
	authSvc, err := auth.New(store.DB(), cfg.Users)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	var clients []*upstream.Client
	var web *upstream.Client
	for _, u := range cfg.Upstreams {
		client, err := upstream.New(u, upstream.Options{})
		if err != nil {
			return err
		}
		clients = append(clients, client)
		if u.Name == cfg.WebUpstream {
			web = client
		}
		syncer := index.NewSyncer(store, client, index.SyncerOptions{OnChange: resolver.Notify})
		wg.Add(1)
		go func() {
			defer wg.Done()
			syncer.Run(ctx)
		}()
	}

	policy, err := proxy.NewPolicy(cfg.FallbackWriteAllow)
	if err != nil {
		return err
	}
	var cat *catalog.Catalog
	px, err := proxy.New(proxy.Options{
		Upstreams:   clients,
		WebUpstream: cfg.WebUpstream,
		Resolver:    resolver,
		ServerID:    authSvc.ServerID(),
		Policy:      policy,
		Hidden:      func(id string) bool { return cat.Hidden(id) },
	})
	if err != nil {
		return err
	}
	cat = catalog.New(catalog.Options{
		DB:          store.DB(),
		Resolver:    resolver,
		Upstreams:   names,
		ServerID:    authSvc.ServerID(),
		UpstreamIDs: px.UpstreamIDs,
	})
	resolver.OnRebuild(func(ctx context.Context) {
		if err := cat.Reload(ctx); err != nil {
			slog.Error("catalog reload failed", "err", err)
		}
	})
	// Start rebuilding only once the catalog hook is registered.
	wg.Add(1)
	go func() {
		defer wg.Done()
		resolver.Run(ctx, time.Second)
	}()
	srv, err := server.New(server.Options{
		ServerName: cfg.ServerName,
		DB:         store.DB(),
		Auth:       authSvc,
		Proxy:      px,
		Catalog:    cat,
		Playback:   playback.New(px, resolver, nil),
		Version: func(ctx context.Context) (string, error) {
			info, err := web.PublicInfo(ctx)
			if err != nil {
				return "", err
			}
			return info.Version, nil
		},
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("JellyBloom listening", "addr", cfg.Listen, "upstreams", len(cfg.Upstreams), "merge", cfg.Merge)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	wg.Wait()
	return nil
}
