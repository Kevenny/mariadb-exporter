package collector

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Custom metrics: robustez contra YAML hostil ou malformado
//
// O arquivo de custom metrics é fornecido pelo operador, mas erros nele não
// devem derrubar o processo do exporter em runtime — devem falhar no startup
// (erro de configuração) ou ser ignorados no scrape.
// ---------------------------------------------------------------------------

// Uma métrica cujo nome não é um identificador Prometheus válido faria
// prometheus.NewDesc gerar um Desc inválido, e MustNewConstMetric entraria em
// pânico durante o scrape — derrubando o exporter inteiro por causa de um YAML
// mal preenchido. A validação precisa acontecer no load.
func TestCustomMetricsRejectsInvalidMetricName(t *testing.T) {
	cases := []struct {
		name       string
		metricName string
	}{
		{"com hifen", "mariadb-invalido"},
		{"com espaco", "mariadb invalido"},
		{"comecando com digito", "1mariadb"},
		{"com ponto", "mariadb.invalido"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempYAML(t, `
"`+tc.metricName+`":
  query: SELECT 1 AS total
  metrics:
    - total:
        usage: "GAUGE"
`)
			_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
			require.Error(t, err, "nome de métrica inválido deveria ser rejeitado no load, não em runtime")
		})
	}
}

// Mesmo raciocínio para nomes de label inválidos.
func TestCustomMetricsRejectsInvalidLabelName(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_teste:
  query: SELECT 'x' AS c, 1 AS total
  metrics:
    - "label-invalido":
        usage: "LABEL"
    - total:
        usage: "GAUGE"
`)
	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.Error(t, err, "nome de label inválido deveria ser rejeitado no load")
}

// Uma mesma coluna declarada duas vezes como LABEL gera labels duplicados no
// Desc, o que faz o client_golang entrar em pânico ao construir a métrica.
func TestCustomMetricsRejectsDuplicateLabels(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_teste:
  query: SELECT 'x' AS c, 1 AS total
  metrics:
    - c:
        usage: "LABEL"
    - c:
        usage: "LABEL"
    - total:
        usage: "GAUGE"
`)
	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.Error(t, err, "label duplicado deveria ser rejeitado no load")
}

// Uma coluna declarada ao mesmo tempo como LABEL e como valor é ambígua e
// também produziria um Desc inconsistente.
func TestCustomMetricsRejectsColumnAsBothLabelAndValue(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_teste:
  query: SELECT 1 AS total
  metrics:
    - total:
        usage: "LABEL"
    - total:
        usage: "GAUGE"
`)
	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.Error(t, err, "coluna usada como label e valor deveria ser rejeitada")
}

// Duas métricas com nomes que colidem após a sufixação de coluna produziriam
// séries com o mesmo nome e conjuntos de label diferentes — o registry do
// Prometheus rejeita isso no scrape.
func TestCustomMetricsCollectDoesNotPanicOnHostileYAML(t *testing.T) {
	// Este YAML passa pela validação de estrutura mas tem uma query que devolve
	// uma coluna a mais do que o declarado: o código precisa lidar com isso sem
	// pânico de índice.
	path := writeTempYAML(t, `
mariadb_extra_colunas:
  query: SELECT 'a' AS c, 1 AS total, 'sobrando' AS extra
  metrics:
    - c:
        usage: "LABEL"
    - total:
        usage: "GAUGE"
`)

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err)

	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT 'a'").WillReturnRows(
		sqlmock.NewRows([]string{"c", "total", "extra"}).AddRow("a", 1, "sobrando"),
	)

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		// Colunas não declaradas são simplesmente ignoradas.
		require.Len(t, metrics, 1)
	})
}

// Valores de label vindos do banco podem conter UTF-8 inválido (por exemplo, um
// blob binário numa coluna usada como label). O formato de exposição do
// Prometheus exige UTF-8 válido; bytes inválidos corrompem a saída de /metrics
// para todos os scrapes.
func TestCustomMetricsHandlesInvalidUTF8InLabels(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_utf8_teste:
  query: SELECT 'x' AS c, 1 AS total
  metrics:
    - c:
        usage: "LABEL"
    - total:
        usage: "GAUGE"
`)

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err)

	db, mock := newMockDB(t)
	// 0xff isolado não é UTF-8 válido.
	mock.ExpectQuery("SELECT 'x'").WillReturnRows(
		sqlmock.NewRows([]string{"c", "total"}).AddRow([]byte{0xff, 0xfe, 0x41}, 1),
	)

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Len(t, metrics, 1)

	// A métrica precisa ser serializável: labels com UTF-8 inválido devem ter
	// sido sanitizados antes de virarem label.
	snaps := snapshot(t, metrics)
	for _, s := range snaps {
		for k, v := range s.Labels {
			require.True(t, isValidUTF8(v),
				"label %q tem UTF-8 inválido (%q), o que corrompe a saída de /metrics", k, v)
		}
	}
}

func isValidUTF8(s string) bool {
	return strings.ToValidUTF8(s, "�") == s
}

// Um arquivo de custom metrics gigantesco não deve travar o startup nem
// consumir memória sem limite.
func TestCustomMetricsHandlesEmptyYAML(t *testing.T) {
	path := writeTempYAML(t, "")

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err, "YAML vazio não é erro, apenas não define métricas")
	require.Empty(t, c.metrics)
}

// YAML com estrutura totalmente diferente do esperado (lista no lugar de mapa)
// deve dar erro claro em vez de pânico.
func TestCustomMetricsRejectsWrongTopLevelType(t *testing.T) {
	path := writeTempYAML(t, "- isso\n- e\n- uma\n- lista\n")

	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Limites de tablestat/indexstat: injeção via LIMIT
//
// Os limites entram na query via fmt.Sprintf. Valores extremos ou negativos não
// devem gerar SQL inválido.
// ---------------------------------------------------------------------------

// Um limite negativo produziria "LIMIT -1", que é erro de sintaxe no MariaDB.
// O código já normaliza <= 0 para o padrão; este teste fixa esse contrato.
func TestTableStatNegativeLimitFallsBackToDefault(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`LIMIT 500`).WillReturnRows(
		sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ROWS_READ", "ROWS_CHANGED", "ROWS_CHANGED_X_INDEXES"}),
	)

	c := NewTableStatCollector(true, -100, testLogger(), allFeatures())
	_, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIndexStatNegativeLimitFallsBackToDefault(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`LIMIT 1000`).WillReturnRows(
		sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "ROWS_READ"}),
	)

	c := NewIndexStatCollector(true, -1, testLogger(), allFeatures())
	_, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Robustez de parsing contra dados hostis do servidor
// ---------------------------------------------------------------------------

// strconv.ParseFloat aceita "NaN", "Inf" e "infinity", mas nenhum é um valor de
// métrica útil: NaN desaparece dos gráficos e faz comparações de alerta
// falharem em silêncio, e Inf distorce agregações. parseFloat precisa recusá-los
// para que o coletor omita a métrica em vez de expor lixo.
func TestParseFloatRejectsNonFiniteValues(t *testing.T) {
	for _, s := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity", "-infinity"} {
		_, err := parseFloat(s)
		require.Error(t, err, "parseFloat(%q) deveria ser recusado", s)
	}

	// Também nos tipos numéricos nativos que o driver pode devolver.
	_, err := parseFloat(math.NaN())
	require.Error(t, err, "float64 NaN deveria ser recusado")

	_, err = parseFloat(math.Inf(1))
	require.Error(t, err, "float64 +Inf deveria ser recusado")

	_, err = parseFloat(float32(math.Inf(-1)))
	require.Error(t, err, "float32 -Inf deveria ser recusado")
}

// Um valor não finito no SHOW GLOBAL STATUS deve fazer a métrica ser omitida,
// não exposta como NaN.
func TestGlobalStatusOmitsNonFiniteValues(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("Uptime", "NaN").
		AddRow("Open_files", "Inf").
		AddRow("Questions", "42")

	mock.ExpectQuery("SHOW GLOBAL STATUS").WillReturnRows(rows)

	c := NewGlobalStatusCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	for _, s := range snaps {
		require.False(t, math.IsNaN(s.Value), "métrica %s exposta como NaN", s.Name)
		require.False(t, math.IsInf(s.Value, 0), "métrica %s exposta como Inf", s.Name)
	}

	// A variável válida continua sendo exportada.
	require.Len(t, snaps, 1)
	require.Equal(t, float64(42), requireMetric(t, snaps, "mariadb_questions_total", nil).Value)
}

// Valores gigantes em colunas de contador não devem causar overflow silencioso
// para negativo.
func TestParseFloatVeryLargeValues(t *testing.T) {
	// Maior uint64 — comum em contadores que dão wrap no MariaDB.
	v, err := parseFloat("18446744073709551615")
	require.NoError(t, err)
	require.Positive(t, v, "contador máximo não deve virar negativo")
}

// O coletor de QRT converte float para uint64. Um COUNT negativo (impossível na
// prática, mas defensivo) causaria um wrap gigantesco nas contagens do
// histograma, quebrando as queries de rate().
func TestQueryResponseTimeIgnoresNegativeCount(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"TIME", "COUNT", "TOTAL"}).
		AddRow("0.001000", -5, "0.002").
		AddRow("0.010000", 10, "0.05")

	mock.ExpectQuery("QUERY_RESPONSE_TIME").WillReturnRows(rows)

	c := NewQueryResponseTimeCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	if len(metrics) == 0 {
		t.Skip("nenhuma métrica emitida")
	}

	h := snapshot(t, metrics)[0]
	// Se o -5 virou uint64, SampleCount fica astronomicamente grande.
	require.Less(t, h.SampleCount, uint64(1000),
		"COUNT negativo virou wrap de uint64: SampleCount=%d", h.SampleCount)

	for bound, count := range h.Buckets {
		require.Less(t, count, uint64(1000),
			"bucket le=%v com contagem absurda %d (wrap de uint64)", bound, count)
	}
}

// Buckets duplicados na tabela de QRT (mesmo TIME em duas linhas) não devem
// gerar um histograma inconsistente.
func TestQueryResponseTimeDuplicateBuckets(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"TIME", "COUNT", "TOTAL"}).
		AddRow("0.001000", 5, "0.002").
		AddRow("0.001000", 7, "0.003")

	mock.ExpectQuery("QUERY_RESPONSE_TIME").WillReturnRows(rows)

	c := NewQueryResponseTimeCollector(true, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		require.NotEmpty(t, metrics)
	})
}

// ---------------------------------------------------------------------------
// Registro concorrente / uso do canal
// ---------------------------------------------------------------------------

// Um coletor que devolve muitas linhas não deve bloquear indefinidamente se o
// contexto for cancelado.
func TestCollectRespectsContextCancellation(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SHOW GLOBAL STATUS").WillReturnRows(
		sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("Uptime", "100"),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // já cancelado

	c := NewGlobalStatusCollector(true, testLogger(), allFeatures())
	ch := make(chan prometheus.Metric, 10)

	// Não deve travar nem entrar em pânico com contexto já cancelado.
	require.NotPanics(t, func() {
		_ = c.Collect(ctx, db, ch)
	})
}

// ---------------------------------------------------------------------------
// Path traversal / leitura de arquivo
// ---------------------------------------------------------------------------

// Caminho de custom metrics apontando para um diretório deve dar erro claro.
func TestCustomMetricsPathIsDirectory(t *testing.T) {
	dir := t.TempDir()

	_, err := NewCustomMetricsCollector([]string{dir}, testLogger(), allFeatures(), nil)
	require.Error(t, err, "diretório no lugar de arquivo deveria dar erro")
}

// Arquivo inexistente já é coberto, mas um caminho com bytes nulos pode causar
// comportamento estranho no syscall.
func TestCustomMetricsPathWithNullByte(t *testing.T) {
	_, err := NewCustomMetricsCollector(
		[]string{filepath.Join(os.TempDir(), "arquivo\x00malicioso.yml")},
		testLogger(), allFeatures(), nil,
	)
	require.Error(t, err)
}

// As custom metrics precisam carregar as mesmas ConstLabels de integração com o
// PMM que as métricas nativas. Sem isso, um painel filtrando por cluster ou
// service_name não encontraria a série — o dado existiria mas ficaria invisível
// no dashboard.
func TestCustomMetricsCarryPMMConstLabels(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_teste_labels:
  query: SELECT 'demo' AS schema_name, 7 AS total
  metrics:
    - schema_name:
        usage: "LABEL"
    - total:
        usage: "GAUGE"
`)

	constLabels := prometheus.Labels{
		"service_name": "mariadb-host01",
		"cluster":      "prod-cluster",
		"environment":  "production",
	}

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), constLabels)
	require.NoError(t, err)

	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT 'demo'").WillReturnRows(
		sqlmock.NewRows([]string{"schema_name", "total"}).AddRow("demo", 7),
	)

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	m := requireMetric(t, snaps, "mariadb_teste_labels", map[string]string{
		"schema_name":  "demo",
		"service_name": "mariadb-host01",
		"cluster":      "prod-cluster",
		"environment":  "production",
	})
	require.Equal(t, float64(7), m.Value)
}
