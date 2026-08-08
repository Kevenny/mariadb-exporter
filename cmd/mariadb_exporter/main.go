// Command mariadb_exporter expõe métricas de instâncias MariaDB no formato
// Prometheus, incluindo os plugins e recursos exclusivos do MariaDB que o
// mysqld_exporter não cobre.
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
	_ "github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"
	toolkitweb "github.com/prometheus/exporter-toolkit/web"

	"github.com/Kevenny/mariadb-exporter/collector"
	"github.com/Kevenny/mariadb-exporter/config"
	"github.com/Kevenny/mariadb-exporter/exporter"
	"github.com/Kevenny/mariadb-exporter/web"
)

// Preenchidos via -ldflags no build (ver Makefile).
var (
	version   = "dev"
	buildDate = "unknown"
)

// shutdownTimeout limita o encerramento gracioso do servidor HTTP.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		// O logger pode não existir ainda quando a falha é de configuração, por
		// isso o erro fatal vai para stderr.
		fmt.Fprintf(os.Stderr, "erro fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	app := kingpin.New("mariadb_exporter", "Exporter Prometheus dedicado ao MariaDB.")
	app.HelpFlag.Short('h')
	app.Version(fmt.Sprintf("mariadb_exporter %s (build %s, %s)", version, buildDate, runtime.Version()))

	cfg := config.Register(app)

	if _, err := app.Parse(os.Args[1:]); err != nil {
		return err
	}

	logger := newLogger(cfg.Log)

	if err := cfg.Validate(); err != nil {
		return err
	}

	dsn, err := config.NormalizeDSN(cfg.DataSource.Name)
	if err != nil {
		return err
	}

	_ = level.Info(logger).Log(
		"msg", "iniciando mariadb_exporter",
		"version", version,
		"build_date", buildDate,
		"go", runtime.Version(),
		"dsn", config.RedactDSN(cfg.DataSource.Name),
	)

	db, err := openDB(dsn, cfg.DataSource)
	if err != nil {
		return err
	}
	defer db.Close()

	// Contexto raiz cancelado no sinal de término, usado pelo refresh de features.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Detecção de versão: instância que não é MariaDB é erro fatal de startup
	// (seções 2.1 e 17).
	detectCtx, cancelDetect := context.WithTimeout(ctx, cfg.DataSource.Timeout)
	defer cancelDetect()

	versionInfo, err := exporter.DetectVersion(detectCtx, db)
	if err != nil {
		if errors.Is(err, exporter.ErrNotMariaDB) {
			_ = level.Error(logger).Log("msg", "instância incompatível", "err", err)
		}
		return err
	}

	_ = level.Info(logger).Log(
		"msg", "MariaDB detectado",
		"version", versionInfo.String(),
		"version_full", versionInfo.Full,
		"version_comment", versionInfo.Comment,
	)

	detector := exporter.NewFeatureDetector(db, versionInfo, logger)
	// Primeiro refresh sincrono: os coletores já veem as features no scrape
	// inicial em vez de reportarem indisponibilidade no primeiro /metrics.
	detector.Refresh(detectCtx)
	go detector.Run(ctx, exporter.FeatureRefreshInterval)

	collectors, err := buildCollectors(cfg, logger, detector)
	if err != nil {
		return err
	}
	_ = level.Info(logger).Log("msg", "coletores habilitados", "lista", fmt.Sprint(enabledNames(collectors)))

	exp := exporter.New(db, collectors, detector, cfg.PMM, logger)

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		exp,
		exporter.BuildInfoCollector(version, buildDate, runtime.Version(), cfg.PMM),
		promcollectors.NewGoCollector(),
		promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{}),
	)

	handler := web.NewHandler(web.Options{
		TelemetryPath: cfg.Web.TelemetryPath,
		MaxRequests:   cfg.Web.MaxRequests,
		Registry:      registry,
		Pinger:        exp,
		Version:       version,
		Logger:        logger,
	})

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		_ = level.Info(logger).Log(
			"msg", "servidor HTTP escutando",
			"addresses", fmt.Sprint(cfg.Web.ListenAddresses()),
			"telemetry_path", cfg.Web.TelemetryPath,
			"web_config_file", cfg.Web.WebConfigFile(),
		)
		// O toolkit cuida do endereço, do TLS e da autenticação a partir de
		// --web.config.file; sem esse arquivo, o comportamento é HTTP simples,
		// idêntico ao anterior.
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
		_ = level.Info(logger).Log("msg", "sinal recebido, encerrando")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("falha no shutdown do servidor HTTP: %w", err)
	}

	return nil
}

// openDB abre o pool de conexões e valida a conectividade imediatamente, para
// que um DSN errado falhe no startup e não no primeiro scrape.
func openDB(dsn string, ds config.DataSource) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("falha ao abrir conexão: %w", err)
	}

	db.SetMaxOpenConns(ds.MaxOpen)
	db.SetMaxIdleConns(ds.MaxIdle)
	// Reciclar conexões evita acumular sessões mortas quando o wait_timeout do
	// servidor é menor que o intervalo entre scrapes.
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), ds.Timeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("falha ao conectar no MariaDB: %w", err)
	}

	return db, nil
}

// buildCollectors instancia os coletores conforme as flags.
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
		custom, err := collector.NewCustomMetricsCollector(cfg.CustomMetrics, logger, detector)
		if err != nil {
			return nil, err
		}
		registry.Register(custom)
		_ = level.Info(logger).Log("msg", "custom metrics carregadas", "arquivos", fmt.Sprint(cfg.CustomMetrics))
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

// newLogger monta o logger conforme --log.format e --log.level.
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
