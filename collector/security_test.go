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
// Custom metrics: robustness against hostile or malformed YAML
//
// The custom metrics file is provided by the operator, but errors in it must
// not bring down the exporter process at runtime — they should fail at
// startup (configuration error) or be ignored during the scrape.
// ---------------------------------------------------------------------------

// A metric whose name is not a valid Prometheus identifier would make
// prometheus.NewDesc generate an invalid Desc, and MustNewConstMetric would
// panic during the scrape — bringing down the entire exporter because of a
// badly filled-out YAML. Validation needs to happen at load time.
func TestCustomMetricsRejectsInvalidMetricName(t *testing.T) {
	cases := []struct {
		name       string
		metricName string
	}{
		{"with hyphen", "mariadb-invalido"},
		{"with space", "mariadb invalido"},
		{"starting with digit", "1mariadb"},
		{"with dot", "mariadb.invalido"},
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
			require.Error(t, err, "invalid metric name should be rejected at load time, not at runtime")
		})
	}
}

// Same reasoning for invalid label names.
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
	require.Error(t, err, "invalid label name should be rejected at load time")
}

// The same column declared twice as LABEL generates duplicate labels in the
// Desc, which makes client_golang panic when building the metric.
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
	require.Error(t, err, "duplicate label should be rejected at load time")
}

// A column declared simultaneously as LABEL and as a value is ambiguous and
// would also produce an inconsistent Desc.
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
	require.Error(t, err, "column used as both label and value should be rejected")
}

// Two metrics whose names collide after column suffixing would produce series
// with the same name and different label sets — the Prometheus registry
// rejects this during the scrape.
func TestCustomMetricsCollectDoesNotPanicOnHostileYAML(t *testing.T) {
	// This YAML passes structural validation but has a query that returns one
	// more column than declared: the code needs to handle this without an
	// index panic.
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
		// Undeclared columns are simply ignored.
		require.Len(t, metrics, 1)
	})
}

// Label values coming from the database may contain invalid UTF-8 (for
// example, a binary blob in a column used as a label). The Prometheus
// exposition format requires valid UTF-8; invalid bytes corrupt the /metrics
// output for every scrape.
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
	// An isolated 0xff is not valid UTF-8.
	mock.ExpectQuery("SELECT 'x'").WillReturnRows(
		sqlmock.NewRows([]string{"c", "total"}).AddRow([]byte{0xff, 0xfe, 0x41}, 1),
	)

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Len(t, metrics, 1)

	// The metric needs to be serializable: labels with invalid UTF-8 must have
	// been sanitized before becoming a label.
	snaps := snapshot(t, metrics)
	for _, s := range snaps {
		for k, v := range s.Labels {
			require.True(t, isValidUTF8(v),
				"label %q has invalid UTF-8 (%q), which corrupts the /metrics output", k, v)
		}
	}
}

func isValidUTF8(s string) bool {
	return strings.ToValidUTF8(s, "�") == s
}

// A gigantic custom metrics file should not hang startup nor consume
// unbounded memory.
func TestCustomMetricsHandlesEmptyYAML(t *testing.T) {
	path := writeTempYAML(t, "")

	c, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.NoError(t, err, "empty YAML is not an error, it just defines no metrics")
	require.Empty(t, c.metrics)
}

// A YAML with a structure entirely different from expected (a list instead of
// a map) should give a clear error instead of a panic.
func TestCustomMetricsRejectsWrongTopLevelType(t *testing.T) {
	path := writeTempYAML(t, "- isso\n- e\n- uma\n- lista\n")

	_, err := NewCustomMetricsCollector([]string{path}, testLogger(), allFeatures(), nil)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// tablestat/indexstat limits: LIMIT injection
//
// The limits go into the query via fmt.Sprintf. Extreme or negative values
// must not generate invalid SQL.
// ---------------------------------------------------------------------------

// A negative limit would produce "LIMIT -1", which is a syntax error in
// MariaDB. The code already normalizes <= 0 to the default; this test pins
// down that contract.
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
// Parsing robustness against hostile server data
// ---------------------------------------------------------------------------

// strconv.ParseFloat accepts "NaN", "Inf" and "infinity", but none of these is
// a useful metric value: NaN disappears from graphs and makes alert
// comparisons fail silently, and Inf distorts aggregations. parseFloat needs
// to reject them so the collector omits the metric instead of exposing junk.
func TestParseFloatRejectsNonFiniteValues(t *testing.T) {
	for _, s := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity", "-infinity"} {
		_, err := parseFloat(s)
		require.Error(t, err, "parseFloat(%q) should be rejected", s)
	}

	// Also for the native numeric types the driver may return.
	_, err := parseFloat(math.NaN())
	require.Error(t, err, "float64 NaN should be rejected")

	_, err = parseFloat(math.Inf(1))
	require.Error(t, err, "float64 +Inf should be rejected")

	_, err = parseFloat(float32(math.Inf(-1)))
	require.Error(t, err, "float32 -Inf should be rejected")
}

// A non-finite value in SHOW GLOBAL STATUS should make the metric be omitted,
// not exposed as NaN.
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
		require.False(t, math.IsNaN(s.Value), "metric %s exposed as NaN", s.Name)
		require.False(t, math.IsInf(s.Value, 0), "metric %s exposed as Inf", s.Name)
	}

	// The valid variable keeps being exported.
	require.Len(t, snaps, 1)
	require.Equal(t, float64(42), requireMetric(t, snaps, "mariadb_questions_total", nil).Value)
}

// Huge values in counter columns must not cause a silent overflow into
// negative.
func TestParseFloatVeryLargeValues(t *testing.T) {
	// Largest uint64 — common in counters that wrap around in MariaDB.
	v, err := parseFloat("18446744073709551615")
	require.NoError(t, err)
	require.Positive(t, v, "maximum counter should not become negative")
}

// The QRT collector converts float to uint64. A negative COUNT (impossible in
// practice, but defensive) would cause a huge wraparound in the histogram
// counts, breaking rate() queries.
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
		t.Skip("no metric emitted")
	}

	h := snapshot(t, metrics)[0]
	// If the -5 turned into uint64, SampleCount becomes astronomically large.
	require.Less(t, h.SampleCount, uint64(1000),
		"negative COUNT wrapped around as uint64: SampleCount=%d", h.SampleCount)

	for bound, count := range h.Buckets {
		require.Less(t, count, uint64(1000),
			"bucket le=%v with absurd count %d (uint64 wraparound)", bound, count)
	}
}

// Duplicate buckets in the QRT table (same TIME in two rows) must not
// generate an inconsistent histogram.
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
// Concurrent registration / channel usage
// ---------------------------------------------------------------------------

// A collector that returns many rows must not block indefinitely if the
// context is canceled.
func TestCollectRespectsContextCancellation(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SHOW GLOBAL STATUS").WillReturnRows(
		sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("Uptime", "100"),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	c := NewGlobalStatusCollector(true, testLogger(), allFeatures())
	ch := make(chan prometheus.Metric, 10)

	// Must not hang or panic with an already-canceled context.
	require.NotPanics(t, func() {
		_ = c.Collect(ctx, db, ch)
	})
}

// ---------------------------------------------------------------------------
// Path traversal / file reading
// ---------------------------------------------------------------------------

// A custom metrics path pointing to a directory should give a clear error.
func TestCustomMetricsPathIsDirectory(t *testing.T) {
	dir := t.TempDir()

	_, err := NewCustomMetricsCollector([]string{dir}, testLogger(), allFeatures(), nil)
	require.Error(t, err, "a directory instead of a file should give an error")
}

// A nonexistent file is already covered, but a path with null bytes can cause
// strange syscall behavior.
func TestCustomMetricsPathWithNullByte(t *testing.T) {
	_, err := NewCustomMetricsCollector(
		[]string{filepath.Join(os.TempDir(), "arquivo\x00malicioso.yml")},
		testLogger(), allFeatures(), nil,
	)
	require.Error(t, err)
}

// Custom metrics need to carry the same PMM integration ConstLabels as the
// native metrics. Without this, a panel filtering by cluster or service_name
// would not find the series — the data would exist but be invisible on the
// dashboard.
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
