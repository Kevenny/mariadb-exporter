package collector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// writeTempYAML writes a temporary YAML file and returns its path.
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "custom.yml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// This YAML is the example from section 10 of the specification.
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

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err)
	require.True(t, c.Enabled())

	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"schema_name", "command", "total"}).
		AddRow("vendas", "Query", 5).
		AddRow("vendas", "Sleep", 12)

	mock.ExpectBegin()
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

// With more than one value column, the column name is suffixed to avoid
// metric name collisions.
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

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err)

	db, mock := newMockDB(t)
	mock.ExpectBegin()
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

	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
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

	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "query")
}

// Without any value column, the metric makes no sense.
func TestCustomMetricsCollectorOnlyLabels(t *testing.T) {
	path := writeTempYAML(t, `
so_labels:
  query: SELECT 'x' AS nome
  metrics:
    - nome:
        usage: "LABEL"
`)

	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "COUNTER or GAUGE")
}

func TestCustomMetricsCollectorFileNotFound(t *testing.T) {
	_, err := NewCustomMetricsCollector([]string{filepath.Join(t.TempDir(), "inexistente.yml")}, testLogger(), allFeatures(), nil)
	require.Error(t, err)
}

// A column declared in the YAML that does not exist in the result set should
// generate a clear error instead of a panic from an invalid index.
func TestCustomMetricsCollectorColumnMismatch(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_col_errada:
  query: SELECT 1 AS existe
  metrics:
    - inexistente:
        usage: "GAUGE"
`)

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err)

	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"existe"}).AddRow(1))

	metrics, err := runCollect(t, c, db)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist in the query's result set")
	require.Empty(t, metrics)
}

// Without files, the collector is disabled.
func TestCustomMetricsCollectorDisabledWithoutFiles(t *testing.T) {
	c, err := NewCustomMetricsCollector(nil, testLogger(), allFeatures(), nil)
	require.NoError(t, err)
	require.False(t, c.Enabled())
}

// Multiple files are combined (--custom-metrics=a.yml --custom-metrics=b.yml).
func TestCustomMetricsCollectorMultipleFiles(t *testing.T) {
	dir := t.TempDir()

	a := filepath.Join(dir, "a.yml")
	require.NoError(t, os.WriteFile(a, []byte("mariadb_a:\n  query: SELECT 1 AS v\n  metrics:\n    - v:\n        usage: \"GAUGE\"\n"), 0o600))

	b := filepath.Join(dir, "b.yml")
	require.NoError(t, os.WriteFile(b, []byte("mariadb_b:\n  query: SELECT 2 AS v\n  metrics:\n    - v:\n        usage: \"GAUGE\"\n"), 0o600))

	c, err := NewCustomMetricsCollector([]string{a, b}, testLogger(), allFeatures(), nil)
	require.NoError(t, err)
	require.Len(t, c.metrics, 2)

	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(1))
	mock.ExpectQuery("SELECT 2").WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(2))

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_a", nil).Value)
	require.Equal(t, float64(2), requireMetric(t, snaps, "mariadb_b", nil).Value)
}

// Custom queries run inside a READ ONLY transaction that is always rolled back.
func TestCustomMetricsRunInReadOnlyTransaction(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_ro_check:
  query: SELECT 1 AS total
  metrics:
    - total:
        usage: "GAUGE"
`)
	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err)

	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"total"}).AddRow(1))
	mock.ExpectRollback()

	_, err = runCollect(t, c, db)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// A query returning more rows than the cap emits up to the cap and reports
// an error, so the cardinality blow-up is visible in mariadb_scrape_errors_total.
func TestCustomMetricsMaxRows(t *testing.T) {
	path := writeTempYAML(t, `
mariadb_many_rows:
  query: SELECT name, total FROM t
  metrics:
    - name:
        usage: "LABEL"
    - total:
        usage: "GAUGE"
`)
	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err)
	c.SetMaxRows(2)

	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT name").WillReturnRows(
		sqlmock.NewRows([]string{"name", "total"}).AddRow("a", 1).AddRow("b", 2).AddRow("c", 3),
	)

	metrics, err := runCollect(t, c, db)
	require.ErrorContains(t, err, "more than 2 rows")
	require.Len(t, metrics, 2)
}

func TestCustomMetricsRejectsWriteStatements(t *testing.T) {
	cases := map[string]string{
		"update":             "UPDATE t SET x = 1",
		"delete in comment":  "/* SELECT */ DELETE FROM t",
		"executable comment": "/*!50000 DELETE FROM t */ SELECT 1",
		"mariadb exec cmt":   "/*M!100000 DROP TABLE t */",
		"only a comment":     "-- SELECT 1",
		"call":               "CALL do_things()",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTempYAML(t, "mariadb_bad:\n  query: \""+query+"\"\n  metrics:\n    - total:\n        usage: \"GAUGE\"\n")
			_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
			require.ErrorContains(t, err, "must be a SELECT")
		})
	}
}

func TestLeadingKeyword(t *testing.T) {
	cases := map[string]string{
		"SELECT 1":                      "SELECT",
		"  \n\tselect 1":                "SELECT",
		"(SELECT 1) UNION (SELECT 2)":   "SELECT",
		"-- note\n# other\nSHOW STATUS": "SHOW",
		"/* a */ /* b */ WITH x AS (SELECT 1) SELECT * FROM x": "WITH",
		"VALUES (1)":      "VALUES",
		"/*!SELECT 1*/":   "/*!",
		"/* unterminated": "",
	}
	for query, want := range cases {
		require.Equal(t, want, leadingKeyword(query), query)
	}
}
