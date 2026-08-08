package exporter

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/Kevenny/mariadb-exporter/collector"
	"github.com/Kevenny/mariadb-exporter/config"
)

// slowCollector emits metrics slowly, widening the window in which two
// concurrent scrapes overlap.
type slowCollector struct {
	name string
	desc *prometheus.Desc
}

func (s *slowCollector) Name() string  { return s.name }
func (s *slowCollector) Help() string  { return "slow test collector" }
func (s *slowCollector) Enabled() bool { return true }

func (s *slowCollector) Collect(_ context.Context, _ *sql.DB, ch chan<- prometheus.Metric) error {
	for i := 0; i < 50; i++ {
		ch <- prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, float64(i))
	}
	return nil
}

// Prometheus allows concurrent scrapes (--web.max-requests default 0 =
// unlimited), and Grafana/PMM can query in parallel. Collect must be safe
// for simultaneous calls: the internal gauges are shared.
//
// Without the race detector (unavailable without cgo on this host), this
// test still catches panics from concurrent map writes and sends on a closed
// channel.
func TestExporterConcurrentCollectDoesNotPanic(t *testing.T) {
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(true),
	)
	require.NoError(t, err)
	defer db.Close()

	// Multiple pings, one per concurrent scrape.
	for i := 0; i < 64; i++ {
		mock.ExpectPing()
	}

	collectors := []collector.Collector{
		&slowCollector{name: "lento1", desc: prometheus.NewDesc("fake_lento1", "t", nil, nil)},
		&slowCollector{name: "lento2", desc: prometheus.NewDesc("fake_lento2", "t", nil, nil)},
	}

	detector := NewFeatureDetector(db, ParseVersion("11.4.3-MariaDB", ""), log.NewNopLogger())
	e := New(db, collectors, detector, config.PMM{ServiceName: "svc"}, log.NewNopLogger())

	const scrapes = 16
	var wg sync.WaitGroup

	require.NotPanics(t, func() {
		for i := 0; i < scrapes; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ch := make(chan prometheus.Metric, 4096)
				done := make(chan struct{})

				// Concurrent drain: Collect would block if the channel filled up.
				go func() {
					defer close(done)
					for range ch {
					}
				}()

				e.Collect(ch)
				close(ch)
				<-done
			}()
		}
		wg.Wait()
	})
}

// The FeatureDetector is read by the collectors on every scrape and written
// by the periodic refresh in another goroutine. Features()/Version() must be
// safe under concurrency with Refresh().
func TestFeatureDetectorConcurrentAccess(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	// The refresh will run several times; allows the queries to fail
	// without disrupting things (the detector logs and moves on).
	mock.MatchExpectationsInOrder(false)
	for i := 0; i < 200; i++ {
		mock.ExpectQuery("FROM information_schema.plugins").
			WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "plugin_status"}).
				AddRow("DISKS", "ACTIVE"))
		mock.ExpectQuery("VARIABLE_NAME = 'userstat'").
			WillReturnRows(sqlmock.NewRows([]string{"VARIABLE_VALUE"}).AddRow("ON"))
		mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'wsrep_ready'").
			WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}))
		mock.ExpectQuery("SELECT @@global.gtid_slave_pos").
			WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(""))
		mock.ExpectQuery("SHOW ALL SLAVES STATUS").
			WillReturnRows(sqlmock.NewRows([]string{"Master_Host"}))
	}

	d := NewFeatureDetector(db, ParseVersion("11.4.3-MariaDB", ""), log.NewNopLogger())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: continuous refresh.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					d.Refresh(context.Background())
				}
			}
		}()
	}

	// Readers: simulate collectors querying the features on every scrape.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					f := d.Features()
					_ = f.HasUserStat
					v := d.Version()
					if v != nil {
						_ = v.String()
					}
				}
			}
		}()
	}

	require.NotPanics(t, func() {
		// Lets it run for a moment and then stops.
		for i := 0; i < 2000; i++ {
			_ = d.Features()
		}
		close(stop)
		wg.Wait()
	})
}

// Features() returns a copy: mutations made by the caller must not affect
// the detector's internal state or other collectors.
func TestFeatureDetectorFeaturesReturnsCopy(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	d := NewFeatureDetector(db, ParseVersion("11.4.3-MariaDB", ""), log.NewNopLogger())

	first := d.Features()
	first.HasUserStat = true
	first.HasGalera = true

	second := d.Features()
	require.False(t, second.HasUserStat,
		"mutation on the copy returned by Features() affected the internal state")
	require.False(t, second.HasGalera)
}
