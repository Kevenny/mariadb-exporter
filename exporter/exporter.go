package exporter

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Kevenny/mariadb-exporter/collector"
)

// collectorTimeout é o teto de tempo por coletor (seção 5, item 3).
const collectorTimeout = 30 * time.Second

// Exporter implementa prometheus.Collector.
// Gerencia o pool de conexões e orquestra os coletores.
type Exporter struct {
	db         *sql.DB
	collectors []collector.Collector
	detector   *FeatureDetector
	logger     log.Logger

	scrapeDuration prometheus.Histogram
	scrapeSuccess  prometheus.Gauge
	scrapeErrors   *prometheus.CounterVec // label: collector
	up             prometheus.Gauge

	collectorDuration *prometheus.Desc
	collectorAvail    *prometheus.Desc
}

// New cria o exporter com os coletores informados.
func New(db *sql.DB, collectors []collector.Collector, detector *FeatureDetector, logger log.Logger) *Exporter {
	e := &Exporter{
		db:         db,
		collectors: collectors,
		detector:   detector,
		logger:     logger,

		scrapeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: collector.Namespace,
			Name:      "scrape_duration_seconds",
			Help:      "Duração total do scrape do mariadb_exporter em segundos.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}),
		scrapeSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: collector.Namespace,
			Name:      "scrape_success",
			Help:      "1 se o último scrape rodou sem erros em nenhum coletor, 0 caso contrário.",
		}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: collector.Namespace,
			Name:      "scrape_errors_total",
			Help:      "Total de erros de scrape por coletor.",
		}, []string{"collector"}),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: collector.Namespace,
			Name:      "up",
			Help:      "1 se o exporter está conectado ao MariaDB, 0 caso contrário.",
		}),

		collectorDuration: prometheus.NewDesc(
			prometheus.BuildFQName(collector.Namespace, "collector", "scrape_duration_seconds"),
			"Duração do scrape de cada coletor em segundos.",
			[]string{"collector"}, nil,
		),
		collectorAvail: prometheus.NewDesc(
			prometheus.BuildFQName(collector.Namespace, "collector", "available"),
			"1 se as dependências do coletor (plugin/variável) estão satisfeitas, 0 caso contrário.",
			[]string{"collector"}, nil,
		),
	}

	// Inicializa a série de erros de cada coletor para que a métrica exista com
	// valor 0 antes do primeiro erro — evita gaps em queries de rate().
	for _, c := range e.collectors {
		e.scrapeErrors.WithLabelValues(c.Name())
	}

	return e
}

// Describe implementa prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.scrapeDuration.Describe(ch)
	e.scrapeSuccess.Describe(ch)
	e.scrapeErrors.Describe(ch)
	e.up.Describe(ch)
	ch <- e.collectorDuration
	ch <- e.collectorAvail
}

// Collect implementa prometheus.Collector, seguindo a ordem definida na seção 5:
// verifica conectividade, atualiza mariadb_up, roda os coletores habilitados em
// paralelo com timeout e contabiliza erros e durações.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()

	// Sempre emite as métricas internas, mesmo com o banco offline (seção 17).
	defer func() {
		e.scrapeDuration.Observe(time.Since(start).Seconds())
		e.scrapeDuration.Collect(ch)
		e.scrapeSuccess.Collect(ch)
		e.scrapeErrors.Collect(ch)
		e.up.Collect(ch)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), collectorTimeout)
	defer cancel()

	if err := e.db.PingContext(ctx); err != nil {
		_ = level.Error(e.logger).Log("msg", "banco inacessível, scrape abortado", "err", err)
		e.up.Set(0)
		e.scrapeSuccess.Set(0)
		return
	}
	e.up.Set(1)

	// Publica a disponibilidade de cada coletor que declara dependências.
	for _, c := range e.collectors {
		avail, ok := c.(collector.Availability)
		if !ok {
			continue
		}
		value := 0.0
		if avail.Available() {
			value = 1.0
		}
		ch <- prometheus.MustNewConstMetric(e.collectorAvail, prometheus.GaugeValue, value, c.Name())
	}

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		anyError bool
	)

	for _, c := range e.collectors {
		if !c.Enabled() {
			continue
		}

		wg.Add(1)
		go func(c collector.Collector) {
			defer wg.Done()

			cctx, ccancel := context.WithTimeout(ctx, collectorTimeout)
			defer ccancel()

			cStart := time.Now()
			err := c.Collect(cctx, e.db, ch)
			elapsed := time.Since(cStart).Seconds()

			ch <- prometheus.MustNewConstMetric(
				e.collectorDuration, prometheus.GaugeValue, elapsed, c.Name(),
			)

			if err != nil {
				_ = level.Error(e.logger).Log(
					"msg", "coletor falhou",
					"collector", c.Name(),
					"duracao_s", elapsed,
					"err", err,
				)
				e.scrapeErrors.WithLabelValues(c.Name()).Inc()

				errMu.Lock()
				anyError = true
				errMu.Unlock()
				return
			}

			_ = level.Debug(e.logger).Log("msg", "coletor concluído", "collector", c.Name(), "duracao_s", elapsed)
		}(c)
	}

	wg.Wait()

	if anyError {
		e.scrapeSuccess.Set(0)
	} else {
		e.scrapeSuccess.Set(1)
	}
}

// Ping verifica a conectividade com o banco. Usado pelo endpoint /health.
func (e *Exporter) Ping(ctx context.Context) error {
	return e.db.PingContext(ctx)
}

// BuildInfoCollector devolve a métrica mariadb_exporter_build_info (seção 16).
func BuildInfoCollector(version, buildDate, goVersion string) prometheus.Collector {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: collector.Namespace,
		Subsystem: "exporter",
		Name:      "build_info",
		Help:      "Informações de build do mariadb_exporter.",
	}, []string{"version", "build_date", "go_version"})
	g.WithLabelValues(version, buildDate, goVersion).Set(1)
	return g
}
