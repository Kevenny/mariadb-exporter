package exporter

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/Kevenny/mariadb-exporter/collector"
	"github.com/Kevenny/mariadb-exporter/config"
)

// fakeCollector é um coletor controlável, usado para exercitar a orquestração
// sem depender de queries reais.
type fakeCollector struct {
	name      string
	enabled   bool
	available bool
	err       error
	desc      *prometheus.Desc
	value     float64
}

func newFakeCollector(name string, enabled bool, err error) *fakeCollector {
	return &fakeCollector{
		name:      name,
		enabled:   enabled,
		available: true,
		err:       err,
		desc:      prometheus.NewDesc("fake_"+name, "métrica de teste", nil, nil),
		value:     1,
	}
}

func (f *fakeCollector) Name() string    { return f.name }
func (f *fakeCollector) Help() string    { return "coletor de teste" }
func (f *fakeCollector) Enabled() bool   { return f.enabled }
func (f *fakeCollector) Available() bool { return f.available }

func (f *fakeCollector) Collect(_ context.Context, _ *sql.DB, ch chan<- prometheus.Metric) error {
	if f.err != nil {
		return f.err
	}
	ch <- prometheus.MustNewConstMetric(f.desc, prometheus.GaugeValue, f.value)
	return nil
}

func newTestExporter(t *testing.T, collectors []collector.Collector) (*Exporter, sqlmock.Sqlmock) {
	t.Helper()
	return newTestExporterWithPMM(t, collectors, config.PMM{})
}

func newTestExporterWithPMM(t *testing.T, collectors []collector.Collector, pmm config.PMM) (*Exporter, sqlmock.Sqlmock) {
	t.Helper()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp), sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	detector := NewFeatureDetector(db, ParseVersion("11.4.3-MariaDB", ""), log.NewNopLogger())
	return New(db, collectors, detector, pmm, log.NewNopLogger()), mock
}

// gather coleta as métricas do exporter através de um registry real.
//
// Usa NewRegistry (e não NewPedanticRegistry) porque os fakeCollector emitem
// Descs próprios que o Describe do Exporter não declara — o registry pedante
// rejeitaria essas métricas, que aqui são justamente o que se quer observar.
func gather(t *testing.T, e *Exporter) []*dto.MetricFamily {
	t.Helper()

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(e))

	families, err := reg.Gather()
	require.NoError(t, err)
	return families
}

// familyExists informa se a família de métricas foi exposta, independente do
// tipo. Necessário para histogramas, que não têm gauge/counter value.
func familyExists(families []*dto.MetricFamily, name string) bool {
	for _, f := range families {
		if f.GetName() == name {
			return len(f.GetMetric()) > 0
		}
	}
	return false
}

func familyValue(families []*dto.MetricFamily, name string) (float64, bool) {
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			switch {
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}

// labeledValue busca o valor de uma métrica filtrando por um label.
func labeledValue(families []*dto.MetricFamily, name, label, value string) (float64, bool) {
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() != label || lp.GetValue() != value {
					continue
				}
				switch {
				case m.GetGauge() != nil:
					return m.GetGauge().GetValue(), true
				case m.GetCounter() != nil:
					return m.GetCounter().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

func TestExporterCollectSuccess(t *testing.T) {
	ok1 := newFakeCollector("ok1", true, nil)
	ok2 := newFakeCollector("ok2", true, nil)
	disabled := newFakeCollector("desligado", false, nil)

	e, mock := newTestExporter(t, []collector.Collector{ok1, ok2, disabled})
	mock.ExpectPing()

	families := gather(t, e)

	up, ok := familyValue(families, "mariadb_up")
	require.True(t, ok)
	require.Equal(t, float64(1), up)

	success, ok := familyValue(families, "mariadb_scrape_success")
	require.True(t, ok)
	require.Equal(t, float64(1), success)

	// Métricas dos coletores habilitados chegam ao registry.
	_, ok = familyValue(families, "fake_ok1")
	require.True(t, ok)
	_, ok = familyValue(families, "fake_ok2")
	require.True(t, ok)

	// Coletor desabilitado não roda.
	_, ok = familyValue(families, "fake_desligado")
	require.False(t, ok)

	// Duração por coletor é publicada para os habilitados.
	_, ok = labeledValue(families, "mariadb_collector_scrape_duration_seconds", "collector", "ok1")
	require.True(t, ok)
}

// Erro em um coletor incrementa o contador daquele coletor e não impede os outros.
func TestExporterCollectPartialFailure(t *testing.T) {
	bom := newFakeCollector("bom", true, nil)
	ruim := newFakeCollector("ruim", true, errors.New("permissão negada"))

	e, mock := newTestExporter(t, []collector.Collector{bom, ruim})
	mock.ExpectPing()

	families := gather(t, e)

	// O coletor saudável continua entregando métricas.
	_, ok := familyValue(families, "fake_bom")
	require.True(t, ok)

	errCount, ok := labeledValue(families, "mariadb_scrape_errors_total", "collector", "ruim")
	require.True(t, ok)
	require.Equal(t, float64(1), errCount)

	okCount, ok := labeledValue(families, "mariadb_scrape_errors_total", "collector", "bom")
	require.True(t, ok, "a série deve existir com 0 antes do primeiro erro")
	require.Equal(t, float64(0), okCount)

	success, _ := familyValue(families, "mariadb_scrape_success")
	require.Equal(t, float64(0), success, "qualquer erro zera o scrape_success")

	up, _ := familyValue(families, "mariadb_up")
	require.Equal(t, float64(1), up, "erro de coletor não afeta mariadb_up")
}

// Banco offline: mariadb_up=0, métricas internas presentes e retorno rápido
// sem executar coletores (seção 17).
func TestExporterCollectDatabaseDown(t *testing.T) {
	c := newFakeCollector("qualquer", true, nil)

	e, mock := newTestExporter(t, []collector.Collector{c})
	mock.ExpectPing().WillReturnError(errors.New("connection refused"))

	families := gather(t, e)

	up, ok := familyValue(families, "mariadb_up")
	require.True(t, ok)
	require.Equal(t, float64(0), up)

	success, _ := familyValue(families, "mariadb_scrape_success")
	require.Equal(t, float64(0), success)

	// Nenhum coletor deve ter rodado.
	_, ok = familyValue(families, "fake_qualquer")
	require.False(t, ok)

	// As métricas internas continuam expostas (duração é histograma).
	require.True(t, familyExists(families, "mariadb_scrape_duration_seconds"))
}

// mariadb_collector_available reflete o Available() de cada coletor.
func TestExporterCollectorAvailability(t *testing.T) {
	disponivel := newFakeCollector("disponivel", true, nil)
	indisponivel := newFakeCollector("indisponivel", true, nil)
	indisponivel.available = false

	e, mock := newTestExporter(t, []collector.Collector{disponivel, indisponivel})
	mock.ExpectPing()

	families := gather(t, e)

	v, ok := labeledValue(families, "mariadb_collector_available", "collector", "disponivel")
	require.True(t, ok)
	require.Equal(t, float64(1), v)

	v, ok = labeledValue(families, "mariadb_collector_available", "collector", "indisponivel")
	require.True(t, ok)
	require.Equal(t, float64(0), v)
}

func TestExporterPing(t *testing.T) {
	e, mock := newTestExporter(t, nil)

	mock.ExpectPing()
	require.NoError(t, e.Ping(context.Background()))

	mock.ExpectPing().WillReturnError(errors.New("offline"))
	require.Error(t, e.Ping(context.Background()))
}

func TestBuildInfoCollector(t *testing.T) {
	c := BuildInfoCollector("1.2.3", "2026-08-07T00:00:00Z", "go1.22.0", config.PMM{})

	expected := `
# HELP mariadb_exporter_build_info Informações de build do mariadb_exporter.
# TYPE mariadb_exporter_build_info gauge
mariadb_exporter_build_info{build_date="2026-08-07T00:00:00Z",go_version="go1.22.0",version="1.2.3"} 1
`
	require.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
}

// Com metadados de PMM informados, eles devem aparecer como ConstLabels em
// mariadb_exporter_build_info (mariadb_exporter_pmm_integration.md, seção 3).
func TestBuildInfoCollectorWithPMMConstLabels(t *testing.T) {
	pmm := config.PMM{ServiceName: "mariadb-host01", Cluster: "prod-cluster", Environment: "production"}
	c := BuildInfoCollector("1.2.3", "2026-08-07T00:00:00Z", "go1.22.0", pmm)

	expected := `
# HELP mariadb_exporter_build_info Informações de build do mariadb_exporter.
# TYPE mariadb_exporter_build_info gauge
mariadb_exporter_build_info{build_date="2026-08-07T00:00:00Z",cluster="prod-cluster",environment="production",go_version="go1.22.0",service_name="mariadb-host01",version="1.2.3"} 1
`
	require.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
}

// As métricas internas do exporter (mariadb_up, scrape_success, etc.) também
// devem carregar as ConstLabels de PMM quando configuradas.
func TestExporterConstLabelsFromPMM(t *testing.T) {
	pmm := config.PMM{ServiceName: "mariadb-host01", Cluster: "prod-cluster", Environment: "production"}
	e, mock := newTestExporterWithPMM(t, nil, pmm)
	mock.ExpectPing()

	families := gather(t, e)

	up, ok := labeledValue(families, "mariadb_up", "cluster", "prod-cluster")
	require.True(t, ok, "mariadb_up deveria carregar o label cluster")
	require.Equal(t, float64(1), up)

	_, ok = labeledValue(families, "mariadb_up", "service_name", "mariadb-host01")
	require.True(t, ok, "mariadb_up deveria carregar o label service_name")

	_, ok = labeledValue(families, "mariadb_up", "environment", "production")
	require.True(t, ok, "mariadb_up deveria carregar o label environment")
}

// Sem configuração de PMM, os labels de cluster/service_name/environment não
// devem aparecer — instalações sem PMM não devem ganhar labels vazios à toa.
func TestExporterNoConstLabelsWithoutPMM(t *testing.T) {
	e, mock := newTestExporter(t, nil)
	mock.ExpectPing()

	families := gather(t, e)

	for _, f := range families {
		if f.GetName() != "mariadb_up" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				require.NotEqual(t, "cluster", lp.GetName(), "não deveria haver label cluster sem --pmm.cluster")
				require.NotEqual(t, "service_name", lp.GetName(), "não deveria haver label service_name sem --pmm.service-name")
			}
		}
	}
}
