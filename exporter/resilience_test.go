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

// panickingCollector simula um coletor com um bug que entra em pânico —
// exatamente o que aconteceria com um label UTF-8 inválido antes da correção,
// ou com qualquer regressão futura num coletor.
type panickingCollector struct{}

func (p *panickingCollector) Name() string  { return "panico" }
func (p *panickingCollector) Help() string  { return "coletor que entra em pânico" }
func (p *panickingCollector) Enabled() bool { return true }

func (p *panickingCollector) Collect(_ context.Context, _ *sql.DB, _ chan<- prometheus.Metric) error {
	panic("bug simulado dentro de um coletor")
}

// Um pânico em qualquer coletor sobe pela goroutine e derruba o processo
// inteiro do exporter — levando embora a coleta de todas as outras instâncias
// monitoradas, não só a métrica com problema.
//
// O Collect roda os coletores em goroutines próprias, e um pânico numa goroutine
// não pode ser recuperado pelo chamador: o único lugar possível é dentro da
// própria goroutine. Este teste fixa esse contrato de isolamento.
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
	}, "um pânico num coletor derrubou o exporter inteiro")

	close(ch)
	<-done
}

// Após um pânico isolado, o coletor problemático deve ser contabilizado em
// mariadb_scrape_errors_total e os demais devem continuar entregando métricas.
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

	// O coletor saudável continua funcionando apesar do vizinho ter entrado em
	// pânico.
	_, ok := familyValue(families, "fake_saudavel")
	require.True(t, ok, "o coletor saudável parou por causa do pânico do outro")

	// E o pânico é reportado como erro de scrape, não silenciado.
	errCount, ok := labeledValue(families, "mariadb_scrape_errors_total", "collector", "panico")
	require.True(t, ok, "não há série de erro para o coletor que entrou em pânico")
	require.Equal(t, float64(1), errCount, "o pânico deveria contar como erro de scrape")

	success, _ := familyValue(families, "mariadb_scrape_success")
	require.Equal(t, float64(0), success, "scrape_success deveria ser 0 após um pânico")
}
