package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/assignment"
	"github.com/shridarpatil/whatomate/internal/calling"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/demodata"
	"github.com/shridarpatil/whatomate/internal/frontend"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/metareview"
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
	case "seed-klinik-relive-sales":
		runKlinikReliveSalesSeed(os.Args[2:])
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
  seed-klinik-relive-sales  Safely validate or apply Klinik Relive sales fixtures
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
  rereply seed-klinik-relive-sales -organization-id c73f761f-5154-4fe1-9a13-06bae570277a -organization-name "Klinik Relive"
  rereply seed-klinik-relive-sales -organization-id c73f761f-5154-4fe1-9a13-06bae570277a -organization-name "Klinik Relive" -apply

Deployment Scenarios:
  All-in-one:    rereply server
  Separate:      rereply server -workers 0  (on API server)
                 rereply worker -workers 4  (on worker server)`)
}

// ============================================================================
// KLINIK RELIVE SALES FIXTURE COMMAND
// ============================================================================

func runKlinikReliveSalesSeed(args []string) {
	seedFlags := flag.NewFlagSet("seed-klinik-relive-sales", flag.ExitOnError)
	configPath := seedFlags.String("config", "config.toml", "Path to config file")
	organizationIDText := seedFlags.String("organization-id", "", "Required exact target organization UUID")
	organizationName := seedFlags.String("organization-name", "", "Required exact target organization name")
	apply := seedFlags.Bool("apply", false, "Commit fixtures; without this flag the transaction is rolled back")
	_ = seedFlags.Parse(args)

	if *organizationIDText == "" || *organizationName == "" {
		fmt.Fprintln(os.Stderr, "Both -organization-id and -organization-name are required; no database changes were made.")
		os.Exit(1)
	}
	organizationID, err := uuid.Parse(*organizationIDText)
	if err != nil {
		fmt.Fprintln(os.Stderr, "The supplied -organization-id is not a valid UUID; no database changes were made.")
		os.Exit(1)
	}
	if organizationID.String() != demodata.KlinikReliveOrganizationID ||
		*organizationName != demodata.KlinikReliveOrganizationName {
		fmt.Fprintln(os.Stderr, "The supplied organization does not exactly match the Klinik Relive fixture target; no database changes were made.")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to load the fixture command configuration; no database changes were made.")
		os.Exit(1)
	}
	if cfg.Database.MigrationURL == "" {
		fmt.Fprintln(os.Stderr, "database.migration_url is required; no database changes were made.")
		os.Exit(1)
	}

	migrationCfg := cfg.Database
	migrationCfg.URL = cfg.Database.MigrationURL
	db, err := database.NewPostgres(&migrationCfg, false)
	if err != nil {
		// Do not include the driver error: connection failures can echo a DSN.
		fmt.Fprintln(os.Stderr, "Failed to connect using database.migration_url; no database changes were made.")
		os.Exit(1)
	}
	sqlDB, err := db.DB()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to initialize the migration database connection; no database changes were made.")
		os.Exit(1)
	}
	defer func() {
		_ = sqlDB.Close()
	}()

	report, err := demodata.SeedKlinikReliveSales(context.Background(), db, demodata.SeedOptions{
		OrganizationID:   organizationID,
		OrganizationName: *organizationName,
		Apply:            *apply,
		Now:              time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Klinik Relive sales fixture validation failed: %v\n", err)
		os.Exit(1)
	}

	mode := "DRY RUN (transaction rolled back)"
	if report.Applied {
		mode = "APPLIED"
	}
	fmt.Printf(
		"Klinik Relive sales fixtures %s: canned=%d ivr=%d calls=%d transfers=%d meta_snapshots=%d\n",
		mode,
		report.CannedResponses,
		report.IVRFlows,
		report.CallLogs,
		report.CallTransfers,
		report.MetaSnapshots,
	)
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
	legacyMetaStats, err := channelapi.BackfillLegacyWhatsAppInbox(db, 500)
	if err != nil {
		lo.Fatal("Legacy WhatsApp omnichannel backfill failed", "error", err)
	}
	lo.Info(
		"Legacy WhatsApp omnichannel backfill complete",
		"accounts",
		legacyMetaStats.Accounts,
		"messages",
		legacyMetaStats.Messages,
		"linked",
		legacyMetaStats.Linked,
	)
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
		legacyMetaStats, err := channelapi.BackfillLegacyWhatsAppInbox(db, 500)
		if err != nil {
			lo.Fatal("Legacy WhatsApp omnichannel backfill failed", "error", err)
		}
		lo.Info(
			"Legacy WhatsApp omnichannel backfill complete",
			"accounts",
			legacyMetaStats.Accounts,
			"messages",
			legacyMetaStats.Messages,
			"linked",
			legacyMetaStats.Linked,
		)
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
	app.CallManager = calling.NewManager(&cfg.Calling, s3Client, db, rdb, waClient, wsHub, assigner, httpClient, cfg.App.EncryptionKey, lo)
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
	if err := validateRouteContextKeyIsolation(g.Router.List()); err != nil {
		lo.Fatal("Route parameter conflicts with request context", "error", err)
	}

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

	// Start the durable care-continuity processor. It creates internal tasks
	// and agent reminders only; it never sends customer messages.
	careProcessor := handlers.NewCareContinuityProcessor(app, 30*time.Second)
	careCtx, careCancel := context.WithCancel(context.Background())
	go careProcessor.Start(careCtx)
	lo.Info("Care continuity processor started")

	// Enforce Copilot data retention independently of HTTP traffic. Expired
	// output is hidden by handlers immediately and hard-purged here, including
	// linked feedback, in tenant-scoped batches.
	copilotRetentionProcessor := handlers.NewCopilotRetentionProcessor(app, time.Hour)
	copilotRetentionCtx, copilotRetentionCancel := context.WithCancel(context.Background())
	go copilotRetentionProcessor.Start(copilotRetentionCtx)
	lo.Info("Copilot retention processor started")

	// Execute immutable visual automation versions from the dedicated customer
	// activity receipt stream. Multi-replica leases keep this safe when more
	// than one API instance is running.
	automationProcessor := handlers.NewAutomationPolicyProcessor(app, 5*time.Second)
	automationCtx, automationCancel := context.WithCancel(context.Background())
	go automationProcessor.Start(automationCtx)
	lo.Info("Automation policy processor started")

	// Recover the post-ACK media/chatbot side of inbound WhatsApp messages.
	// The durable scheduled-job lease prevents duplicate replicas from
	// processing the same WAMID concurrently.
	inboundProcessor := handlers.NewInboundContinuationProcessor(app, 2*time.Second)
	inboundCtx, inboundCancel := context.WithCancel(context.Background())
	go inboundProcessor.Start(inboundCtx)
	lo.Info("Inbound continuation processor started")

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
			go func() {
				lo.Info("Channel outbox worker started", "worker_num", workerNum)
				if err := w.RunChannelOutbox(workerCtx); err != nil && err != context.Canceled {
					lo.Error("Channel outbox worker error", "error", err, "worker_num", workerNum)
				}
			}()
			go func() {
				lo.Info("Channel AI reply worker started", "worker_num", workerNum)
				if err := w.RunChannelAIReplies(workerCtx); err != nil && err != context.Canceled {
					lo.Error("Channel AI reply worker error", "error", err, "worker_num", workerNum)
				}
			}()
			go func() {
				lo.Info("Threads credential refresh worker started", "worker_num", workerNum)
				if err := w.RunThreadsCredentialRefresh(workerCtx); err != nil && err != context.Canceled {
					lo.Error("Threads credential refresh worker error", "error", err, "worker_num", workerNum)
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

	// Stop inbound continuation recovery.
	lo.Info("Stopping inbound continuation processor...")
	inboundCancel()
	inboundProcessor.Stop()
	lo.Info("Inbound continuation processor stopped")

	// Stop visual automation before legacy care so neither can claim new
	// customer lifecycle work during shutdown.
	lo.Info("Stopping automation policy processor...")
	automationCancel()
	automationProcessor.Stop()
	lo.Info("Automation policy processor stopped")

	// Stop Copilot retention before the shared database pool is closed.
	lo.Info("Stopping Copilot retention processor...")
	copilotRetentionCancel()
	copilotRetentionProcessor.Stop()
	lo.Info("Copilot retention processor stopped")

	// Stop care continuity processor
	lo.Info("Stopping care continuity processor...")
	careCancel()
	careProcessor.Stop()
	lo.Info("Care continuity processor stopped")

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
	errCh := make(chan error, *workerCount*4)

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
		go func(workerNum int) {
			lo.Info("Channel outbox worker started", "worker_num", workerNum)
			errCh <- w.RunChannelOutbox(ctx)
		}(i + 1)
		go func(workerNum int) {
			lo.Info("Channel AI reply worker started", "worker_num", workerNum)
			errCh <- w.RunChannelAIReplies(ctx)
		}(i + 1)
		go func(workerNum int) {
			lo.Info("Threads credential refresh worker started", "worker_num", workerNum)
			errCh <- w.RunThreadsCredentialRefresh(ctx)
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
	if cfg.RateLimit.Enabled {
		window := time.Duration(cfg.RateLimit.WindowSeconds) * time.Second
		g.GET("/api/auth/sso/providers", withRateLimit(app.GetPublicSSOProviders, middleware.RateLimitOpts{
			Redis: rdb, Log: lo, Max: cfg.RateLimit.SSOMaxAttempts, Window: window, KeyPrefix: "sso_discovery", TrustProxy: cfg.RateLimit.TrustProxy,
		}))
		g.GET("/api/auth/sso/{provider}/init", withRateLimit(app.InitSSO, middleware.RateLimitOpts{
			Redis: rdb, Log: lo, Max: cfg.RateLimit.SSOMaxAttempts, Window: window, KeyPrefix: "sso_init", TrustProxy: cfg.RateLimit.TrustProxy,
		}))
		g.GET("/api/auth/sso/{provider}/callback", withRateLimit(app.CallbackSSO, middleware.RateLimitOpts{
			Redis: rdb, Log: lo, Max: cfg.RateLimit.SSOMaxAttempts, Window: window, KeyPrefix: "sso_callback", TrustProxy: cfg.RateLimit.TrustProxy,
		}))
	} else {
		g.GET("/api/auth/sso/providers", app.GetPublicSSOProviders)
		g.GET("/api/auth/sso/{provider}/init", app.InitSSO)
		g.GET("/api/auth/sso/{provider}/callback", app.CallbackSSO)
	}

	// Public OAuth callback. Tenant and initiating admin identities are carried
	// only in a one-time Redis state and are re-authorized by the handler.
	g.GET("/api/integrations/google_search_console/callback", app.CallbackGoogleSearchConsole)
	g.GET("/api/integrations/threads/callback", app.CallbackThreads)
	g.POST(
		"/api/integrations/threads/{target_organization_id}/deauthorize",
		app.DeauthorizeThreads,
	)
	g.POST(
		"/api/integrations/threads/{target_organization_id}/data-deletion",
		app.DeleteThreadsUserData,
	)
	g.GET(
		"/api/integrations/threads/{target_organization_id}/data-deletion/status/{confirmation_code}",
		app.ThreadsDataDeletionStatus,
	)

	// Webhook routes (public - for Meta)
	g.GET("/api/webhook", app.WebhookVerify)
	g.POST("/api/webhook", app.WebhookHandler)
	g.POST(
		"/api/webhooks/channels/{channel_account_id}",
		app.RelayChannelWebhook,
	)
	// Staging review relay credential broker. Authentication is a dedicated,
	// replay-protected server-to-server HMAC; browser JWTs are never accepted.
	g.POST(metareview.ProvisionPath, app.ProvisionMetaMessengerReviewCredential)
	g.GET(
		"/api/webhooks/channels/{channel_account_id}",
		app.VerifyChannelWebhook,
	)

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
		if strings.HasPrefix(path, "/api/webhooks/channels/") {
			return r
		}
		if path == metareview.ProvisionPath {
			return r
		}
		// Skip auth for SSO routes (they handle their own auth via state tokens)
		if len(path) >= 13 && path[:13] == "/api/auth/sso" {
			return r
		}
		if path == "/api/integrations/google_search_console/callback" ||
			path == "/api/integrations/threads/callback" {
			return r
		}
		if isPublicThreadsLifecyclePath(path) {
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

	// Product catalog and manual licensing control plane. These handlers
	// authorize platform/reseller ownership before entering the target tenant.
	g.POST("/api/admin/product/plans", app.CreateProductPlan)
	g.PUT("/api/admin/product/plans/{id}", app.UpdateProductPlan)
	g.GET(
		"/api/admin/organizations/{target_organization_id}/product/plans",
		app.ListAssignableProductPlans,
	)
	g.GET(
		"/api/admin/organizations/{target_organization_id}/subscription",
		app.GetOrganizationSubscription,
	)
	g.PUT(
		"/api/admin/organizations/{target_organization_id}/subscription",
		app.SetOrganizationSubscription,
	)
	g.POST(
		"/api/admin/organizations/{target_organization_id}/entitlements/threads-public-engagement/enable",
		app.EnableOrganizationThreadsPublicEngagement,
	)
	g.GET(
		"/api/admin/organizations/{target_organization_id}/entitlements/threads-public-engagement/support-status",
		app.GetOrganizationThreadsPublicEngagementSupportStatus,
	)
	g.POST(
		"/api/admin/organizations/{target_organization_id}/entitlements/threads-public-engagement/revoke-support",
		app.RevokeOrganizationThreadsPublicEngagementSupport,
	)

	// Commercial, onboarding, privacy, and support tenant surfaces.
	g.GET("/api/product/plans", tenant((*handlers.App).ListProductPlans))
	g.GET("/api/product/subscription", tenant((*handlers.App).GetProductSubscription))
	g.GET("/api/product/entitlements", tenant((*handlers.App).GetProductEntitlements))
	g.GET("/api/onboarding", tenant((*handlers.App).GetOnboarding))
	g.PUT("/api/onboarding/profile", tenant((*handlers.App).UpdateOnboardingProfile))
	g.POST(
		"/api/onboarding/steps/{key}/complete",
		tenant((*handlers.App).CompleteOnboardingStep),
	)
	g.GET("/api/workspace-templates", tenant((*handlers.App).ListWorkspaceTemplates))
	g.POST(
		"/api/workspace-templates/{key}/apply",
		tenant((*handlers.App).ApplyWorkspaceTemplate),
	)
	g.GET("/api/privacy/settings", tenant((*handlers.App).GetPrivacySettings))
	g.PUT("/api/privacy/settings", tenant((*handlers.App).UpdatePrivacySettings))
	g.GET("/api/privacy/consents", tenant((*handlers.App).ListConsentStates))
	g.POST("/api/privacy/consents", tenant((*handlers.App).RecordConsent))
	g.GET("/api/privacy/requests", tenant((*handlers.App).ListPrivacyRequests))
	g.POST("/api/privacy/requests", tenant((*handlers.App).CreatePrivacyRequest))
	g.PUT("/api/privacy/requests/{id}", tenant((*handlers.App).UpdatePrivacyRequest))
	g.GET("/api/trust/summary", tenant((*handlers.App).GetTrustQueueSummary))
	g.GET("/api/support/health", tenant((*handlers.App).GetTenantSupportHealth))
	g.GET("/api/support/cases", tenant((*handlers.App).ListSupportCases))
	g.POST("/api/support/cases", tenant((*handlers.App).CreateSupportCase))
	g.PUT("/api/support/cases/{id}", tenant((*handlers.App).UpdateSupportCase))
	g.GET("/api/support/recovery", tenant((*handlers.App).GetRecoverySummary))

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

	// Admin Integration Center. Provider tests perform network I/O between two
	// short tenant phases and therefore must not hold the route tenant
	// transaction open while waiting on an external service.
	g.GET("/api/integrations", tenant((*handlers.App).GetIntegrations))
	g.PUT("/api/integrations/{provider}", tenant((*handlers.App).UpdateIntegration))
	g.DELETE("/api/integrations/{provider}/credentials", tenant((*handlers.App).DeleteIntegrationCredentials))
	g.POST("/api/integrations/{provider}/connect", tenant((*handlers.App).ConnectIntegration))
	g.POST("/api/integrations/{provider}/test", app.TestIntegration)
	// Facebook Login for Business returns the authorization code to the
	// authenticated browser SDK. All three managed Messenger onboarding phases
	// remain authenticated; there is deliberately no public OAuth callback.
	g.POST(
		"/api/integrations/meta/messenger/onboarding/start",
		app.StartMetaMessengerOnboarding,
	)
	g.POST(
		"/api/integrations/meta/messenger/onboarding/exchange",
		app.ExchangeMetaMessengerOnboarding,
	)
	g.POST(
		"/api/integrations/meta/messenger/onboarding/select",
		app.SelectMetaMessengerOnboarding,
	)
	g.DELETE(
		"/api/integrations/meta/messenger/review/{id}",
		app.DeprovisionMetaMessengerReviewAccount,
	)
	g.GET("/api/integrations/google_search_console/properties", tenant((*handlers.App).ListGoogleSearchConsoleProperties))
	g.PUT("/api/integrations/google_search_console/properties", tenant((*handlers.App).UpdateGoogleSearchConsoleProperties))
	g.POST("/api/integrations/google_search_console/properties/refresh", app.RefreshGoogleSearchConsoleProperties)
	g.DELETE("/api/integrations/google_search_console/connection", app.DisconnectGoogleSearchConsole)

	// Provider-neutral channel accounts and shared inbox.
	g.GET("/api/channel-accounts", tenant((*handlers.App).ListChannelAccounts))
	g.POST("/api/channel-accounts", tenant((*handlers.App).CreateChannelAccount))
	g.PUT("/api/channel-accounts/{id}", tenant((*handlers.App).UpdateChannelAccount))
	g.DELETE("/api/channel-accounts/{id}", tenant((*handlers.App).DeleteChannelAccount))
	// Health validation performs two short, independently committed tenant
	// phases around a provider network call. Do not wrap it in the
	// transaction-spanning Tenant adapter or the outer connection can deadlock
	// while the phases wait for another pool connection.
	g.POST("/api/channel-accounts/{id}/test", app.TestChannelAccount)
	// The staging App Review Page-post preview likewise performs one short
	// tenant read followed by a provider call. Its handler owns both phases and
	// fails closed unless the path names the deployment-pinned review account.
	g.GET("/api/channel-accounts/{id}/meta-page-posts", app.GetMetaMessengerReviewPagePosts)
	g.GET("/api/conversations", tenant((*handlers.App).ListInboxConversations))
	g.GET(
		"/api/conversations/{id}/messages",
		tenant((*handlers.App).GetInboxConversationMessages),
	)
	g.POST(
		"/api/conversations/{id}/messages",
		tenant((*handlers.App).SendInboxConversationMessage),
	)
	g.POST(
		"/api/conversations/{id}/read",
		tenant((*handlers.App).MarkInboxConversationRead),
	)
	g.PUT(
		"/api/conversations/{id}/ai",
		tenant((*handlers.App).SetInboxConversationAIState),
	)

	// Contacts
	g.GET("/api/contacts", tenant((*handlers.App).ListContacts))
	g.POST("/api/contacts", tenant((*handlers.App).CreateContact))
	g.GET("/api/contacts/{id}", tenant((*handlers.App).GetContact))
	g.GET("/api/contacts/{id}/workspace", tenant((*handlers.App).GetCustomerWorkspace))
	g.GET("/api/contacts/{id}/activities", tenant((*handlers.App).ListCustomerActivities))
	g.POST("/api/contacts/{id}/merge", tenant((*handlers.App).MergeContact))
	g.PUT("/api/contacts/{id}", tenant((*handlers.App).UpdateContact))
	g.DELETE("/api/contacts/{id}", tenant((*handlers.App).DeleteContact))
	g.PUT("/api/contacts/{id}/assign", tenant((*handlers.App).AssignContact))
	g.PUT("/api/contacts/{id}/tags", tenant((*handlers.App).UpdateContactTags))
	g.GET("/api/contacts/{id}/session-data", tenant((*handlers.App).GetContactSessionData))

	// CRM pipeline and follow-up operations
	g.GET("/api/crm/pipelines", tenant((*handlers.App).ListCRMPipelines))
	g.POST("/api/crm/pipelines", tenant((*handlers.App).CreateCRMPipeline))
	g.POST("/api/crm/pipelines/{pipeline_id}/stages", tenant((*handlers.App).CreateCRMPipelineStage))
	g.PUT("/api/crm/pipelines/{pipeline_id}/stages/{id}", tenant((*handlers.App).UpdateCRMPipelineStage))
	g.DELETE("/api/crm/pipelines/{pipeline_id}/stages/{id}", tenant((*handlers.App).DeleteCRMPipelineStage))
	g.GET("/api/crm/leads", tenant((*handlers.App).ListCRMLeads))
	g.POST("/api/crm/leads", tenant((*handlers.App).CreateCRMLead))
	g.GET("/api/crm/insights", tenant((*handlers.App).GetCRMInsights))
	g.GET("/api/crm/segments", tenant((*handlers.App).ListCRMSystemSegments))
	g.GET(
		"/api/crm/segments/{key}/contacts",
		tenant((*handlers.App).ListCRMSystemSegmentContacts),
	)
	g.GET("/api/crm/leads/{id}", tenant((*handlers.App).GetCRMLead))
	g.PUT("/api/crm/leads/{id}", tenant((*handlers.App).UpdateCRMLead))
	g.PUT("/api/crm/leads/{id}/move", tenant((*handlers.App).MoveCRMLead))
	g.PUT("/api/crm/leads/{id}/archive", tenant((*handlers.App).ArchiveCRMLead))
	g.PUT("/api/crm/leads/{id}/reopen", tenant((*handlers.App).ReopenCRMLead))
	g.GET("/api/automation-policies/catalog", tenant((*handlers.App).GetAutomationPolicyCatalog))
	g.GET("/api/automation-policies", tenant((*handlers.App).ListAutomationPolicies))
	g.POST("/api/automation-policies", tenant((*handlers.App).CreateAutomationPolicy))
	g.POST("/api/automation-policies/{id}/activate", tenant((*handlers.App).ActivateAutomationPolicy))
	g.POST("/api/automation-policies/{id}/pause", tenant((*handlers.App).PauseAutomationPolicy))
	g.POST("/api/automation-policies/{id}/preview", tenant((*handlers.App).PreviewAutomationPolicy))
	g.GET("/api/automation-policies/{id}/versions", tenant((*handlers.App).ListAutomationPolicyVersions))
	g.GET("/api/automation-policies/{id}/executions", tenant((*handlers.App).ListAutomationPolicyExecutions))
	g.GET("/api/automation-policies/{id}", tenant((*handlers.App).GetAutomationPolicy))
	g.PUT("/api/automation-policies/{id}", tenant((*handlers.App).UpdateAutomationPolicy))
	g.DELETE("/api/automation-policies/{id}", tenant((*handlers.App).DeleteAutomationPolicy))
	g.GET("/api/tasks", tenant((*handlers.App).ListFollowUpTasks))
	g.GET("/api/tasks/summary", tenant((*handlers.App).GetFollowUpTaskSummary))
	g.POST("/api/tasks", tenant((*handlers.App).CreateFollowUpTask))
	g.PUT("/api/tasks/{id}", tenant((*handlers.App).UpdateFollowUpTask))
	g.PUT("/api/tasks/{id}/complete", tenant((*handlers.App).CompleteFollowUpTask))
	g.PUT("/api/tasks/{id}/reopen", tenant((*handlers.App).ReopenFollowUpTask))

	// Scheduling, packages, invoices, and the append-only payment ledger.
	g.GET("/api/booking/services", tenant((*handlers.App).ListBookingServices))
	g.POST("/api/booking/services", tenant((*handlers.App).CreateBookingService))
	g.PUT("/api/booking/services/{id}", tenant((*handlers.App).UpdateBookingService))
	g.GET("/api/booking/resources", tenant((*handlers.App).ListBookingResources))
	g.POST("/api/booking/resources", tenant((*handlers.App).CreateBookingResource))
	g.PUT("/api/booking/resources/{id}", tenant((*handlers.App).UpdateBookingResource))
	g.GET(
		"/api/booking/resources/{resource_id}/availability-rules",
		tenant((*handlers.App).ListAvailabilityRules),
	)
	g.POST(
		"/api/booking/resources/{resource_id}/availability-rules",
		tenant((*handlers.App).CreateAvailabilityRule),
	)
	g.PUT(
		"/api/booking/resources/{resource_id}/availability-rules/{id}",
		tenant((*handlers.App).UpdateAvailabilityRule),
	)
	g.DELETE(
		"/api/booking/resources/{resource_id}/availability-rules/{id}",
		tenant((*handlers.App).DeleteAvailabilityRule),
	)
	g.GET(
		"/api/booking/resources/{resource_id}/time-off",
		tenant((*handlers.App).ListResourceTimeOff),
	)
	g.POST(
		"/api/booking/resources/{resource_id}/time-off",
		tenant((*handlers.App).CreateResourceTimeOff),
	)
	g.PUT(
		"/api/booking/resources/{resource_id}/time-off/{id}",
		tenant((*handlers.App).UpdateResourceTimeOff),
	)
	g.DELETE(
		"/api/booking/resources/{resource_id}/time-off/{id}",
		tenant((*handlers.App).DeleteResourceTimeOff),
	)
	g.GET("/api/booking/events", tenant((*handlers.App).ListBookingEvents))
	g.POST("/api/booking/events", tenant((*handlers.App).CreateBookingEvent))
	g.PUT("/api/booking/events/{id}", tenant((*handlers.App).UpdateBookingEvent))
	g.GET("/api/bookings", tenant((*handlers.App).ListBookings))
	g.POST(
		"/api/booking/events/{id}/bookings",
		tenant((*handlers.App).CreateBooking),
	)
	g.POST(
		"/api/bookings/{id}/{transition}",
		tenant((*handlers.App).TransitionBooking),
	)
	g.GET("/api/packages", tenant((*handlers.App).ListPackages))
	g.POST("/api/packages", tenant((*handlers.App).CreatePackage))
	g.PUT("/api/packages/{id}", tenant((*handlers.App).UpdatePackage))
	g.GET("/api/contact-packages", tenant((*handlers.App).ListContactPackages))
	g.POST("/api/contact-packages", tenant((*handlers.App).CreateContactPackage))
	g.POST("/api/package-sales", tenant((*handlers.App).SellContactPackage))
	g.GET("/api/invoices", tenant((*handlers.App).ListCommerceInvoices))
	g.POST("/api/invoices", tenant((*handlers.App).CreateCommerceInvoice))
	g.GET("/api/commerce/summary", tenant((*handlers.App).GetCommerceSummary))
	g.POST(
		"/api/invoices/{id}/payment-intents",
		tenant((*handlers.App).CreateInvoicePaymentIntent),
	)
	g.POST(
		"/api/invoices/{id}/manual-payments",
		tenant((*handlers.App).RecordManualInvoicePayment),
	)
	g.GET("/api/payments", tenant((*handlers.App).ListPaymentTransactions))

	// Human-in-the-loop Qwen Copilot. Generation never sends a message.
	g.POST("/api/contacts/{id}/copilot/{task}", tenant((*handlers.App).RunContactCopilot))
	g.GET("/api/copilot/runs", tenant((*handlers.App).ListCopilotRuns))
	g.POST("/api/copilot/runs/{id}/feedback", tenant((*handlers.App).CreateCopilotFeedback))

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
	// Search Console calls Google live and manages its own short tenant phases.
	g.GET("/api/analytics/search-visibility/setup", tenant((*handlers.App).GetGoogleSearchVisibilitySetup))
	g.GET("/api/analytics/search-visibility", app.GetGoogleSearchVisibility)

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
	g.DELETE("/api/organizations/{id}", app.DeleteOrganization)
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

func validateRouteContextKeyIsolation(routes map[string][]string) error {
	reserved := map[string]struct{}{
		middleware.ContextKeyUserID:          {},
		middleware.ContextKeyOrganizationID:  {},
		middleware.ContextKeyEmail:           {},
		middleware.ContextKeyRoleID:          {},
		middleware.ContextKeyIsSuperAdmin:    {},
		middleware.ContextKeyIsResellerAdmin: {},
		middleware.ContextKeyUser:            {},
		middleware.ContextKeyOrganization:    {},
		"request_start":                      {},
	}

	var collisions []string
	for method, paths := range routes {
		for _, path := range paths {
			remaining := path
			for {
				start := strings.IndexByte(remaining, '{')
				if start < 0 {
					break
				}
				remaining = remaining[start+1:]
				end := strings.IndexByte(remaining, '}')
				if end < 0 {
					break
				}
				parameter := strings.SplitN(remaining[:end], ":", 2)[0]
				if _, collides := reserved[parameter]; collides {
					collisions = append(collisions, method+" "+path+" {"+parameter+"}")
				}
				remaining = remaining[end+1:]
			}
		}
	}
	if len(collisions) == 0 {
		return nil
	}
	sort.Strings(collisions)
	return fmt.Errorf("route parameters reuse middleware context keys: %s", strings.Join(collisions, ", "))
}

func isPublicThreadsLifecyclePath(path string) bool {
	const prefix = "/api/integrations/threads/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 2 {
		return false
	}
	organizationID, err := uuid.Parse(parts[0])
	if err != nil || organizationID == uuid.Nil {
		return false
	}
	if len(parts) == 2 {
		return parts[1] == "deauthorize" || parts[1] == "data-deletion"
	}
	return len(parts) == 4 &&
		parts[1] == "data-deletion" &&
		parts[2] == "status" &&
		isPublicThreadsConfirmationCode(parts[3])
}

func isPublicThreadsConfirmationCode(value string) bool {
	if len(value) != len("THDEL")+32 || !strings.HasPrefix(value, "THDEL") {
		return false
	}
	for _, character := range value[len("THDEL"):] {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') &&
			(character < 'A' || character > 'F') {
			return false
		}
	}
	return true
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
