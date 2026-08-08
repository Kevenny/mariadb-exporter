package collector

import (
	"context"
	"database/sql"
	"io"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// testLogger devolve um logger silencioso para os testes.
func testLogger() log.Logger {
	return log.NewNopLogger()
}

// staticFeatures implementa FeatureProvider com valores fixos, permitindo testar
// os caminhos de plugin disponível e indisponível.
type staticFeatures struct {
	features *FeatureFlags
	version  *VersionInfo
}

func (s staticFeatures) Features() *FeatureFlags { return s.features }
func (s staticFeatures) Version() *VersionInfo   { return s.version }

// allFeatures habilita todas as features detectáveis.
func allFeatures() staticFeatures {
	return staticFeatures{
		features: &FeatureFlags{
			HasUserStat:          true,
			HasQueryResponseTime: true,
			HasMetadataLockInfo:  true,
			HasDisksPlugin:       true,
			HasGalera:            true,
			IsReplica:            true,
		},
		version: &VersionInfo{Major: 11, Minor: 4, Patch: 3, Full: "11.4.3-MariaDB", IsMariaDB: true},
	}
}

// noFeatures desabilita todas as features.
func noFeatures() staticFeatures {
	return staticFeatures{
		features: &FeatureFlags{},
		version:  &VersionInfo{Major: 11, Minor: 4, Patch: 3, Full: "11.4.3-MariaDB", IsMariaDB: true},
	}
}

// newMockDB cria um *sql.DB com sqlmock. QueryMatcherRegexp é usado porque as
// queries do exporter são multilinha e a comparação literal seria frágil.
func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// runCollect executa o coletor e devolve as métricas emitidas.
//
// O Collect roda em goroutine e o canal é fechado ao término, para que a leitura
// não precise adivinhar quantas métricas virão.
func runCollect(t *testing.T, c Collector, db *sql.DB) ([]prometheus.Metric, error) {
	t.Helper()

	ch := make(chan prometheus.Metric, 1024)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		errCh <- c.Collect(context.Background(), db, ch)
	}()

	var metrics []prometheus.Metric
	for m := range ch {
		metrics = append(metrics, m)
	}

	return metrics, <-errCh
}

// metricSnapshot é a forma comparável de uma métrica coletada.
type metricSnapshot struct {
	Name   string
	Labels map[string]string
	Value  float64

	// Preenchidos apenas para histogramas.
	SampleCount uint64
	SampleSum   float64
	Buckets     map[float64]uint64
}

// snapshot converte as métricas do canal em structs inspecionáveis.
func snapshot(t *testing.T, metrics []prometheus.Metric) []metricSnapshot {
	t.Helper()

	out := make([]metricSnapshot, 0, len(metrics))
	for _, m := range metrics {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))

		snap := metricSnapshot{
			Name:   metricName(t, m),
			Labels: map[string]string{},
		}
		for _, lp := range pb.GetLabel() {
			snap.Labels[lp.GetName()] = lp.GetValue()
		}

		switch {
		case pb.GetCounter() != nil:
			snap.Value = pb.GetCounter().GetValue()
		case pb.GetGauge() != nil:
			snap.Value = pb.GetGauge().GetValue()
		case pb.GetUntyped() != nil:
			snap.Value = pb.GetUntyped().GetValue()
		case pb.GetHistogram() != nil:
			h := pb.GetHistogram()
			snap.SampleCount = h.GetSampleCount()
			snap.SampleSum = h.GetSampleSum()
			snap.Buckets = make(map[float64]uint64, len(h.GetBucket()))
			for _, b := range h.GetBucket() {
				snap.Buckets[b.GetUpperBound()] = b.GetCumulativeCount()
			}
		}

		out = append(out, snap)
	}

	return out
}

// metricName extrai o nome totalmente qualificado a partir do Desc.
//
// O Desc não expõe o nome como campo público, mas seu String() tem o formato
// `Desc{fqName: "nome", help: ...}`, de onde o nome é lido.
func metricName(t *testing.T, m prometheus.Metric) string {
	t.Helper()

	desc := m.Desc().String()
	const marker = `fqName: "`
	start := indexOf(desc, marker)
	require.GreaterOrEqual(t, start, 0, "formato inesperado de Desc: %s", desc)
	start += len(marker)

	end := indexOf(desc[start:], `"`)
	require.GreaterOrEqual(t, end, 0, "formato inesperado de Desc: %s", desc)

	return desc[start : start+end]
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// findMetric localiza a primeira métrica com o nome e os labels informados.
func findMetric(snaps []metricSnapshot, name string, labels map[string]string) (metricSnapshot, bool) {
	for _, s := range snaps {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s, true
		}
	}
	return metricSnapshot{}, false
}

// requireMetric falha o teste se a métrica não existir, devolvendo-a caso exista.
func requireMetric(t *testing.T, snaps []metricSnapshot, name string, labels map[string]string) metricSnapshot {
	t.Helper()
	s, ok := findMetric(snaps, name, labels)
	require.True(t, ok, "métrica %s com labels %v não encontrada; coletadas: %v", name, labels, names(snaps))
	return s
}

// requireNoMetric falha o teste se a métrica existir.
func requireNoMetric(t *testing.T, snaps []metricSnapshot, name string) {
	t.Helper()
	for _, s := range snaps {
		require.NotEqual(t, name, s.Name, "métrica %s não deveria ter sido emitida", name)
	}
}

func names(snaps []metricSnapshot) []string {
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.Name)
	}
	return out
}

// discardWriter evita que os logs dos coletores poluam a saída dos testes.
var _ io.Writer = io.Discard

func TestVersionInfoAtLeast(t *testing.T) {
	v := &VersionInfo{Major: 10, Minor: 0, Patch: 7}

	require.True(t, v.AtLeast(10, 0, 7), "mesma versão deve satisfazer")
	require.True(t, v.AtLeast(10, 0, 6))
	require.True(t, v.AtLeast(9, 9, 9))
	require.False(t, v.AtLeast(10, 0, 8))
	require.False(t, v.AtLeast(10, 1, 0))
	require.False(t, v.AtLeast(11, 0, 0))

	var nilVersion *VersionInfo
	require.False(t, nilVersion.AtLeast(10, 0, 0), "versão nil nunca satisfaz")
}

func TestParseFloat(t *testing.T) {
	cases := []struct {
		name    string
		input   interface{}
		want    float64
		wantErr bool
	}{
		{"inteiro", int64(42), 42, false},
		{"string numérica", "3.5", 3.5, false},
		{"bytes numéricos", []byte("120"), 120, false},
		{"ON vira 1", "ON", 1, false},
		{"OFF vira 0", "OFF", 0, false},
		{"YES vira 1", "yes", 1, false},
		{"nulo é erro", nil, 0, true},
		{"texto é erro", "abacate", 0, true},
		{"vazio é erro", "", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFloat(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestRegistryEnabled(t *testing.T) {
	r := NewRegistry()
	r.Register(
		NewInfoCollector(testLogger(), noFeatures()),
		NewGaleraCollector(false, testLogger(), noFeatures()),
		NewInnoDBCollector(true, testLogger(), noFeatures()),
		nil, // valores nil são ignorados
	)

	require.Len(t, r.All(), 3)
	require.ElementsMatch(t, []string{"info", "innodb"}, collectorNames(r.Enabled()))
	require.ElementsMatch(t, []string{"info", "galera", "innodb"}, r.Names())
}

func collectorNames(cs []Collector) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name())
	}
	return out
}
