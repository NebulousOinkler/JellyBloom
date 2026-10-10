package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
	"github.com/NebulousOinkler/JellyBloom/internal/metrics"
	"github.com/NebulousOinkler/JellyBloom/internal/pairing"
	"github.com/NebulousOinkler/JellyBloom/internal/playback"
	"github.com/NebulousOinkler/JellyBloom/internal/proxy"
	"github.com/NebulousOinkler/JellyBloom/internal/redact"
	"github.com/NebulousOinkler/JellyBloom/internal/secrets"
	"github.com/NebulousOinkler/JellyBloom/internal/server"
	"github.com/NebulousOinkler/JellyBloom/internal/sources"
	"github.com/NebulousOinkler/JellyBloom/internal/upstream"
	"github.com/NebulousOinkler/JellyBloom/internal/userdata"
)

func main() {
	setupLogging("", "")

	subcommands := map[string]func([]string) error{
		"hash-password": func([]string) error { return hashPassword() },
		"pair":          pairCmd,
		"unpair":        unpairCmd,
		"report":        reportCmd,
		"sync":          syncCmd,
	}
	if len(os.Args) > 1 {
		if cmd, ok := subcommands[os.Args[1]]; ok {
			if err := cmd(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", redact.String(err.Error()))
				os.Exit(1)
			}
			return
		}
	}

	path := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	if err := run(*path); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// registerGauges adds the metrics read at scrape time: upstream health and
// speed, copy checks, sync state, and what clients see.
func registerGauges(store *index.Store, names []string, reg *sources.Registry, cat *catalog.Catalog, srv *server.Server) {
	m := metrics.Default
	type emitFn = func(v float64, labels ...string)
	one := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	m.NewGaugeFunc("jellybloom_build_info", "JellyBloom's version and the Go version it was built with.", []string{"version", "go_version"},
		func(emit emitFn) { emit(1, upstream.Version, runtime.Version()) })
	m.NewGaugeFunc("jellybloom_upstream_up", "Whether the upstream is reachable.", []string{"upstream"}, func(emit emitFn) {
		for _, u := range reg.UpstreamStates() {
			emit(one(u.Reachable), u.Name)
		}
	})
	m.NewGaugeFunc("jellybloom_upstream_latency_seconds", "Measured response latency to the upstream (moving average).", []string{"upstream"}, func(emit emitFn) {
		for _, u := range reg.UpstreamStates() {
			if u.LatencyMs > 0 {
				emit(u.LatencyMs/1000, u.Name)
			}
		}
	})
	m.NewGaugeFunc("jellybloom_upstream_throughput_bytes_per_second", "Measured relay throughput from the upstream (moving average).", []string{"upstream"}, func(emit emitFn) {
		for _, u := range reg.UpstreamStates() {
			if u.ThroughputMBps > 0 {
				emit(u.ThroughputMBps*1e6, u.Name)
			}
		}
	})
	m.NewGaugeFunc("jellybloom_sources_quarantined", "Copies quarantined after repeated failures.", nil,
		func(emit emitFn) { emit(float64(reg.Counts().Quarantined)) })
	m.NewGaugeFunc("jellybloom_copies_flagged", "Copies with a content verdict, by state (truncated, damaged, disputed).", []string{"state"}, func(emit emitFn) {
		for state, n := range reg.Counts().Verdicts {
			emit(float64(n), state)
		}
	})
	m.NewGaugeFunc("jellybloom_play_sessions", "Play sessions being tracked.", nil,
		func(emit emitFn) { emit(float64(reg.Counts().Sessions)) })
	status := func(emit func(st index.Status)) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, n := range names {
			if st, err := store.Status(ctx, n); err == nil {
				emit(st)
			}
		}
	}
	m.NewGaugeFunc("jellybloom_index_items", "Items indexed from the upstream.", []string{"upstream"},
		func(emit emitFn) { status(func(st index.Status) { emit(float64(st.Items), st.Upstream) }) })
	m.NewGaugeFunc("jellybloom_index_last_full_sync_timestamp_seconds", "When the upstream was last fully synced.", []string{"upstream"},
		func(emit emitFn) {
			status(func(st index.Status) {
				if !st.LastFullSync.IsZero() {
					emit(float64(st.LastFullSync.Unix()), st.Upstream)
				}
			})
		})
	m.NewGaugeFunc("jellybloom_index_sync_failing", "Whether the upstream's last sync failed.", []string{"upstream"},
		func(emit emitFn) { status(func(st index.Status) { emit(one(st.LastError != ""), st.Upstream) }) })
	m.NewGaugeFunc("jellybloom_catalog_items", "Items visible to clients.", nil, func(emit emitFn) {
		items, _ := cat.Counts()
		emit(float64(items))
	})
	m.NewGaugeFunc("jellybloom_catalog_libraries", "Libraries visible to clients.", nil, func(emit emitFn) {
		_, libs := cat.Counts()
		emit(float64(libs))
	})
	m.NewGaugeFunc("jellybloom_socket_clients", "Connected client sockets.", nil,
		func(emit emitFn) { emit(float64(srv.SocketClients())) })
}

// library lists items with media files and their copies for the source
// registry.
func library(res *identity.Resolver) sources.Library {
	return sources.LibraryFunc(func(ctx context.Context, fn func(sources.LibraryItem) error) error {
		return res.EachFile(ctx, func(id, typ string, copies []identity.CopyDTO) error {
			it := sources.LibraryItem{ID: id, Type: typ}
			for _, c := range copies {
				it.Copies = append(it.Copies, sources.LibraryCopy{Upstream: c.Upstream, Item: c.ID, DTO: c.DTO})
			}
			return fn(it)
		})
	})
}

// reportCmd: jellybloom report [-config f] [-json] [-all]
func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	all := fs.Bool("all", false, "list every item (default: the first 20 of each list)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: jellybloom report [-config FILE] [-json] [-all]")
		fmt.Fprintln(os.Stderr, "Lists items with one good copy, and broken, disputed, and quarantined copies, from")
		fmt.Fprintln(os.Stderr, "JellyBloom's database. Live upstream state is at /JellyBloom/Report (admin users).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
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
	resolver, err := identity.New(store, identity.Mode{Op: identity.Op(cfg.Merge), Upstreams: names, Base: cfg.Base, Subtract: cfg.Subtract}, nil)
	if err != nil {
		return err
	}
	reg, err := sources.New(store.DB(), sources.Options{Upstreams: names, Library: library(resolver)})
	if err != nil {
		return err
	}
	rep, err := reg.Report(context.Background())
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReport(os.Stdout, rep, *all)
	return nil
}

func printReport(w io.Writer, rep *sources.Report, all bool) {
	t := rep.Totals
	fmt.Fprintf(w, "%d items with media files; %d have copies on more than one upstream (%d not fully checked yet)\n\n",
		t.Items, t.MultiCopy, t.Unchecked)
	section := func(title string, items []sources.ReportItem, show func(sources.ReportCopy) bool) {
		fmt.Fprintf(w, "%s: %d\n", title, len(items))
		for i, it := range items {
			if i == 20 && !all {
				fmt.Fprintf(w, "  … %d more (use -all)\n", len(items)-20)
				break
			}
			fmt.Fprintf(w, "  %s [%s]\n", it.Name, it.Type)
			for _, c := range it.Copies {
				if !show(c) {
					continue
				}
				fmt.Fprintf(w, "      %s: %s  %s", c.Upstream, c.State, c.Path)
				if c.Reason != "" {
					fmt.Fprintf(w, "  (%s)", c.Reason)
				}
				fmt.Fprintln(w)
			}
		}
		fmt.Fprintln(w)
	}
	every := func(sources.ReportCopy) bool { return true }
	notOK := func(c sources.ReportCopy) bool { return c.State != "ok" }
	section("Broken copies (truncated or damaged)", rep.Broken, notOK)
	section("Disputed copies (they differ; neither looks damaged)", rep.Disputed, notOK)
	section("Quarantined copies (failing repeatedly)", rep.Quarantined, notOK)
	section("Items with a good copy on only one upstream", rep.SingleCopy, every)
}

// prompt reads a line from the terminal, without echo when hidden.
func prompt(label string, hidden bool) (string, error) {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	if hidden && term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// upstreamArg loads the config and finds the upstream named by the one
// positional argument.
func upstreamArg(fs *flag.FlagSet, args []string, configPath *string) (*config.Config, config.Upstream, error) {
	if err := fs.Parse(args); err != nil {
		return nil, config.Upstream{}, err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return nil, config.Upstream{}, errors.New("expected one upstream name")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return nil, config.Upstream{}, err
	}
	for _, u := range cfg.Upstreams {
		if u.Name == fs.Arg(0) {
			return cfg, u, nil
		}
	}
	return nil, config.Upstream{}, fmt.Errorf("no upstream named %q in %s", fs.Arg(0), *configPath)
}

// pairCmd: jellybloom pair [-config f] [-password | -api-key] NAME
func pairCmd(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	usePassword := fs.Bool("password", false, "pair by entering a password once (it is not stored) instead of Quick Connect")
	useAPIKey := fs.Bool("api-key", false, "pair with a server API key (admin-level; needs user: and allow_admin: true)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: jellybloom pair [-config FILE] [-password | -api-key] UPSTREAM")
		fs.PrintDefaults()
	}
	cfg, u, err := upstreamArg(fs, args, configPath)
	if err != nil {
		return err
	}
	mode := pairing.QuickConnect
	switch {
	case *usePassword && *useAPIKey:
		return errors.New("choose one of -password and -api-key")
	case *usePassword:
		mode = pairing.Password
	case *useAPIKey:
		mode = pairing.APIKey
	}
	store, err := secrets.Open(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return pairing.Pair(ctx, pairing.Options{Upstream: u, Store: store, Mode: mode, Out: os.Stdout, Prompt: prompt})
}

// unpairCmd: jellybloom unpair [-config f] NAME
func unpairCmd(args []string) error {
	fs := flag.NewFlagSet("unpair", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: jellybloom unpair [-config FILE] UPSTREAM") }
	cfg, u, err := upstreamArg(fs, args, configPath)
	if err != nil {
		return err
	}
	store, err := secrets.Open(cfg)
	if err != nil {
		return err
	}
	return pairing.Unpair(context.Background(), u, store, os.Stdout)
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

// setupLogging sets the default logger: text or JSON, at the given level
// (JELLYBLOOM_LOG=debug overrides it). Every line passes through credential
// redaction.
func setupLogging(format, level string) {
	lv := slog.LevelInfo
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	}
	if strings.EqualFold(os.Getenv("JELLYBLOOM_LOG"), "debug") {
		lv = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: redact.ReplaceAttr}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	setupLogging(cfg.LogFormat, cfg.LogLevel)
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

	vault, err := secrets.Open(cfg)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var clients []*upstream.Client
	var web *upstream.Client
	for _, u := range cfg.Upstreams {
		creds := secrets.Credentials(vault, u)
		if u.PasswordFile != "" {
			if warn := secrets.LoosePermissions(u.PasswordFile); warn != "" {
				slog.Warn("password_file permissions", "upstream", u.Name, "detail", warn)
			}
		} else if len(u.TokenCommand) == 0 {
			if _, err := creds.Load(ctx); errors.Is(err, upstream.ErrNotPaired) {
				slog.Warn("upstream is not paired; it stays offline until you run the pair command",
					"upstream", u.Name, "command", "jellybloom pair -config "+configPath+" "+u.Name)
			}
		}
		if u.AllowAdmin {
			slog.Warn("allow_admin is on: an administrator credential is permitted for this upstream", "upstream", u.Name)
		}
		client, err := upstream.New(u, creds, upstream.Options{})
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
	var px *proxy.Proxy
	reg, err := sources.New(store.DB(), sources.Options{
		Upstreams: names,
		// Checks and pings use JellyBloom's own upstream sessions.
		Open: func(ctx context.Context, up, method, path string, q url.Values, hdr http.Header) (*http.Response, error) {
			return px.Open(ctx, up, proxy.Caller{}, method, path, q, hdr)
		},
		Library:      library(resolver),
		StallTimeout: cfg.StallTimeout,
	})
	if err != nil {
		return err
	}
	px, err = proxy.New(proxy.Options{
		Upstreams:   clients,
		WebUpstream: cfg.WebUpstream,
		Resolver:    resolver,
		ServerID:    authSvc.ServerID(),
		Policy:      policy,
		Hidden:      func(id string) bool { return cat.Hidden(id) },
		Sources:     reg,
		Timeout:     cfg.UpstreamTimeout,
	})
	if err != nil {
		return err
	}
	ud, err := userdata.Open(store.DB())
	if err != nil {
		return err
	}
	cat = catalog.New(catalog.Options{
		UserData:    ud,
		DB:          store.DB(),
		Resolver:    resolver,
		Upstreams:   names,
		ServerID:    authSvc.ServerID(),
		UpstreamIDs: px.UpstreamIDs,
	})
	resolver.OnRebuild(func(ctx context.Context) {
		// Items merged into another keep their users' state under the new ID.
		if err := ud.FollowRedirects(ctx); err != nil {
			slog.Error("moving user data to merged items failed", "err", err)
		}
		if err := cat.Reload(ctx); err != nil {
			slog.Error("catalog reload failed", "err", err)
		}
		reg.Kick() // check new copies
	})
	// Start rebuilding only once the catalog hook is registered.
	wg.Add(1)
	go func() {
		defer wg.Done()
		resolver.Run(ctx, time.Second)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		reg.Run(ctx)
	}()
	srv, err := server.New(server.Options{
		ServerName: cfg.ServerName,
		DB:         store.DB(),
		Auth:       authSvc,
		Proxy:      px,
		Catalog:    cat,
		Playback:   playback.New(px, resolver, reg, nil),
		UserData:   ud,
		Sources:    reg,
		Resolver:   resolver,
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

	registerGauges(store, names, reg, cat, srv)
	if cfg.MetricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", metrics.Default)
		ms := &http.Server{Addr: cfg.MetricsListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := ms.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				slog.Error("metrics server failed", "addr", cfg.MetricsListen, "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			ms.Close()
		}()
		slog.Info("metrics listening", "addr", cfg.MetricsListen)
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
