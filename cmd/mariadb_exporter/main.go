// Command mariadb_exporter exposes metrics of MariaDB instances in
// Prometheus format, including the plugins and MariaDB-exclusive features
// that mysqld_exporter does not cover.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"
	toolkitweb "github.com/prometheus/exporter-toolkit/web"

	"github.com/Kevenny/mariadb-exporter/collector"
	"github.com/Kevenny/mariadb-exporter/config"
	"github.com/Kevenny/mariadb-exporter/exporter"
	"github.com/Kevenny/mariadb-exporter/web"
)

// Filled in via -ldflags at build time (see Makefile).
var (
	version   = "dev"
	buildDate = "unknown"
)

// shutdownTimeout limits the graceful shutdown of the HTTP server.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when the failure is a configuration
		// one, so the fatal error goes to stderr.
		fmt.Fprintf(os.Stderr, "fatal error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	app := kingpin.New("mariadb_exporter", "Prometheus exporter dedicated to MariaDB.")
	app.HelpFlag.Short('h')
	app.Version(fmt.Sprintf("mariadb_exporter %s (build %s, %s)", version, buildDate, runtime.Version()))

	cfg := config.Register(app)

	app.Command("serve", "Run the exporter (default).").Default()
	hc := app.Command("healthcheck", "Probe a running exporter's /health; exits 0 when healthy. "+
		"Meant for Docker HEALTHCHECK, since the image has no shell or curl.")
	hcURL := hc.Flag("url", "Health URL; credentials for basic auth go in the URL (https://user:pass@host/health).").
		Envar("MARIADB_HEALTHCHECK_URL").Default("http://127.0.0.1:9104/health").String()
	hcInsecure := hc.Flag("insecure-skip-verify", "Skip TLS certificate verification (self-signed exporter certificate).").Bool()
	hcTimeout := hc.Flag("timeout", "Request timeout.").Default("5s").Duration()

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		return err
	}
	if cmd == hc.FullCommand() {
		return healthcheck(*hcURL, *hcInsecure, *hcTimeout)
	}

	logger := newLogger(cfg.Log)

	if err := cfg.Validate(); err != nil {
		return err
	}

	driverCfg, err := cfg.DataSource.DriverConfig()
	if err != nil {
		return err
	}

	_ = level.Info(logger).Log(
		"msg", "starting mariadb_exporter",
		"version", version,
		"build_date", buildDate,
		"go", runtime.Version(),
		"dsn", config.Redacted(driverCfg),
	)

	db, err := openDB(driverCfg, cfg.DataSource)
	if err != nil {
		return err
	}
	defer db.Close()

	// Root context canceled on the termination signal, used by the feature refresh.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Version detection: an instance that isn't MariaDB is a fatal startup
	// error (sections 2.1 and 17).
	detectCtx, cancelDetect := context.WithTimeout(ctx, cfg.DataSource.Timeout)
	defer cancelDetect()

	versionInfo, err := exporter.DetectVersion(detectCtx, db)
	if err != nil {
		if errors.Is(err, exporter.ErrNotMariaDB) {
			_ = level.Error(logger).Log("msg", "incompatible instance", "err", err)
		}
		return err
	}

	_ = level.Info(logger).Log(
		"msg", "MariaDB detected",
		"version", versionInfo.String(),
		"version_full", versionInfo.Full,
		"version_comment", versionInfo.Comment,
	)

	detector := exporter.NewFeatureDetector(db, versionInfo, logger)
	// First synchronous refresh: the collectors already see the features on
	// the initial scrape instead of reporting unavailability on the first
	// /metrics.
	detector.Refresh(detectCtx)
	go detector.Run(ctx, exporter.FeatureRefreshInterval)

	collectors, err := buildCollectors(cfg, logger, detector)
	if err != nil {
		return err
	}
	_ = level.Info(logger).Log("msg", "collectors enabled", "list", fmt.Sprint(enabledNames(collectors)))

	exp := exporter.New(db, collectors, detector, cfg.PMM, logger)

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		exporter.BuildInfoCollector(version, buildDate, runtime.Version(), cfg.PMM),
		promcollectors.NewGoCollector(),
		promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{}),
	)

	handler := web.NewHandler(web.Options{
		TelemetryPath: cfg.Web.TelemetryPath,
		MaxRequests:   cfg.Web.MaxRequests,
		Registry:      registry,
		Scrape:        exp.WithContext,
		Pinger:        exp,
		Version:       version,
		Logger:        logger,
	})

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Above the 30s scrape cap, so only a client that stops reading the
		// response is cut off — it would otherwise hold a --web.max-requests slot.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		_ = level.Info(logger).Log(
			"msg", "HTTP server listening",
			"addresses", fmt.Sprint(cfg.Web.ListenAddresses()),
			"telemetry_path", cfg.Web.TelemetryPath,
			"web_config_file", cfg.Web.WebConfigFile(),
		)
		// The toolkit handles the address, TLS, and authentication based on
		// --web.config.file; without that file, the behavior is plain HTTP,
		// identical to before.
		if err := toolkitweb.ListenAndServe(server, cfg.Web.ToolkitFlags, logger); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		_ = level.Info(logger).Log("msg", "signal received, shutting down")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("failed to shut down the HTTP server: %w", err)
	}

	return nil
}

// openDB opens the connection pool and validates connectivity immediately,
// so that a wrong DSN fails at startup and not on the first scrape.
func openDB(driverCfg *mysql.Config, ds config.DataSource) (*sql.DB, error) {
	connector, err := mysql.NewConnector(driverCfg)
	if err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}
	db := sql.OpenDB(connector)

	db.SetMaxOpenConns(ds.MaxOpen)
	db.SetMaxIdleConns(ds.MaxIdle)
	// Recycling connections avoids accumulating dead sessions when the
	// server's wait_timeout is shorter than the interval between scrapes.
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), ds.Timeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to MariaDB: %w", err)
	}

	return db, nil
}

// buildCollectors instantiates the collectors according to the flags.
func buildCollectors(cfg *config.Config, logger log.Logger, detector *exporter.FeatureDetector) ([]collector.Collector, error) {
	registry := collector.NewRegistry()

	registry.Register(
		collector.NewInfoCollector(logger, detector, cfg.PMM.ConstLabels()),
		collector.NewGlobalStatusCollector(cfg.Collectors.GlobalStatus, logger, detector),
		collector.NewGlobalVariablesCollector(cfg.Collectors.GlobalVariables, logger, detector),
		collector.NewUserStatCollector(cfg.Collectors.UserStat, logger, detector),
		collector.NewTableStatCollector(cfg.Collectors.TableStat, cfg.Collectors.TableStatLimit, logger, detector),
		collector.NewIndexStatCollector(cfg.Collectors.IndexStat, cfg.Collectors.IndexStatLimit, logger, detector),
		collector.NewClientStatCollector(cfg.Collectors.ClientStat, logger, detector),
		collector.NewQueryResponseTimeCollector(cfg.Collectors.QueryResponseTime, logger, detector),
		collector.NewMetadataLocksCollector(cfg.Collectors.MetadataLocks, logger, detector),
		collector.NewDisksCollector(cfg.Collectors.Disks, logger, detector),
		collector.NewReplicationCollector(cfg.Collectors.Replication, logger, detector),
		collector.NewInnoDBCollector(cfg.Collectors.InnoDB, logger, detector),
		collector.NewGaleraCollector(cfg.Collectors.Galera, logger, detector),
	)

	if len(cfg.CustomMetrics) > 0 {
		custom, err := collector.NewCustomMetricsCollector(cfg.CustomMetrics, logger, detector, cfg.PMM.ConstLabels())
		if err != nil {
			return nil, err
		}
		custom.SetMaxRows(cfg.CustomMetricsMaxRows)
		registry.Register(custom)
		_ = level.Info(logger).Log("msg", "custom metrics loaded", "files", fmt.Sprint(cfg.CustomMetrics))
	}

	return registry.All(), nil
}

func enabledNames(cs []collector.Collector) []string {
	var out []string
	for _, c := range cs {
		if c.Enabled() {
			out = append(out, c.Name())
		}
	}
	return out
}

// newLogger builds the logger according to --log.format and --log.level.
func newLogger(cfg config.Log) log.Logger {
	var logger log.Logger
	if cfg.Format == "json" {
		logger = log.NewJSONLogger(log.NewSyncWriter(os.Stderr))
	} else {
		logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	}

	var opt level.Option
	switch cfg.Level {
	case "debug":
		opt = level.AllowDebug()
	case "warn":
		opt = level.AllowWarn()
	case "error":
		opt = level.AllowError()
	default:
		opt = level.AllowInfo()
	}

	logger = level.NewFilter(logger, opt)
	return log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.Caller(4))
}
