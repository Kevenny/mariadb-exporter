package exporter

import (
	"context"
	"database/sql"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Kevenny/mariadb-exporter/collector"
	"github.com/Kevenny/mariadb-exporter/config"
)

// collectorTimeout is the time cap per collector (section 5, item 3).
const collectorTimeout = 30 * time.Second

// Exporter implements prometheus.Collector.
// Manages the connection pool and orchestrates the collectors.
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

// New creates the exporter with the given collectors. The metadata in pmm
// becomes ConstLabels on the internal metrics, allowing PMM's cluster and
// environment filters to work in dashboards.
func New(db *sql.DB, collectors []collector.Collector, detector *FeatureDetector, pmm config.PMM, logger log.Logger) *Exporter {
	constLabels := prometheus.Labels(pmm.ConstLabels())

	e := &Exporter{
		db:         db,
		collectors: collectors,
		detector:   detector,
		logger:     logger,

		scrapeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace:   collector.Namespace,
			Name:        "scrape_duration_seconds",
			Help:        "Total duration of the mariadb_exporter scrape in seconds.",
			Buckets:     []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
			ConstLabels: constLabels,
		}),
		scrapeSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   collector.Namespace,
			Name:        "scrape_success",
			Help:        "1 if the last scrape ran without errors in any collector, 0 otherwise.",
			ConstLabels: constLabels,
		}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   collector.Namespace,
			Name:        "scrape_errors_total",
			Help:        "Total scrape errors per collector.",
			ConstLabels: constLabels,
		}, []string{"collector"}),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   collector.Namespace,
			Name:        "up",
			Help:        "1 if the exporter is connected to MariaDB, 0 otherwise.",
			ConstLabels: constLabels,
		}),

		collectorDuration: prometheus.NewDesc(
			prometheus.BuildFQName(collector.Namespace, "collector", "scrape_duration_seconds"),
			"Scrape duration of each collector in seconds.",
			[]string{"collector"}, constLabels,
		),
		collectorAvail: prometheus.NewDesc(
			prometheus.BuildFQName(collector.Namespace, "collector", "available"),
			"1 if the collector's dependencies (plugin/variable) are satisfied, 0 otherwise.",
			[]string{"collector"}, constLabels,
		),
	}

	// Initializes the error series for each collector so the metric exists
	// with value 0 before the first error — avoids gaps in rate() queries.
	for _, c := range e.collectors {
		e.scrapeErrors.WithLabelValues(c.Name())
	}

	return e
}

// Describe implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.scrapeDuration.Describe(ch)
	e.scrapeSuccess.Describe(ch)
	e.scrapeErrors.Describe(ch)
	e.up.Describe(ch)
	ch <- e.collectorDuration
	ch <- e.collectorAvail
}

// Collect implements prometheus.Collector, following the order defined in
// section 5: checks connectivity, updates mariadb_up, runs the enabled
// collectors in parallel with a timeout, and tallies errors and durations.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()

	// Always emits the internal metrics, even with the database offline (section 17).
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
		_ = level.Error(e.logger).Log("msg", "database unreachable, scrape aborted", "err", err)
		e.up.Set(0)
		e.scrapeSuccess.Set(0)
		return
	}
	e.up.Set(1)

	// Publishes the availability of each collector that declares dependencies.
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
			err := collectSafely(c, cctx, e.db, ch)
			elapsed := time.Since(cStart).Seconds()

			ch <- prometheus.MustNewConstMetric(
				e.collectorDuration, prometheus.GaugeValue, elapsed, c.Name(),
			)

			if err != nil {
				_ = level.Error(e.logger).Log(
					"msg", "collector failed",
					"collector", c.Name(),
					"duration_s", elapsed,
					"err", err,
				)
				e.scrapeErrors.WithLabelValues(c.Name()).Inc()

				errMu.Lock()
				anyError = true
				errMu.Unlock()
				return
			}

			_ = level.Debug(e.logger).Log("msg", "collector completed", "collector", c.Name(), "duration_s", elapsed)
		}(c)
	}

	wg.Wait()

	if anyError {
		e.scrapeSuccess.Set(0)
	} else {
		e.scrapeSuccess.Set(1)
	}
}

// collectSafely runs a collector, converting any panic into an error.
//
// Each collector runs in its own goroutine, and a panic there cannot be
// recovered by the caller: it would take down the exporter's entire
// process, stopping collection for all monitored instances because of a
// single faulty collector (an unexpected label, a missing column, a future
// regression). Recovering here isolates the failure to the affected
// collector, which is then counted in mariadb_scrape_errors_total like any
// other error.
func collectSafely(c collector.Collector, ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in collector %s: %v\n%s", c.Name(), r, debug.Stack())
		}
	}()
	return c.Collect(ctx, db, ch)
}

// Ping checks connectivity with the database. Used by the /health endpoint.
func (e *Exporter) Ping(ctx context.Context) error {
	return e.db.PingContext(ctx)
}

// BuildInfoCollector returns the mariadb_exporter_build_info metric
// (section 16), with the PMM metadata applied as ConstLabels.
func BuildInfoCollector(version, buildDate, goVersion string, pmm config.PMM) prometheus.Collector {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   collector.Namespace,
		Subsystem:   "exporter",
		Name:        "build_info",
		Help:        "Build information for mariadb_exporter.",
		ConstLabels: prometheus.Labels(pmm.ConstLabels()),
	}, []string{"version", "build_date", "go_version"})
	g.WithLabelValues(version, buildDate, goVersion).Set(1)
	return g
}
