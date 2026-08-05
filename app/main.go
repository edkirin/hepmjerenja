package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	pinTimezone()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "fetch":
			runFetch(os.Args[2:])
			return
		case "migrate":
			runMigrate(os.Args[2:])
			return
		case "help", "--help", "-h":
			fmt.Println("Usage: hepmjerenja [command]")
			fmt.Println()
			fmt.Println("Commands:")
			fmt.Println("  fetch [yyyy-mm]     Schedule re-fetch of meter readings.")
			fmt.Println("                      Month defaults to previous month.")
			fmt.Println("  migrate [up|down|status]")
			fmt.Println("                      Apply, roll back or list database migrations.")
			fmt.Println("                      Defaults to up; the server also migrates on startup.")
			fmt.Println("  help                Show this help message")
			fmt.Println()
			fmt.Println("Run without arguments to start the HTTP server.")
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n\nRun 'hepmjerenja help' for usage.\n", os.Args[1])
			os.Exit(1)
		}
	}

	// Load configuration from environment variables (and the settings file if present).
	cfg := LoadConfig()

	// Initialize structured loggers: main app and background worker write to separate files.
	logger := NewLogger(cfg.LogDir, cfg.LogLevel, "app.log")
	workerLogger := NewLogger(cfg.LogDir, cfg.LogLevel, "worker.log")

	switch {
	case cfg.ConfigFileError != nil:
		logger.Error().Err(cfg.ConfigFileError).Msg("Settings file could not be parsed; using environment variables only")
	case cfg.ConfigFile != "":
		logger.Info().Str("file", cfg.ConfigFile).Msg("Loaded settings file")
	default:
		logger.Info().Strs("looked_for", configFiles).Msg("No settings file found; using environment variables only")
	}

	// Create a root context that is automatically cancelled when the process
	// receives SIGINT (Ctrl+C) or SIGTERM (e.g. from Docker/systemd).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Make sure the database directory exists before SQLite tries to create the
	// file inside it.
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		logger.Fatal().Err(err).Str("path", cfg.DBPath).Msg("Failed to create database directory")
	}

	// Open the SQLite database.
	db, err := NewDB(ctx, cfg.DSN(), cfg.SQLEcho)
	if err != nil {
		logger.Fatal().Err(err).Str("path", cfg.DBPath).Msg("Failed to open database")
	}
	defer db.Close()

	if err := RunMigrations(cfg.DSN(), logger); err != nil {
		logger.Fatal().Err(err).Msg("Database migration failed")
	}

	// Create the HEP API client used by the background worker.
	hepClient := NewHepClient(logger)

	creds := cfg.HepCredentials()
	if !creds.IsSet() {
		logger.Warn().Msg("HEP credentials are not configured; set HEP_USERNAME and HEP_PASSWORD to collect data")
	}

	// manualFetchCh carries one-off collection requests from HTTP handlers.
	manualFetchCh := make(chan ManualFetchRequest, 5)

	// One token cache for both workers: HEP allows a single active session per
	// account, so two independent logins would invalidate each other.
	tokens := newTokenCache()

	// The manual-fetch consumer always runs so the "Osvježi podatke" button works
	// even when the periodic background worker is disabled (the worker only handles
	// scheduled collection).
	go StartManualFetchWorker(ctx, db, hepClient, tokens, creds, workerLogger, manualFetchCh)

	// Start the periodic background worker in a separate goroutine.
	go StartWorker(ctx, db, hepClient, tokens, creds, workerLogger)

	// Handler with shared dependencies.
	h := &Handler{
		db:             db,
		logger:         logger,
		hepClient:      hepClient,
		hepCredentials: creds,
		tokens:         tokens,
		manualFetchCh:  manualFetchCh,
	}

	// Set up the Echo web framework.
	e := echo.New()
	e.HideBanner = true
	if !cfg.Debug {
		e.Use(newMinifyMiddleware())
	}
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true,
		LogURI:    true,
		LogMethod: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			logger.Info().
				Str("method", v.Method).
				Str("uri", v.URI).
				Int("status", v.Status).
				Msg("request")
			return nil
		},
	}))
	e.Use(middleware.Recover())

	// Static assets (embedded into the binary).
	registerStaticRoutes(e)

	// Views.
	e.GET("/", func(c echo.Context) error {
		return c.Redirect(http.StatusFound, "/mjesecno")
	})
	e.GET("/mjesecno", h.handleIndex)
	e.GET("/godisnje", h.handleYearlyView)
	e.GET("/postavke", h.handleSettings)
	e.POST("/postavke/mjerno-mjesto/:code/tarifa", h.handleUpdateMeteringPointTariff)

	// JSON API.
	e.GET("/api/readings", h.handleAPIReadings)
	e.GET("/api/readings/year", h.handleAPIReadingsYear)
	e.GET("/api/readings/hourly", h.handleAPIReadingsHourly)
	e.GET("/api/readings/calendar", h.handleAPIReadingsCalendar)
	e.GET("/api/insolation", h.handleAPIInsolation)
	e.GET("/api/insolation/year", h.handleAPIInsolationYear)
	e.POST("/api/hep-test", h.handleHepTest)
	e.POST("/api/fetch/now", h.handleForceFetch)

	// Start the HTTP server in a goroutine.
	go func() {
		logger.Info().Str("port", cfg.Port).Msg("Starting server")
		if err := e.Start(cfg.Port); err != nil {
			logger.Info().Err(err).Msg("Server stopped")
		}
	}()

	// Block until a shutdown signal is received.
	<-ctx.Done()
	logger.Info().Msg("Shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := e.Shutdown(shutdownCtx); err != nil {
		logger.Error().Err(err).Msg("Echo shutdown error")
	}

	logger.Info().Msg("Application terminated")
}

func runFetch(args []string) {
	var month time.Time

	switch len(args) {
	case 0:
		month = time.Now().AddDate(0, 0, -1)
	case 1:
		t, err := time.Parse("2006-01", args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid month %q — expected yyyy-mm\n", args[0])
			os.Exit(1)
		}
		month = t
	default:
		fmt.Fprintln(os.Stderr, "usage: hepmjerenja fetch [yyyy-mm]")
		os.Exit(1)
	}

	monthStr := month.Format("2006-01")
	fmt.Printf("Schedule re-fetch of %s for all metering points? [y/N] ", monthStr)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	if strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
		fmt.Println("Aborted.")
		return
	}

	cfg := LoadConfig()
	ctx := context.Background()
	db, err := NewDB(ctx, cfg.DSN(), false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	n, err := ScheduleForceFetch(ctx, db, month)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Scheduled re-fetch for %d metering point(s). Worker will pick them up on the next tick.\n", n)
}

// runMigrate applies, rolls back or lists database migrations from the command
// line, using the same embedded migration files the server applies on startup.
func runMigrate(args []string) {
	action := "up"
	if len(args) > 0 {
		action = args[0]
	}
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: hepmjerenja migrate [up|down|status]")
		os.Exit(1)
	}

	cfg := LoadConfig()
	logger := NewLogger(cfg.LogDir, cfg.LogLevel, "app.log")

	if err := Migrate(cfg.DSN(), action, logger); err != nil {
		fmt.Fprintf(os.Stderr, "migrate %s: %v\n", action, err)
		os.Exit(1)
	}
}

// pinTimezone forces the process into AppTimezone, for both Go and SQLite.
//
// Every date bucket in the SQL layer comes from SQLite's 'localtime' modifier,
// which reads the TZ environment variable — on a host running in UTC every daily
// total and every VT/NT tariff split would silently shift. Setting TZ here rather
// than relying on the environment makes the application correct wherever it runs;
// time.Local is aligned for the Go side.
func pinTimezone() {
	os.Setenv("TZ", AppTimezone)
	loc, err := time.LoadLocation(AppTimezone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load timezone %s: %v\n", AppTimezone, err)
		os.Exit(1)
	}
	time.Local = loc
}
