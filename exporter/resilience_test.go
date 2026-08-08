package exporter

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/Kevenny/mariadb-exporter/collector"
	"github.com/Kevenny/mariadb-exporter/config"
)

// panickingCollector simulates a collector with a bug that panics — exactly
// what would happen with an invalid UTF-8 label before the fix, or with any
// future regression in a collector.
type panickingCollector struct{}

func (p *panickingCollector) Name() string  { return "panico" }
func (p *panickingCollector) Help() string  { return "collector that panics" }
func (p *panickingCollector) Enabled() bool { return true }

func (p *panickingCollector) Collect(_ context.Context, _ *sql.DB, _ chan<- prometheus.Metric) error {
	panic("simulated bug inside a collector")
}

// A panic in any collector propagates up the goroutine and takes down the
// exporter's entire process — wiping out collection for all other monitored
// instances, not just the metric with the problem.
//
// Collect runs the collectors in their own goroutines, and a panic in a
// goroutine cannot be recovered by the caller: the only possible place is
// inside the goroutine itself. This test pins down that isolation contract.
func TestExporterSurvivesPanickingCollector(t *testing.T) {
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(true),
	)
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectPing()

	saudavel := newFakeCollector("saudavel", true, nil)
	collectors := []collector.Collector{&panickingCollector{}, saudavel}

	detector := NewFeatureDetector(db, ParseVersion("11.4.3-MariaDB", ""), log.NewNopLogger())
	e := New(db, collectors, detector, config.PMM{}, log.NewNopLogger())

	ch := make(chan prometheus.Metric, 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()

	require.NotPanics(t, func() {
		e.Collect(ch)
	}, "a panic in one collector took down the entire exporter")

	close(ch)
	<-done
}

// After an isolated panic, the problematic collector should be counted in
// mariadb_scrape_errors_total and the others should keep delivering metrics.
func TestExporterPanicIsCountedAsScrapeError(t *testing.T) {
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(true),
	)
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectPing()

	saudavel := newFakeCollector("saudavel", true, nil)
	collectors := []collector.Collector{&panickingCollector{}, saudavel}

	detector := NewFeatureDetector(db, ParseVersion("11.4.3-MariaDB", ""), log.NewNopLogger())
	e := New(db, collectors, detector, config.PMM{}, log.NewNopLogger())

	families := gather(t, e)

	// The healthy collector keeps working despite its neighbor having panicked.
	_, ok := familyValue(families, "fake_saudavel")
	require.True(t, ok, "the healthy collector stopped because of the other's panic")

	// And the panic is reported as a scrape error, not silenced.
	errCount, ok := labeledValue(families, "mariadb_scrape_errors_total", "collector", "panico")
	require.True(t, ok, "there is no error series for the collector that panicked")
	require.Equal(t, float64(1), errCount, "the panic should count as a scrape error")

	success, _ := familyValue(families, "mariadb_scrape_success")
	require.Equal(t, float64(0), success, "scrape_success should be 0 after a panic")
}
