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

// slowCollector emite métricas devagar, ampliando a janela em que dois scrapes
// concorrentes se sobrepõem.
type slowCollector struct {
	name string
	desc *prometheus.Desc
}

func (s *slowCollector) Name() string  { return s.name }
func (s *slowCollector) Help() string  { return "coletor lento de teste" }
func (s *slowCollector) Enabled() bool { return true }

func (s *slowCollector) Collect(_ context.Context, _ *sql.DB, ch chan<- prometheus.Metric) error {
	for i := 0; i < 50; i++ {
		ch <- prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, float64(i))
	}
	return nil
}

// O Prometheus permite scrapes concorrentes (--web.max-requests default 0 =
// ilimitado), e o Grafana/PMM podem consultar em paralelo. Collect precisa ser
// seguro para chamadas simultâneas: os gauges internos são compartilhados.
//
// Sem o detector de race (indisponível sem cgo neste host), este teste ainda
// pega pânicos por escrita concorrente em mapa e envio em canal fechado.
func TestExporterConcurrentCollectDoesNotPanic(t *testing.T) {
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(true),
	)
	require.NoError(t, err)
	defer db.Close()

	// Vários pings, um por scrape concorrente.
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

				// Dreno concorrente: o Collect bloquearia se o canal enchesse.
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

// O FeatureDetector é lido pelos coletores a cada scrape e escrito pelo refresh
// periódico em outra goroutine. Features()/Version() devem ser seguros sob
// concorrência com Refresh().
func TestFeatureDetectorConcurrentAccess(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	// O refresh vai rodar várias vezes; permite que as queries falhem sem
	// atrapalhar (o detector loga e segue).
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

	// Escritores: refresh contínuo.
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

	// Leitores: simulam coletores consultando as features a cada scrape.
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
		// Deixa rodar um instante e para.
		for i := 0; i < 2000; i++ {
			_ = d.Features()
		}
		close(stop)
		wg.Wait()
	})
}

// Features() devolve uma cópia: mutações feitas pelo chamador não devem afetar o
// estado interno do detector nem outros coletores.
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
		"mutação na cópia devolvida por Features() afetou o estado interno")
	require.False(t, second.HasGalera)
}
