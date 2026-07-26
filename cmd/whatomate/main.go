package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/assignment"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/frontend"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/internal/tts"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/internal/worker"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"github.com/zerodha/logf"
)

var (
	Version   = "dev"
	BuildTime = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "worker":
		runWorker(os.Args[2:])
	case "rls-migrate":
		runRLSMigration(os.Args[2:])
	case "version":
		fmt.Printf("ReReply %s (built %s)\n", Version, BuildTime)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Printf("Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`ReReply - WhatsApp Business API Platform

Usage:
  rereply <command> [options]

Commands:
  server    Start the API server (with optional embedded workers)
  worker    Start background workers only (no API server)
  rls-migrate  Run schema migrations and install PostgreSQL tenant RLS
  version   Show version information
  help      Show this help message

Server Options:
  -config string    Path to config file (default "config.toml")
  -migrate          Run database migrations on startup
  -workers int      Number of embedded workers (0 to disable) (default 1)

Worker Options:
  -config string    Path to config file (default "config.toml")
  -workers int      Number of workers to run (default 1)

Examples:
  rereply server                     # API + 1 embedded worker
  rereply server -workers 0          # API only (no workers)
  rereply server -workers 4          # API + 4 embedded workers
  rereply server -migrate            # Run migrations and start server
  rereply worker -workers 4          # 4 workers only (no API)
  rereply rls-migrate                # Pre-deploy migration using database.migration_url

Deployment Scenarios:
  All-in-one:    rereply server
  Separate:      rereply server -workers 0  (on API server)
                 rereply worker -workers 4  (on worker server)`)
}

// ============================================================================
// RLS MIGRATION COMMAND
// ============================================================================

func runRLSMigration(args []string) {
	migrationFlags := flag.NewFlagSet("rls-migrate", flag.ExitOnError)
	configPath := migrationFlags.String("config", "config.toml", "Path to config file")
	rollback := migrationFlags.Bool("rollback", false, "Disable ReReply RLS policies")
	_ = migrationFlags.Parse(args)

	lo := logf.New(logf.Opts{
		Level:           logf.InfoLevel,
		TimestampFormat: "2006-01-02 15:04:05",
		DefaultFields:   []any{"app", "rereply-rls-migrate"},
	})

	cfg, err := config.Load(*configPath)
	if err != nil {
		lo.Fatal("Failed to load config", "error", err)
	}
	if cfg.Database.MigrationURL == "" {
		lo.Fatal("database.migration_url is required for RLS migrations")
	}
	if cfg.Database.RuntimeRole == "" {
		lo.Fatal("database.runtime_role is required for RLS migrations")
	}

	migrationCfg := cfg.Database
	migrationCfg.URL = cfg.Database.MigrationURL
	db, err := database.NewPostgres(&migrationCfg, false)
	if err != nil {
		lo.Fatal("Failed to connect with migration database role", "error", err)
	}

	if *rollback {
		if err := database.RemoveTenantRLS(db); err != nil {
			lo.Fatal("RLS rollback failed", "error", err)
		}
		lo.Info("Tenant RLS disabled")
		return
	}

	if err := database.RunMigrationWithProgress(db, &cfg.DefaultAdmin); err != nil {
		lo.Fatal("Schema migration failed", "error", err)
	}
	if err := handlers.BackfillChatbotFlowGraph(db, lo); err != nil {
		lo.Fatal("Chatbot flow graph backfill failed", "error", err)
	}
	if err := database.ApplyTenantRLS(db, cfg.Database.RuntimeRole); err != nil {
		lo.Fatal("Tenant RLS migration failed", "error", err)
	}
	lo.Info("Tenant RLS installed", "runtime_role", cfg.Database.RuntimeRole)
}

// ============================================================================
// SERVER COMMAND
// ============================================================================

func runServer(args []string) {
	serverFlags := flag.NewFlagSet("server", flag.ExitOnError)
	configPath := serverFlags.String("config", "config.toml", "Path to config file")
	migrate := serverFlags.Bool("migrate", false, "Run database migrations")
	numWorkers := serverFlags.Int("workers", 1, "Number of workers to run (0 to disable embedded workers)")
	_ = serverFlags.Parse(args)

	// Initialize logger
	lo := logf.New(logf.Opts{
		EnableColor:     true,
		Level:           logf.DebugLevel,
		EnableCaller:    true,
		TimestampFormat: "2006-01-02 15:04:05",
		DefaultFields:   []any{"app", "rereply"},
	})

	lo.Info("Starting ReReply server...", "version", Version)

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		lo.Fatal("Failed to load config", "error", err)
	}
	if cfg.Database.RLSEnabled && *migrate {
		lo.Fatal("server -migrate is not allowed when database.rls_enabled=true; run the rls-migrate pre-deploy command")
	}

	// Validate JWT secret
	if cfg.App.Environment == "production" && len(cfg.JWT.Secret) < 32 {
		lo.Fatal("JWT secret must be at least 32 characters in production")
	}
	if cfg.JWT.Secret == "" {
		lo.Warn("JWT secret is empty, using a random secret (tokens will not persist across restarts)")
	}

	// Warn if debug mode is on in production
	if cfg.App.Environment == "production" && cfg.App.Debug {
		lo.Warn("Debug mode is enabled in production! This may expose sensitive information.")
	}

	// Require explicit CORS origins in production
	if cfg.App.Environment == "production" && cfg.Server.AllowedOrigins == "" {
		lo.Fatal("server.allowed_origins must be set in production (e.g. \"https://app.example.com\")")
	}

	// Set log level based on environment
	if cfg.App.Environment == "production" {
		lo = logf.New(logf.Opts{
			Level:           logf.InfoLevel,
			TimestampFormat: "2006-01-02 15:04:05",
			DefaultFields:   []any{"app", "rereply"},
		})
	}

	// Connect to PostgreSQL
	db, err := database.NewPostgres(&cfg.Database, cfg.App.Debug)
	if err != nil {
		lo.Fatal("Failed to connect to database", "error", err)
	}
	lo.Info("Connected to PostgreSQL")
	if cfg.Database.RLSEnabled {
		if err := database.VerifyTenantRLS(db, cfg.Database.RuntimeRole); err != nil {
			lo.Fatal("PostgreSQL tenant RLS verification failed", "error", err)
		}
		lo.Info("PostgreSQL tenant RLS verified", "runtime_role", cfg.Database.RuntimeRole)
	}

	// Run migrations if requested
	if *migrate {
		if err := database.RunMigrationWithProgress(db, &cfg.DefaultAdmin); err != nil {
			lo.Fatal("Migration failed", "error", err)
		}
		// Backfill v2 graph for any legacy chatbot flow still on Steps[].
		// Idempotent — re-running is a no-op once every row is converted.
		if err := handlers.BackfillChatbotFlowGraph(db, lo); err != nil {
			lo.Fatal("Chatbot flow graph backfill failed", "error", err)
		}
	}

	// Connect to Redis
	rdb, err := database.NewRedis(&cfg.Redis)
	if err != nil {
		lo.Fatal("Failed to connect to Redis", "error", err)
	}
	lo.Info("Connected to Redis")

	// Initialize job queue
	jobQueue := queue.NewRedisQueue(rdb, lo)
	lo.Info("Job queue initialized")

	// Initialize Fastglue
	g := fastglue.NewGlue()

	// Initialize WhatsApp client
	waClient := whatsapp.NewWithBaseURL(lo, cfg.WhatsApp.BaseURL)

	// Initialize WebSocket hub
	wsHub := websocket.NewHub(lo)
	go wsHub.Run()
	lo.Info("WebSocket hub started")

	// Initialize app with dependencies
	// Shared HTTP client with connection pooling for external API calls
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:         handlers.SSRFSafeDialer(),
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	app := &handlers.App{
		Config:     cfg,
		DB:         db,
		Redis:      rdb,
		Log:        lo,
		WhatsApp:   waClient,
		WSHub:      wsHub,
		Queue:      jobQueue,
		HTTPClient: httpClient,
	}

	// Initialize the S3-compatible client when durable media storage or call
	// recording needs it. Explicit object-storage configuration fails fast so
	// production never silently falls back to an app instance's local disk.
	var s3Client *storage.S3Client
	if cfg.Storage.Type == "s3" || cfg.Calling.RecordingEnabled {
		var err error
		s3Client, err = storage.NewS3Client(&cfg.Storage)
		if err != nil {
			if cfg.Storage.Type == "s3" {
				lo.Fatal("Failed to initialize configured object storage", "error", err)
			}
			lo.Warn("Failed to initialize S3 client for recordings, recording disabled", "error", err)
		} else {
			lo.Info("S3-compatible storage initialized", "bucket", cfg.Storage.S3Bucket)
		}
	}
	if cfg.Storage.Type == "s3" {
		app.ObjectStore = s3Client
	}

	// Initialize shared assignment engine (used by both chat and call transfers)
	assigner := assignment.New(db, rdb, lo)
	app.Assigner = assigner

	// Initialize CallManager (per-org calling_enabled DB setting controls access)
	app.CallManager = calling.NewManager(&cfg.Calling, s3Client, db, rdb, waClient, wsHub, assigner, lo)
	app.S3Client = s3Client
	lo.Info("Call manager initialized")

	// Initialize TTS if configured (requires piper binary + model)
	if cfg.TTS.PiperBinary != "" && cfg.TTS.PiperModel != "" {
		app.TTS = &tts.PiperTTS{
			BinaryPath:    cfg.TTS.PiperBinary,
			ModelPath:     cfg.TTS.PiperModel,
			OpusencBinary: cfg.TTS.OpusencBinary,
			AudioDir:      cfg.Calling.AudioDir,
		}
		lo.Info("TTS initialized", "piper", cfg.TTS.PiperBinary, "model", cfg.TTS.PiperModel)
	}

	// Start campaign stats subscriber for real-time WebSocket updates from worker
	if err := app.StartCampaignStatsSubscriber(); err != nil {
		lo.Error("Failed to start campaign stats subscriber", "error", err)
	}

	// Parse allowed origins for CORS
	allowedOrigins := middleware.ParseAllowedOrigins(cfg.Server.AllowedOrigins)

	// Setup middleware (CORS is handled by corsWrapper at fasthttp level)
	g.Before(middleware.SecurityHeaders())
	g.Before(middleware.RequestLogger(lo))
	g.Before(middleware.Recovery(lo))
	g.Before(middleware.CSRFProtection())

	// Setup routes
	setupRoutes(g, app, lo, cfg.Server.BasePath, rdb, cfg)

	// Create server with CORS wrapper
	server := &fasthttp.Server{
		Handler:            corsWrapper(g.Handler(), allowedOrigins),
		ReadTimeout:        time.Duration(cfg.Server.ReadTimeout) * time.Second,
		WriteTimeout:       time.Duration(cfg.Server.WriteTimeout) * time.Second,
		MaxRequestBodySize: 15 * 1024 * 1024,
		Name:               "ReReply",
	}

	// Start server in goroutine
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	go func() {
		lo.Info("Server listening", "address", addr)
		if err := server.ListenAndServe(addr); err != nil {
			lo.Fatal("Server failed", "error", err)
		}
	}()

	// Start SLA processor (runs every minute)
	slaProcessor := handlers.NewSLAProcessor(app, time.Minute)
	slaCtx, slaCancel := context.WithCancel(context.Background())
	go slaProcessor.Start(slaCtx)
	lo.Info("SLA processor started")

	// Start embedded workers
	var workers []*worker.Worker
	var workerCancel context.CancelFunc
	if *numWorkers > 0 {
		var workerCtx context.Context
		workerCtx, workerCancel = context.WithCancel(context.Background())

		for i := 0; i < *numWorkers; i++ {
			w, err := worker.New(cfg, db, rdb, lo)
			if err != nil {
				lo.Fatal("Failed to create worker", "error", err, "worker_num", i+1)
			}
			workers = append(workers, w)

			workerNum := i + 1
			go func() {
				lo.Info("Worker started", "worker_num", workerNum)
				if err := w.Run(workerCtx); err != nil && err != context.Canceled {
					lo.Error("Worker error", "error", err, "worker_num", workerNum)
				}
			}()
		}
		lo.Info("Embedded workers started", "count", *numWorkers)
	} else {
		lo.Info("Embedded workers disabled, run workers separately")
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	lo.Info("Shutting down...")

	// Stop campaign stats subscriber
	lo.Info("Stopping campaign stats subscriber...")
	app.StopCampaignStatsSubscriber()
	lo.Info("Campaign stats subscriber stopped")

	// Stop SLA processor
	lo.Info("Stopping SLA processor...")
	slaCancel()
	slaProcessor.Stop()
	lo.Info("SLA processor stopped")

	// Stop workers first
	if workerCancel != nil {
		lo.Info("Stopping workers...", "count", len(workers))
		workerCancel()
		for _, w := range workers {
			_ = w.Close()
		}
		lo.Info("Workers stopped")
	}

	// Then stop server
	lo.Info("Stopping server...")
	if err := server.Shutdown(); err != nil {
		lo.Error("Server shutdown error", "error", err)
	}
	lo.Info("Server stopped")
}

// ============================================================================
// WORKER COMMAND
// ============================================================================

func runWorker(args []string) {
	workerFlags := flag.NewFlagSet("worker", flag.ExitOnError)
	configPath := workerFlags.String("config", "config.toml", "Path to config file")
	workerCount := workerFlags.Int("workers", 1, "Number of workers to run")
	_ = workerFlags.Parse(args)

	// Initialize logger
	lo := logf.New(logf.Opts{
		EnableColor:     true,
		Level:           logf.DebugLevel,
		EnableCaller:    true,
		TimestampFormat: "2006-01-02 15:04:05",
		DefaultFields:   []any{"app", "rereply-worker"},
	})

	lo.Info("Starting ReReply worker...", "version", Version)

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		lo.Fatal("Failed to load config", "error", err)
	}

	// Set log level based on environment
	if cfg.App.Environment == "production" {
		lo = logf.New(logf.Opts{
			Level:           logf.InfoLevel,
			TimestampFormat: "2006-01-02 15:04:05",
			DefaultFields:   []any{"app", "rereply-worker"},
		})
	}

	// Connect to PostgreSQL
	db, err := database.NewPostgres(&cfg.Database, cfg.App.Debug)
	if err != nil {
		lo.Fatal("Failed to connect to database", "error", err)
	}
	lo.Info("Connected to PostgreSQL")
	if cfg.Database.RLSEnabled {
		if err := database.VerifyTenantRLS(db, cfg.Database.RuntimeRole); err != nil {
			lo.Fatal("PostgreSQL tenant RLS verification failed", "error", err)
		}
		lo.Info("PostgreSQL tenant RLS verified", "runtime_role", cfg.Database.RuntimeRole)
	}

	// Connect to Redis
	rdb, err := database.NewRedis(&cfg.Redis)
	if err != nil {
		lo.Fatal("Failed to connect to Redis", "error", err)
	}
	lo.Info("Connected to Redis")

	// Setup context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Create and run workers
	workers := make([]*worker.Worker, *workerCount)
	errCh := make(chan error, *workerCount)

	for i := 0; i < *workerCount; i++ {
		w, err := worker.New(cfg, db, rdb, lo)
		if err != nil {
			lo.Fatal("Failed to create worker", "error", err, "worker_num", i+1)
		}
		workers[i] = w

		go func(workerNum int) {
			lo.Info("Worker started", "worker_num", workerNum)
			errCh <- w.Run(ctx)
		}(i + 1)
	}

	lo.Info("Workers started", "count", *workerCount)

	// Wait for shutdown signal or error
	select {
	case sig := <-quit:
		lo.Info("Received shutdown signal", "signal", sig)
		cancel()
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			lo.Error("Worker error", "error", err)
			cancel()
		}
	}

	// Cleanup
	lo.Info("Shutting down workers...")
	for _, w := range workers {
		if w != nil {
			if err := w.Close(); err != nil {
				lo.Error("Error closing worker", "error", err)
			}
		}
	}
	lo.Info("Workers stopped")
}

// ============================================================================
// ROUTES
// ============================================================================

func setupRoutes(g *fastglue.Fastglue, app *handlers.App, lo logf.Logger, basePath string, rdb *redis.Client, cfg *config.Config) {
	tenant := app.Tenant

	// Health check
	g.GET("/health", app.HealthCheck)
	g.GET("/ready", app.ReadyCheck)

	g.GET("/api/embedded-signup/config", tenant((*handlers.App).GetEmbeddedSignupConfig))

	// Auth routes (public, optionally rate-limited)
	if cfg.RateLimit.Enabled {
		window := time.Duration(cfg.RateLimit.WindowSeconds) * time.Second
		lo.Info("Rate limiting enabled on auth endpoints",
			"login_max", cfg.RateLimit.LoginMaxAttempts,
			"register_max", cfg.RateLimit.RegisterMaxAttempts,
			"refresh_max", cfg.RateLimit.RefreshMaxAttempts,
			"sso_max", cfg.RateLimit.SSOMaxAttempts,
			"window_seconds", cfg.RateLimit.WindowSeconds)

		g.POST("/api/auth/login", withRateLimit(app.Login, middleware.RateLimitOpts{
			Redis: rdb, Log: lo, Max: cfg.RateLimit.LoginMaxAttempts, Window: window, KeyPrefix: "login", TrustProxy: cfg.RateLimit.TrustProxy,
		}))
		g.POST("/api/auth/register", withRateLimit(app.Register, middleware.RateLimitOpts{
			Redis: rdb, Log: lo, Max: cfg.RateLimit.RegisterMaxAttempts, Window: window, KeyPrefix: "register", TrustProxy: cfg.RateLimit.TrustProxy,
		}))
		g.POST("/api/auth/refresh", withRateLimit(app.RefreshToken, middleware.RateLimitOpts{
			Redis: rdb, Log: lo, Max: cfg.RateLimit.RefreshMaxAttempts, Window: window, KeyPrefix: "refresh", TrustProxy: cfg.RateLimit.TrustProxy,
		}))
	} else {
		g.POST("/api/auth/login", app.Login)
		g.POST("/api/auth/register", app.Register)
		g.POST("/api/auth/refresh", app.RefreshToken)
	}
	g.POST("/api/auth/logout", app.Logout)
	g.POST("/api/auth/switch-org", app.SwitchOrg)
	g.GET("/api/auth/ws-token", app.GetWSToken)

	// SSO routes (public, optionally rate-limited)
	g.GET("/api/auth/sso/providers", app.GetPublicSSOProviders)
	if cfg.RateLimit.Enabled {
		window := time.Duration(cfg.RateLimit.WindowSeconds) * time.Second
		g.GET("/api/auth/sso/{provider}/init", withRateLimit(app.InitSSO, middleware.RateLimitOpts{
			Redis: rdb, Log: lo, Max: cfg.RateLimit.SSOMaxAttempts, Window: window, KeyPrefix: "sso_init", TrustProxy: cfg.RateLimit.TrustProxy,
		}))
		g.GET("/api/auth/sso/{provider}/callback", withRateLimit(app.CallbackSSO, middleware.RateLimitOpts{
			Redis: rdb, Log: lo, Max: cfg.RateLimit.SSOMaxAttempts, Window: window, KeyPrefix: "sso_callback", TrustProxy: cfg.RateLimit.TrustProxy,
		}))
	} else {
		g.GET("/api/auth/sso/{provider}/init", app.InitSSO)
		g.GET("/api/auth/sso/{provider}/callback", app.CallbackSSO)
	}

	// Webhook routes (public - for Meta)
	g.GET("/api/webhook", app.WebhookVerify)
	g.POST("/api/webhook", app.WebhookHandler)

	// WebSocket route (auth via message-based flow after upgrade)
	g.GET("/ws", app.WebSocketHandler)

	// For protected routes, we'll use a path-based middleware approach
	// Apply auth middleware globally but check path in the middleware
	g.Before(func(r *fastglue.Request) *fastglue.Request {
		// Skip auth for OPTIONS preflight requests (handled by CORS middleware)
		if string(r.RequestCtx.Method()) == "OPTIONS" {
			return r
		}
		path := string(r.RequestCtx.Path())
		// Skip auth for public routes
		if path == "/health" || path == "/ready" ||
			path == "/api/auth/login" || path == "/api/auth/register" || path == "/api/auth/refresh" ||
			path == "/api/auth/logout" || path == "/api/webhook" || path == "/ws" {
			return r
		}
		// Skip auth for SSO routes (they handle their own auth via state tokens)
		if len(path) >= 13 && path[:13] == "/api/auth/sso" {
			return r
		}
		// Skip auth for custom action redirects (uses one-time token)
		if len(path) >= 28 && path[:28] == "/api/custom-actions/redirect" {
			return r
		}
		// Apply auth for all other /api routes (supports both JWT and API key)
		if len(path) > 4 && path[:4] == "/api" {
			return middleware.AuthWithDB(app.Config.JWT.Secret, app.DB)(r)
		}
		return r
	})

	// Global rate limit on all /api/ routes, keyed by user ID (or IP if unauthenticated).
	// Runs after auth so the user identity is available.
	if cfg.RateLimit.Enabled {
		apiMax := cfg.RateLimit.APIMaxRequests
		if apiMax == 0 {
			apiMax = 200
		}
		apiWindow := cfg.RateLimit.APIWindowSeconds
		if apiWindow == 0 {
			apiWindow = 60
		}
		apiRL := middleware.UserAwareRateLimit(middleware.RateLimitOpts{
			Redis:      rdb,
			Log:        lo,
			Max:        apiMax,
			Window:     time.Duration(apiWindow) * time.Second,
			KeyPrefix:  "api_global",
			TrustProxy: cfg.RateLimit.TrustProxy,
		})
		g.Before(func(r *fastglue.Request) *fastglue.Request {
			path := string(r.RequestCtx.Path())
			if len(path) > 4 && path[:4] == "/api" {
				return apiRL(r)
			}
			return r
		})
	}

	// Role-based access control middleware
	g.Before(func(r *fastglue.Request) *fastglue.Request {
		method := string(r.RequestCtx.Method())

		// Skip OPTIONS preflight requests
		if method == "OPTIONS" {
			return r
		}

		// Route-level permission checks are now handled at the handler level
		// using the granular permission system (HasPermission checks)
		return r
	})

	// Current User (all authenticated users)
	g.GET("/api/me", tenant((*handlers.App).GetCurrentUser))
	g.PUT("/api/me/settings", tenant((*handlers.App).UpdateCurrentUserSettings))
	g.PUT("/api/me/password", tenant((*handlers.App).ChangePassword))
	g.PUT("/api/me/availability", tenant((*handlers.App).UpdateAvailability))
	g.GET("/api/me/organizations", tenant((*handlers.App).ListMyOrganizations))

	// Reseller control plane. These routes intentionally run outside a tenant
	// transaction because they manage portfolios spanning organizations.
	g.GET("/api/resellers", app.ListResellers)
	g.POST("/api/resellers", app.CreateReseller)
	g.GET("/api/resellers/{id}", app.GetReseller)
	g.PUT("/api/resellers/{id}", app.UpdateReseller)
	g.GET("/api/resellers/{id}/usage", app.GetResellerUsage)
	g.GET("/api/resellers/{id}/members", app.ListResellerMembers)
	g.POST("/api/resellers/{id}/members", app.AddResellerMember)
	g.DELETE("/api/resellers/{id}/members/{member_id}", app.RemoveResellerMember)

	// User Management (admin only - enforced by middleware)
	g.GET("/api/users", tenant((*handlers.App).ListUsers))
	g.POST("/api/users", tenant((*handlers.App).CreateUser))
	g.GET("/api/users/{id}", tenant((*handlers.App).GetUser))
	g.PUT("/api/users/{id}", tenant((*handlers.App).UpdateUser))
	g.DELETE("/api/users/{id}", tenant((*handlers.App).DeleteUser))

	// Roles & Permissions (admin only - enforced by middleware)
	g.GET("/api/roles", tenant((*handlers.App).ListRoles))
	g.POST("/api/roles", tenant((*handlers.App).CreateRole))
	g.GET("/api/roles/{id}", tenant((*handlers.App).GetRole))
	g.PUT("/api/roles/{id}", tenant((*handlers.App).UpdateRole))
	g.DELETE("/api/roles/{id}", tenant((*handlers.App).DeleteRole))
	g.GET("/api/permissions", tenant((*handlers.App).ListPermissions))

	// API Keys (admin only - enforced by middleware)
	g.GET("/api/api-keys", tenant((*handlers.App).ListAPIKeys))
	g.GET("/api/api-keys/{id}", tenant((*handlers.App).GetAPIKey))
	g.POST("/api/api-keys", tenant((*handlers.App).CreateAPIKey))
	g.PUT("/api/api-keys/{id}", tenant((*handlers.App).UpdateAPIKey))
	g.DELETE("/api/api-keys/{id}", tenant((*handlers.App).DeleteAPIKey))

	// Accounts
	g.GET("/api/accounts", tenant((*handlers.App).ListAccounts))
	g.POST("/api/accounts", tenant((*handlers.App).CreateAccount))
	g.POST("/api/accounts/exchange-token", tenant((*handlers.App).ExchangeToken)) // Embedded signup
	g.GET("/api/accounts/{id}", tenant((*handlers.App).GetAccount))
	g.PUT("/api/accounts/{id}", tenant((*handlers.App).UpdateAccount))
	g.DELETE("/api/accounts/{id}", tenant((*handlers.App).DeleteAccount))
	g.POST("/api/accounts/{id}/register", tenant((*handlers.App).RegisterPhoneNumber)) // Embedded signup manual/2fa registration
	g.POST("/api/accounts/{id}/test", tenant((*handlers.App).TestAccountConnection))
	g.POST("/api/accounts/{id}/subscribe", tenant((*handlers.App).SubscribeApp))
	g.GET("/api/accounts/{id}/business_profile", tenant((*handlers.App).GetBusinessProfile))
	g.PUT("/api/accounts/{id}/business_profile", tenant((*handlers.App).UpdateBusinessProfile))
	g.POST("/api/accounts/{id}/business_profile/photo", tenant((*handlers.App).UpdateProfilePicture))

	// Contacts
	g.GET("/api/contacts", tenant((*handlers.App).ListContacts))
	g.POST("/api/contacts", tenant((*handlers.App).CreateContact))
	g.GET("/api/contacts/{id}", tenant((*handlers.App).GetContact))
	g.PUT("/api/contacts/{id}", tenant((*handlers.App).UpdateContact))
	g.DELETE("/api/contacts/{id}", tenant((*handlers.App).DeleteContact))
	g.PUT("/api/contacts/{id}/assign", tenant((*handlers.App).AssignContact))
	g.PUT("/api/contacts/{id}/tags", tenant((*handlers.App).UpdateContactTags))
	g.GET("/api/contacts/{id}/session-data", tenant((*handlers.App).GetContactSessionData))

	// Generic Import/Export
	g.POST("/api/export", tenant((*handlers.App).ExportData))
	g.POST("/api/import", tenant((*handlers.App).ImportData))
	g.GET("/api/export/{table}/config", tenant((*handlers.App).GetExportConfig))
	g.GET("/api/import/{table}/config", tenant((*handlers.App).GetImportConfig))

	// Tags
	g.GET("/api/tags", tenant((*handlers.App).ListTags))
	g.POST("/api/tags", tenant((*handlers.App).CreateTag))
	g.PUT("/api/tags/{name}", tenant((*handlers.App).UpdateTag))
	g.DELETE("/api/tags/{name}", tenant((*handlers.App).DeleteTag))

	// Messages
	g.GET("/api/contacts/{id}/messages", tenant((*handlers.App).GetMessages))
	g.POST("/api/contacts/{id}/messages", tenant((*handlers.App).SendMessage))
	g.POST("/api/contacts/{id}/mark-read", tenant((*handlers.App).MarkContactRead))
	g.POST("/api/contacts/{id}/messages/{message_id}/reaction", tenant((*handlers.App).SendReaction))
	g.POST("/api/messages", tenant((*handlers.App).SendMessage)) // Legacy route
	g.POST("/api/messages/template", tenant((*handlers.App).SendTemplateMessage))
	g.POST("/api/messages/media", tenant((*handlers.App).SendMediaMessage))
	g.PUT("/api/messages/{id}/read", tenant((*handlers.App).MarkMessageRead))

	// Conversation Notes
	g.GET("/api/contacts/{id}/notes", tenant((*handlers.App).ListConversationNotes))
	g.POST("/api/contacts/{id}/notes", tenant((*handlers.App).CreateConversationNote))
	g.PUT("/api/contacts/{id}/notes/{note_id}", tenant((*handlers.App).UpdateConversationNote))
	g.DELETE("/api/contacts/{id}/notes/{note_id}", tenant((*handlers.App).DeleteConversationNote))

	// Media (serves media files for messages, auth-protected)
	g.GET("/api/media/{message_id}", tenant((*handlers.App).ServeMedia))

	// Templates
	g.GET("/api/templates", tenant((*handlers.App).ListTemplates))
	g.POST("/api/templates", tenant((*handlers.App).CreateTemplate))
	g.GET("/api/templates/{id}", tenant((*handlers.App).GetTemplate))
	g.PUT("/api/templates/{id}", tenant((*handlers.App).UpdateTemplate))
	g.DELETE("/api/templates/{id}", tenant((*handlers.App).DeleteTemplate))
	g.POST("/api/templates/sync", tenant((*handlers.App).SyncTemplates))
	g.POST("/api/templates/{id}/publish", tenant((*handlers.App).SubmitTemplate))
	g.POST("/api/templates/upload-media", tenant((*handlers.App).UploadTemplateMedia))

	// WhatsApp Flows
	g.GET("/api/flows", tenant((*handlers.App).ListFlows))
	g.POST("/api/flows", tenant((*handlers.App).CreateFlow))
	g.GET("/api/flows/{id}", tenant((*handlers.App).GetFlow))
	g.PUT("/api/flows/{id}", tenant((*handlers.App).UpdateFlow))
	g.DELETE("/api/flows/{id}", tenant((*handlers.App).DeleteFlow))
	g.POST("/api/flows/{id}/save-to-meta", tenant((*handlers.App).SaveFlowToMeta))
	g.POST("/api/flows/{id}/publish", tenant((*handlers.App).PublishFlow))
	g.POST("/api/flows/{id}/deprecate", tenant((*handlers.App).DeprecateFlow))
	g.POST("/api/flows/{id}/duplicate", tenant((*handlers.App).DuplicateFlow))
	g.POST("/api/flows/sync", tenant((*handlers.App).SyncFlows))

	// Bulk Campaigns
	g.GET("/api/campaigns", tenant((*handlers.App).ListCampaigns))
	g.POST("/api/campaigns", tenant((*handlers.App).CreateCampaign))
	g.GET("/api/campaigns/{id}", tenant((*handlers.App).GetCampaign))
	g.PUT("/api/campaigns/{id}", tenant((*handlers.App).UpdateCampaign))
	g.DELETE("/api/campaigns/{id}", tenant((*handlers.App).DeleteCampaign))
	g.POST("/api/campaigns/{id}/start", tenant((*handlers.App).StartCampaign))
	g.POST("/api/campaigns/{id}/pause", tenant((*handlers.App).PauseCampaign))
	g.POST("/api/campaigns/{id}/cancel", tenant((*handlers.App).CancelCampaign))
	g.POST("/api/campaigns/{id}/retry-failed", tenant((*handlers.App).RetryFailed))
	g.GET("/api/campaigns/{id}/progress", tenant((*handlers.App).GetCampaign))
	g.POST("/api/campaigns/{id}/recipients/import", tenant((*handlers.App).ImportRecipients))
	g.GET("/api/campaigns/{id}/recipients", tenant((*handlers.App).GetCampaignRecipients))
	g.DELETE("/api/campaigns/{id}/recipients/{recipientId}", tenant((*handlers.App).DeleteCampaignRecipient))
	g.POST("/api/campaigns/{id}/media", tenant((*handlers.App).UploadCampaignMedia))
	g.GET("/api/campaigns/{id}/media", tenant((*handlers.App).ServeCampaignMedia))

	// Chatbot Settings
	g.GET("/api/chatbot/settings", tenant((*handlers.App).GetChatbotSettings))
	g.PUT("/api/chatbot/settings", tenant((*handlers.App).UpdateChatbotSettings))

	// Keyword Rules
	g.GET("/api/chatbot/keywords", tenant((*handlers.App).ListKeywordRules))
	g.POST("/api/chatbot/keywords", tenant((*handlers.App).CreateKeywordRule))
	g.GET("/api/chatbot/keywords/{id}", tenant((*handlers.App).GetKeywordRule))
	g.PUT("/api/chatbot/keywords/{id}", tenant((*handlers.App).UpdateKeywordRule))
	g.DELETE("/api/chatbot/keywords/{id}", tenant((*handlers.App).DeleteKeywordRule))

	// Chatbot Flows
	g.GET("/api/chatbot/flows", tenant((*handlers.App).ListChatbotFlows))
	g.POST("/api/chatbot/flows", tenant((*handlers.App).CreateChatbotFlow))
	g.GET("/api/chatbot/flows/{id}", tenant((*handlers.App).GetChatbotFlow))
	g.PUT("/api/chatbot/flows/{id}", tenant((*handlers.App).UpdateChatbotFlow))
	g.DELETE("/api/chatbot/flows/{id}", tenant((*handlers.App).DeleteChatbotFlow))

	// AI Contexts
	g.GET("/api/chatbot/ai-contexts", tenant((*handlers.App).ListAIContexts))
	g.POST("/api/chatbot/ai-contexts", tenant((*handlers.App).CreateAIContext))
	g.GET("/api/chatbot/ai-contexts/{id}", tenant((*handlers.App).GetAIContext))
	g.PUT("/api/chatbot/ai-contexts/{id}", tenant((*handlers.App).UpdateAIContext))
	g.DELETE("/api/chatbot/ai-contexts/{id}", tenant((*handlers.App).DeleteAIContext))

	// Agent Transfers
	g.GET("/api/chatbot/transfers", tenant((*handlers.App).ListAgentTransfers))
	g.POST("/api/chatbot/transfers", tenant((*handlers.App).CreateAgentTransfer))
	g.POST("/api/chatbot/transfers/pick", tenant((*handlers.App).PickNextTransfer))
	g.PUT("/api/chatbot/transfers/{id}/resume", tenant((*handlers.App).ResumeFromTransfer))
	g.PUT("/api/chatbot/transfers/{id}/assign", tenant((*handlers.App).AssignAgentTransfer))

	// Teams (admin/manager - access control in handler)
	g.GET("/api/teams", tenant((*handlers.App).ListTeams))
	g.POST("/api/teams", tenant((*handlers.App).CreateTeam))
	g.GET("/api/teams/{id}", tenant((*handlers.App).GetTeam))
	g.PUT("/api/teams/{id}", tenant((*handlers.App).UpdateTeam))
	g.DELETE("/api/teams/{id}", tenant((*handlers.App).DeleteTeam))
	g.GET("/api/teams/{id}/members", tenant((*handlers.App).ListTeamMembers))
	g.POST("/api/teams/{id}/members", tenant((*handlers.App).AddTeamMember))
	g.DELETE("/api/teams/{id}/members/{member_user_id}", tenant((*handlers.App).RemoveTeamMember))

	// Audit Logs
	g.GET("/api/audit-logs", tenant((*handlers.App).ListAuditLogs))
	g.GET("/api/audit-logs/{id}", tenant((*handlers.App).GetAuditLog))

	// Canned Responses
	g.GET("/api/canned-responses", tenant((*handlers.App).ListCannedResponses))
	g.POST("/api/canned-responses", tenant((*handlers.App).CreateCannedResponse))
	g.GET("/api/canned-responses/{id}", tenant((*handlers.App).GetCannedResponse))
	g.PUT("/api/canned-responses/{id}", tenant((*handlers.App).UpdateCannedResponse))
	g.DELETE("/api/canned-responses/{id}", tenant((*handlers.App).DeleteCannedResponse))
	g.POST("/api/canned-responses/{id}/use", tenant((*handlers.App).IncrementCannedResponseUsage))

	// Sessions (admin/debug)
	g.GET("/api/chatbot/sessions", tenant((*handlers.App).ListChatbotSessions))
	g.GET("/api/chatbot/sessions/{id}", tenant((*handlers.App).GetChatbotSession))

	// Analytics
	g.GET("/api/analytics/dashboard", tenant((*handlers.App).GetDashboardStats))
	g.GET("/api/analytics/messages", tenant((*handlers.App).GetMessageAnalytics))
	g.GET("/api/analytics/chatbot", tenant((*handlers.App).GetChatbotAnalytics))
	g.GET("/api/analytics/agents", tenant((*handlers.App).GetAgentAnalytics))
	g.GET("/api/analytics/agents/{id}", tenant((*handlers.App).GetAgentDetails))
	g.GET("/api/analytics/agents/comparison", tenant((*handlers.App).GetAgentComparison))

	// Meta WhatsApp Analytics
	g.GET("/api/analytics/meta", tenant((*handlers.App).GetMetaAnalytics))
	g.GET("/api/analytics/meta/accounts", tenant((*handlers.App).ListMetaAccountsForAnalytics))
	g.POST("/api/analytics/meta/refresh", tenant((*handlers.App).RefreshMetaAnalyticsCache))

	// Widgets (customizable analytics)
	g.GET("/api/widgets", tenant((*handlers.App).ListWidgets))
	g.POST("/api/widgets", tenant((*handlers.App).CreateWidget))
	g.GET("/api/widgets/data-sources", tenant((*handlers.App).GetWidgetDataSources))
	g.GET("/api/widgets/data", tenant((*handlers.App).GetAllWidgetsData))
	g.GET("/api/widgets/{id}", tenant((*handlers.App).GetWidget))
	g.PUT("/api/widgets/{id}", tenant((*handlers.App).UpdateWidget))
	g.DELETE("/api/widgets/{id}", tenant((*handlers.App).DeleteWidget))
	g.GET("/api/widgets/{id}/data", tenant((*handlers.App).GetWidgetData))
	g.POST("/api/widgets/layout", tenant((*handlers.App).SaveWidgetLayout))

	// Organization Settings
	g.GET("/api/org/settings", tenant((*handlers.App).GetOrganizationSettings))
	g.PUT("/api/org/settings", tenant((*handlers.App).UpdateOrganizationSettings))
	g.POST("/api/org/audio", tenant((*handlers.App).UploadOrgAudio))

	// Organizations
	g.GET("/api/organizations", tenant((*handlers.App).ListOrganizations))
	g.POST("/api/organizations", app.CreateOrganization)
	g.POST("/api/organizations/invitations", tenant((*handlers.App).CreateOrganizationInvitation))
	g.GET("/api/organizations/current", tenant((*handlers.App).GetCurrentOrganization))
	g.GET("/api/organizations/members", tenant((*handlers.App).ListOrganizationMembers))
	g.POST("/api/organizations/members", tenant((*handlers.App).AddOrganizationMember))
	g.PUT("/api/organizations/members/{member_id}", tenant((*handlers.App).UpdateOrganizationMemberRole))
	g.DELETE("/api/organizations/members/{member_id}", tenant((*handlers.App).RemoveOrganizationMember))

	// SSO Settings (admin only - enforced by middleware)
	g.GET("/api/settings/sso", tenant((*handlers.App).GetSSOSettings))
	g.PUT("/api/settings/sso/{provider}", tenant((*handlers.App).UpdateSSOProvider))
	g.DELETE("/api/settings/sso/{provider}", tenant((*handlers.App).DeleteSSOProvider))

	// Webhooks
	g.GET("/api/webhooks", tenant((*handlers.App).ListWebhooks))
	g.POST("/api/webhooks", tenant((*handlers.App).CreateWebhook))
	g.GET("/api/webhooks/{id}", tenant((*handlers.App).GetWebhook))
	g.PUT("/api/webhooks/{id}", tenant((*handlers.App).UpdateWebhook))
	g.DELETE("/api/webhooks/{id}", tenant((*handlers.App).DeleteWebhook))
	g.POST("/api/webhooks/{id}/test", tenant((*handlers.App).TestWebhook))

	// Custom Actions
	g.GET("/api/custom-actions", tenant((*handlers.App).ListCustomActions))
	g.POST("/api/custom-actions", tenant((*handlers.App).CreateCustomAction))
	g.GET("/api/custom-actions/{id}", tenant((*handlers.App).GetCustomAction))
	g.PUT("/api/custom-actions/{id}", tenant((*handlers.App).UpdateCustomAction))
	g.DELETE("/api/custom-actions/{id}", tenant((*handlers.App).DeleteCustomAction))
	g.POST("/api/custom-actions/{id}/execute", tenant((*handlers.App).ExecuteCustomAction))
	g.GET("/api/custom-actions/redirect/{token}", app.CustomActionRedirect)

	// IVR Flows
	g.GET("/api/ivr-flows", tenant((*handlers.App).ListIVRFlows))
	g.GET("/api/ivr-flows/{id}", tenant((*handlers.App).GetIVRFlow))
	g.POST("/api/ivr-flows", tenant((*handlers.App).CreateIVRFlow))
	g.PUT("/api/ivr-flows/{id}", tenant((*handlers.App).UpdateIVRFlow))
	g.DELETE("/api/ivr-flows/{id}", tenant((*handlers.App).DeleteIVRFlow))
	g.POST("/api/ivr-flows/audio", tenant((*handlers.App).UploadIVRAudio))
	g.GET("/api/ivr-flows/audio/{filename}", tenant((*handlers.App).ServeIVRAudio))

	// Call Logs
	g.GET("/api/call-logs", tenant((*handlers.App).ListCallLogs))
	g.GET("/api/call-logs/{id}", tenant((*handlers.App).GetCallLog))
	g.GET("/api/call-logs/{id}/recording", tenant((*handlers.App).GetCallRecording))

	// Call Transfers
	g.GET("/api/call-transfers", tenant((*handlers.App).ListCallTransfers))
	g.GET("/api/call-transfers/{id}", tenant((*handlers.App).GetCallTransfer))
	g.POST("/api/call-transfers/{id}/connect", tenant((*handlers.App).ConnectCallTransfer))
	g.POST("/api/call-transfers/{id}/hangup", tenant((*handlers.App).HangupCallTransfer))
	g.POST("/api/call-transfers/initiate", tenant((*handlers.App).InitiateAgentTransfer))

	// Call Hold
	g.POST("/api/call-logs/{id}/hold", tenant((*handlers.App).HoldCall))
	g.POST("/api/call-logs/{id}/resume", tenant((*handlers.App).ResumeCall))

	// Outgoing Calls
	g.POST("/api/calls/outgoing", tenant((*handlers.App).InitiateOutgoingCall))
	g.POST("/api/calls/outgoing/{id}/hangup", tenant((*handlers.App).HangupOutgoingCall))
	g.POST("/api/calls/permission-request", tenant((*handlers.App).SendCallPermissionRequest))
	g.GET("/api/calls/permission/{contactId}", tenant((*handlers.App).GetCallPermission))
	g.GET("/api/calls/ice-servers", tenant((*handlers.App).GetICEServers))

	// Catalogs
	g.GET("/api/catalogs", tenant((*handlers.App).ListCatalogs))
	g.POST("/api/catalogs", tenant((*handlers.App).CreateCatalog))
	g.GET("/api/catalogs/{id}", tenant((*handlers.App).GetCatalog))
	g.DELETE("/api/catalogs/{id}", tenant((*handlers.App).DeleteCatalog))
	g.POST("/api/catalogs/sync", tenant((*handlers.App).SyncCatalogs))

	// Catalog Products
	g.GET("/api/catalogs/{id}/products", tenant((*handlers.App).ListCatalogProducts))
	g.POST("/api/catalogs/{id}/products", tenant((*handlers.App).CreateCatalogProduct))
	g.GET("/api/products/{id}", tenant((*handlers.App).GetCatalogProduct))
	g.PUT("/api/products/{id}", tenant((*handlers.App).UpdateCatalogProduct))
	g.DELETE("/api/products/{id}", tenant((*handlers.App).DeleteCatalogProduct))

	// Serve embedded frontend (SPA)
	if frontend.IsEmbedded() {
		lo.Info("Serving embedded frontend", "base_path", basePath)
		frontendHandler := frontend.Handler(basePath)
		// Catch-all for frontend routes
		g.GET("/{path:*}", func(r *fastglue.Request) error {
			frontendHandler(r.RequestCtx)
			return nil
		})
		g.GET("/", func(r *fastglue.Request) error {
			frontendHandler(r.RequestCtx)
			return nil
		})
	} else {
		lo.Info("Frontend not embedded, API-only mode")
	}
}

// withRateLimit wraps a handler with the rate limit middleware.
func withRateLimit(handler fastglue.FastRequestHandler, opts middleware.RateLimitOpts) fastglue.FastRequestHandler {
	rl := middleware.RateLimit(opts)
	return func(r *fastglue.Request) error {
		if rl(r) == nil {
			return nil // Rate limited — response already sent.
		}
		return handler(r)
	}
}

// corsWrapper wraps a handler with CORS support at the fasthttp level.
// This ensures CORS headers are set even for auto-handled OPTIONS requests.
func corsWrapper(next fasthttp.RequestHandler, allowedOrigins map[string]bool) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		origin := string(ctx.Request.Header.Peek("Origin"))

		if origin != "" && middleware.IsOriginAllowed(origin, allowedOrigins) {
			ctx.Response.Header.Set("Access-Control-Allow-Origin", origin)
			ctx.Response.Header.Set("Access-Control-Allow-Credentials", "true")
		} else if len(allowedOrigins) == 0 && origin != "" {
			// Development: no whitelist configured
			ctx.Response.Header.Set("Access-Control-Allow-Origin", origin)
		}

		ctx.Response.Header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
		ctx.Response.Header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key, X-Organization-ID, X-CSRF-Token")
		ctx.Response.Header.Set("Access-Control-Max-Age", "86400")

		// Handle preflight OPTIONS requests
		if string(ctx.Method()) == "OPTIONS" {
			ctx.SetStatusCode(fasthttp.StatusNoContent)
			return
		}

		next(ctx)
	}
}
