package collector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// writeTempYAML grava um YAML temporário e devolve seu caminho.
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "custom.yml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// Este YAML é o exemplo da seção 10 da especificação.
const exampleYAML = `
mariadb_active_sessions_by_schema:
  query: |
    SELECT db AS schema_name,
           command,
           COUNT(*) AS total
    FROM information_schema.PROCESSLIST
    WHERE db IS NOT NULL
    GROUP BY db, command
  metrics:
    - schema_name:
        usage: "LABEL"
        description: "Schema do banco"
    - command:
        usage: "LABEL"
        description: "Tipo de comando"
    - total:
        usage: "GAUGE"
        description: "Total de sessoes ativas por schema e comando"
`

func TestCustomMetricsCollector(t *testing.T) {
	path := writeTempYAML(t, exampleYAML)

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures())
	require.NoError(t, err)
	require.True(t, c.Enabled())

	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"schema_name", "command", "total"}).
		AddRow("vendas", "Query", 5).
		AddRow("vendas", "Sleep", 12)

	mock.ExpectQuery("FROM information_schema.PROCESSLIST").WillReturnRows(rows)

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	require.Len(t, snaps, 2)

	query := requireMetric(t, snaps, "mariadb_active_sessions_by_schema",
		map[string]string{"schema_name": "vendas", "command": "Query"})
	require.Equal(t, float64(5), query.Value)

	sleep := requireMetric(t, snaps, "mariadb_active_sessions_by_schema",
		map[string]string{"schema_name": "vendas", "command": "Sleep"})
	require.Equal(t, float64(12), sleep.Value)

	require.NoError(t, mock.ExpectationsWereMet())
}

// Com mais de uma coluna de valor, o nome da coluna é sufixado para evitar
// colisão de nomes de métrica.
func TestCustomMetricsCollectorMultipleValueColumns(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_custom_io:
  query: SELECT 'vendas' AS schema_name, 10 AS reads, 20 AS writes
  metrics:
    - schema_name:
        usage: "LABEL"
    - reads:
        usage: "COUNTER"
        description: "Leituras"
    - writes:
        usage: "COUNTER"
        description: "Escritas"
`)

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures())
	require.NoError(t, err)

	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT 'vendas'").
		WillReturnRows(sqlmock.NewRows([]string{"schema_name", "reads", "writes"}).AddRow("vendas", 10, 20))

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	labels := map[string]string{"schema_name": "vendas"}

	require.Equal(t, float64(10), requireMetric(t, snaps, "mariadb_custom_io_reads", labels).Value)
	require.Equal(t, float64(20), requireMetric(t, snaps, "mariadb_custom_io_writes", labels).Value)
}

func TestCustomMetricsCollectorInvalidUsage(t *testing.T) {
	path := writeTempYAML(t, `
metrica_ruim:
  query: SELECT 1 AS total
  metrics:
    - total:
        usage: "HISTOGRAM"
`)

	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures())
	require.Error(t, err)
	require.Contains(t, err.Error(), "usage")
}

func TestCustomMetricsCollectorMissingQuery(t *testing.T) {
	path := writeTempYAML(t, `
metrica_sem_query:
  metrics:
    - total:
        usage: "GAUGE"
`)

	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures())
	require.Error(t, err)
	require.Contains(t, err.Error(), "query")
}

// Sem nenhuma coluna de valor a métrica não faz sentido.
func TestCustomMetricsCollectorOnlyLabels(t *testing.T) {
	path := writeTempYAML(t, `
so_labels:
  query: SELECT 'x' AS nome
  metrics:
    - nome:
        usage: "LABEL"
`)

	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures())
	require.Error(t, err)
	require.Contains(t, err.Error(), "COUNTER ou GAUGE")
}

func TestCustomMetricsCollectorFileNotFound(t *testing.T) {
	_, err := NewCustomMetricsCollector([]string{filepath.Join(t.TempDir(), "inexistente.yml")}, testLogger(), allFeatures())
	require.Error(t, err)
}

// Uma coluna declarada no YAML que não existe no result set deve gerar erro
// claro em vez de pânico por índice inválido.
func TestCustomMetricsCollectorColumnMismatch(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_col_errada:
  query: SELECT 1 AS existe
  metrics:
    - inexistente:
        usage: "GAUGE"
`)

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures())
	require.NoError(t, err)

	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"existe"}).AddRow(1))

	metrics, err := runCollect(t, c, db)
	require.Error(t, err)
	require.Contains(t, err.Error(), "não existe no result set")
	require.Empty(t, metrics)
}

// Sem arquivos, o coletor fica desabilitado.
func TestCustomMetricsCollectorDisabledWithoutFiles(t *testing.T) {
	c, err := NewCustomMetricsCollector(nil, testLogger(), allFeatures())
	require.NoError(t, err)
	require.False(t, c.Enabled())
}

// Múltiplos arquivos são combinados (--custom-metrics=a.yml --custom-metrics=b.yml).
func TestCustomMetricsCollectorMultipleFiles(t *testing.T) {
	dir := t.TempDir()

	a := filepath.Join(dir, "a.yml")
	require.NoError(t, os.WriteFile(a, []byte("mariadb_a:\n  query: SELECT 1 AS v\n  metrics:\n    - v:\n        usage: \"GAUGE\"\n"), 0o600))

	b := filepath.Join(dir, "b.yml")
	require.NoError(t, os.WriteFile(b, []byte("mariadb_b:\n  query: SELECT 2 AS v\n  metrics:\n    - v:\n        usage: \"GAUGE\"\n"), 0o600))

	c, err := NewCustomMetricsCollector([]string{a, b}, testLogger(), allFeatures())
	require.NoError(t, err)
	require.Len(t, c.metrics, 2)

	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(1))
	mock.ExpectQuery("SELECT 2").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(2))

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_a", nil).Value)
	require.Equal(t, float64(2), requireMetric(t, snaps, "mariadb_b", nil).Value)
}
