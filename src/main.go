package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const appVersion = "0.3.0"
const appSlug = "mangagate"

func main() {
	var (
		configDir = flag.String("config", envOr("TRIM_PKGETC", "./config"), "configuration directory")
		dataDir   = flag.String("data", envOr("TRIM_PKGVAR", "./data"), "runtime data directory")
		portFlag  = flag.Int("port", 0, "listen port")
		socket    = flag.String("socket", "", "unix socket path for the fnOS gateway")
		rootsEnv  = flag.String("roots", os.Getenv("TRIM_DATA_ACCESSIBLE_PATHS"), "colon separated granted directories")
		initUser  = flag.String("init-user", "", "create or update this admin user")
		initPass  = flag.String("init-pass", "", "password for --init-user")
		scanOnly   = flag.Bool("scan", false, "scan the library, print stats and exit")
		tlsCert    = flag.String("tls-cert", envOr("tls_cert", ""), "TLS certificate chain (enables HTTPS on the same port)")
		tlsKey     = flag.String("tls-key", envOr("tls_key", ""), "TLS private key")
		scrapeSrc  = flag.String("scrape-source", envOr("scrape_source", "anilist"), "metadata source: anilist | bangumi")
		scrapeBase = flag.String("scrape-base", envOr("scrape_base", ""), "metadata source base URL override")
		scrapePx   = flag.String("scrape-proxy", envOr("scrape_proxy", ""), "HTTP(S) proxy for scraping, e.g. http://192.168.1.2:7890")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags)
	log.Printf("可达书柜 (mangagate) %s starting", appVersion)

	if err := os.MkdirAll(*configDir, 0o755); err != nil {
		log.Fatalf("create config dir: %v", err)
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	cfg, err := loadConfig(*configDir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	port := resolvePort(*portFlag, cfg.Port)
	cfg.Port = port

	users, err := loadUsers(*configDir)
	if err != nil {
		log.Fatalf("load users: %v", err)
	}
	if err := ensureUsers(cfg, users, *configDir, *initUser, *initPass); err != nil {
		log.Fatalf("init users: %v", err)
	}
	if len(users.Users) == 0 {
		log.Printf("WARNING: no users configured; set an admin password in the app wizard or via --init-user/--init-pass")
	}

	roots := resolveRoots(cfg, *rootsEnv)
	if len(roots) == 0 && cfg.Root != "" {
		// No granted directory: fall back to an explicitly configured root so
		// local development and manual setups still work.
		if st, err := os.Stat(cfg.Root); err == nil && st.IsDir() {
			roots = []string{cfg.Root}
		}
	}
	cfg.Roots = roots
	if err := saveConfig(*configDir, cfg); err != nil {
		log.Printf("save config: %v", err)
	}

	log.Printf("config=%s data=%s port=%d", *configDir, *dataDir, port)
	log.Printf("granted roots: %v", roots)
	if len(roots) == 0 {
		log.Printf("WARNING: no comic directory granted yet; use app settings to grant one")
	}

	lib := newLibrary(roots, cfg.LibraryName)
	libFromCache := false
	if len(roots) > 0 {
		if cached := loadLibraryCache(libraryCachePath(*dataDir), roots, cfg.LibraryName); cached != nil {
			lib = cached
			libFromCache = true
			log.Printf("loaded cached library: %d series, %d books; rescanning in background", lenSeries(lib), lenBooks(lib))
		} else {
			start := time.Now()
			lib.scan(nil, nil)
			log.Printf("scan finished in %s: %d series, %d books", time.Since(start).Round(time.Millisecond), lenSeries(lib), lenBooks(lib))
			if probs := len(lib.problemsSnapshot()); probs > 0 {
				log.Printf("scan found %d problems (see admin page)", probs)
			}
			_ = osWriteLastScan(*dataDir, time.Since(start))
			if err := lib.saveCache(libraryCachePath(*dataDir)); err != nil {
				log.Printf("save library cache: %v", err)
			}
		}
	}

	tlsCfg, err := loadTLSConfig(*tlsCert, *tlsKey)
	if err != nil {
		log.Printf("TLS disabled: %v", err)
	}

	s := &server{
		cfg:       cfg,
		users:     users,
		configDir: *configDir,
		dataDir:   *dataDir,
		lib:       lib,
		limiter:   newRateLimiter(),
		roots:     roots,
		port:      port,
		started:   time.Now(),
		tlsOn:     tlsCfg != nil,
		meta:      loadMetaStore(filepath.Join(*dataDir, "series-meta.json"), *dataDir),
		scrape:    newScraper(*scrapeSrc, *scrapeBase, *scrapePx),
	}

	if *scanOnly {
		return
	}

	// The cached library is served immediately while a fresh scan verifies it
	// in the background; unchanged archives skip the page recount.
	if libFromCache {
		s.startScan()
	}

	handler := s.routes()

	tcpAddr := ":" + strconv.Itoa(port)
	ln, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", tcpAddr, err)
	}

	if tlsCfg != nil {
		ln = &sniffListener{Listener: ln, cfg: tlsCfg}
		log.Printf("serving OPDS on %s (HTTP + HTTPS)", tcpAddr)
	} else {
		log.Printf("serving OPDS on %s (HTTP only)", tcpAddr)
	}

	tcpSrv := &http.Server{
		Handler:           s.requireAuth(handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	servers := []*http.Server{tcpSrv}
	sockets := []string{}

	if *socket != "" {
		_ = os.Remove(*socket)
		uln, err := net.Listen("unix", *socket)
		if err != nil {
			log.Printf("gateway socket disabled: %v", err)
		} else {
			_ = os.Chmod(*socket, 0o660)
			sockets = append(sockets, *socket)
			gatewaySrv := &http.Server{
				Handler:           s.gatewayAuth(stripPrefix(handler, "/app/"+appSlug)),
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       120 * time.Second,
			}
			servers = append(servers, gatewaySrv)
			go func() {
				if err := gatewaySrv.Serve(uln); err != nil && err != http.ErrServerClosed {
					log.Printf("gateway serve: %v", err)
				}
			}()
			log.Printf("gateway socket listening on %s", *socket)
		}
	}

	go func() {
		if err := tcpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("tcp serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
	for _, p := range sockets {
		_ = os.Remove(p)
	}
}

// routes builds the shared mux used by both listeners.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		series, _, _ := s.lib.snapshot()
		writeJSON(w, map[string]any{
			"status": "ok",
			"ready":  s.lib.rootCount() > 0,
			"series": len(series),
			"books":  countBooks(series),
			"uptime": int(time.Since(s.started).Seconds()),
		})
	})
	mux.HandleFunc("/opds", s.handleOPDS)
	mux.HandleFunc("/opds/", s.handleOPDS)

	mux.HandleFunc("/api/status", s.apiStatus)
	mux.HandleFunc("/api/users", s.adminOnly(s.apiUsers))
	mux.HandleFunc("/api/users/add", s.adminOnly(s.apiAddUser))
	mux.HandleFunc("/api/users/delete", s.adminOnly(s.apiDeleteUser))
	mux.HandleFunc("/api/users/password", s.adminOnly(s.apiSetPassword))
	mux.HandleFunc("/api/users/perm", s.adminOnly(s.apiUserPerm))
	mux.HandleFunc("/api/series", s.adminOnly(s.apiSeriesList))
	mux.HandleFunc("/api/rescan", s.adminOnly(s.apiRescan))
	mux.HandleFunc("/api/scan/problems", s.adminOnly(s.apiScanProblems))
	mux.HandleFunc("/api/log", s.adminOnly(s.apiLog))
	mux.HandleFunc("/api/export", s.adminOnly(s.apiExport))
	mux.HandleFunc("/api/import", s.adminOnly(s.apiImport))
	mux.HandleFunc("/api/scrape/status", s.adminOnly(s.apiScrapeStatus))
	mux.HandleFunc("/api/scrape/search", s.adminOnly(s.apiScrapeSearch))
	mux.HandleFunc("/api/scrape/apply", s.adminOnly(s.apiScrapeApply))
	mux.HandleFunc("/api/scrape/remove", s.adminOnly(s.apiScrapeRemove))
	mux.HandleFunc("/api/scrape/ignore", s.adminOnly(s.apiScrapeIgnore))
	mux.HandleFunc("/api/scrape/auto", s.adminOnly(s.apiScrapeAuto))
	mux.HandleFunc("/", s.handleAdminPage)
	return mux
}

func (s *server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			writeErr(w, http.StatusForbidden, "仅飞牛 NAS 管理员可执行此操作")
			return
		}
		h(w, r)
	}
}

// stripPrefix lets the same handler serve the gateway path /app/<appname>/.
func stripPrefix(h http.Handler, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) {
			r2 := r.Clone(r.Context())
			p := strings.TrimPrefix(r.URL.Path, prefix)
			if p == "" {
				p = "/"
			}
			r2.URL.Path = p
			r2.URL.RawPath = ""
			h.ServeHTTP(w, r2)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func resolvePort(flagVal, cfgPort int) int {
	if flagVal > 0 {
		return flagVal
	}
	if v := strings.TrimSpace(os.Getenv("port")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			return n
		}
	}
	if v := strings.TrimSpace(os.Getenv("TRIM_SERVICE_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			return n
		}
	}
	if cfgPort > 0 {
		return cfgPort
	}
	return defaultPort
}

// ensureUsers creates the first admin from wizard values on a fresh install.
func ensureUsers(cfg *Config, store *UserStore, configDir, initUser, initPass string) error {
	name := strings.TrimSpace(initUser)
	pass := initPass
	if name == "" || pass == "" {
		if n := strings.TrimSpace(os.Getenv("admin_user")); n != "" {
			name = n
		}
		if p := os.Getenv("admin_pass"); p != "" {
			pass = p
		}
	}
	if name == "" || pass == "" {
		return nil
	}
	if len(pass) < 6 {
		log.Printf("admin password is shorter than 6 characters; refusing to create user")
		return nil
	}
	existing := store.find(name)
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if existing != nil {
		// Only the explicit --init-pass flag overwrites an existing account.
		if initUser != "" && initPass != "" {
			existing.Hash = string(hash)
			existing.Admin = true
			return store.save(configDir)
		}
		return nil
	}
	store.Users = append(store.Users, User{Name: name, Hash: string(hash), Admin: true})
	cfg.AdminUser = name
	log.Printf("created admin user %q", name)
	return store.save(configDir)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func lenSeries(l *library) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.series)
}

func lenBooks(l *library) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.books)
}

func osWriteLastScan(dir string, d time.Duration) error {
	raw, _ := json.Marshal(map[string]any{
		"at":          time.Now().Format(time.RFC3339),
		"duration_ms": d.Milliseconds(),
	})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "last-scan.json"), raw, 0o644)
}
