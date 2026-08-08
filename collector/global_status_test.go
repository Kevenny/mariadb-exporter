package collector

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestGlobalStatusCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("Connections", "15234").
		AddRow("Max_used_connections", "87").
		AddRow("Aborted_connects", "3").
		AddRow("Aborted_clients", "11").
		AddRow("Bytes_received", "998877").
		AddRow("Bytes_sent", "5544332211").
		AddRow("Questions", "77123").
		AddRow("Slow_queries", "42").
		AddRow("Open_files", "56").
		AddRow("Open_tables", "412").
		AddRow("Table_open_cache_hits", "90000").
		AddRow("Table_open_cache_misses", "120").
		AddRow("Created_tmp_tables", "800").
		AddRow("Created_tmp_disk_tables", "35").
		AddRow("Select_full_join", "7").
		AddRow("Select_scan", "1500").
		AddRow("Sort_merge_passes", "2").
		AddRow("Uptime", "864000").
		// Variáveis fora da lista explícita devem ser ignoradas.
		AddRow("Threads_connected", "9").
		AddRow("Ssl_cipher", "TLS_AES_256_GCM_SHA384")

	mock.ExpectQuery("SHOW GLOBAL STATUS").WillReturnRows(rows)

	c := NewGlobalStatusCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)

	require.Equal(t, float64(15234), requireMetric(t, snaps, "mariadb_connections_total", nil).Value)
	require.Equal(t, float64(87), requireMetric(t, snaps, "mariadb_max_used_connections", nil).Value)
	require.Equal(t, float64(3), requireMetric(t, snaps, "mariadb_aborted_connects_total", nil).Value)
	require.Equal(t, float64(11), requireMetric(t, snaps, "mariadb_aborted_clients_total", nil).Value)
	require.Equal(t, float64(998877), requireMetric(t, snaps, "mariadb_bytes_received_total", nil).Value)
	require.Equal(t, float64(5544332211), requireMetric(t, snaps, "mariadb_bytes_sent_total", nil).Value)
	require.Equal(t, float64(77123), requireMetric(t, snaps, "mariadb_questions_total", nil).Value)
	require.Equal(t, float64(42), requireMetric(t, snaps, "mariadb_slow_queries_total", nil).Value)
	require.Equal(t, float64(56), requireMetric(t, snaps, "mariadb_open_files", nil).Value)
	require.Equal(t, float64(412), requireMetric(t, snaps, "mariadb_open_tables", nil).Value)
	require.Equal(t, float64(90000), requireMetric(t, snaps, "mariadb_table_open_cache_hits_total", nil).Value)
	require.Equal(t, float64(120), requireMetric(t, snaps, "mariadb_table_open_cache_misses_total", nil).Value)
	require.Equal(t, float64(800), requireMetric(t, snaps, "mariadb_created_tmp_tables_total", nil).Value)
	require.Equal(t, float64(35), requireMetric(t, snaps, "mariadb_created_tmp_disk_tables_total", nil).Value)
	require.Equal(t, float64(7), requireMetric(t, snaps, "mariadb_select_full_join_total", nil).Value)
	require.Equal(t, float64(1500), requireMetric(t, snaps, "mariadb_select_scan_total", nil).Value)
	require.Equal(t, float64(2), requireMetric(t, snaps, "mariadb_sort_merge_passes_total", nil).Value)
	require.Equal(t, float64(864000), requireMetric(t, snaps, "mariadb_uptime_seconds", nil).Value)

	// Exatamente as 18 variáveis da lista explícita, nada de wildcard.
	require.Len(t, snaps, 18)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Valores não numéricos são ignorados sem derrubar o scrape.
func TestGlobalStatusCollectorIgnoresNonNumeric(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("Uptime", "100").
		AddRow("Open_files", "")

	mock.ExpectQuery("SHOW GLOBAL STATUS").WillReturnRows(rows)

	c := NewGlobalStatusCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	require.Len(t, snaps, 1)
	require.Equal(t, float64(100), requireMetric(t, snaps, "mariadb_uptime_seconds", nil).Value)
}

func TestGlobalVariablesCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("max_connections", "500").
		AddRow("innodb_buffer_pool_size", "8589934592").
		AddRow("query_cache_size", "1048576").
		AddRow("thread_cache_size", "100").
		AddRow("wait_timeout", "600").
		AddRow("interactive_timeout", "28800").
		AddRow("userstat", "ON").
		AddRow("version", "11.4.3-MariaDB")

	mock.ExpectQuery("SHOW GLOBAL VARIABLES").WillReturnRows(rows)

	c := NewGlobalVariablesCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)

	require.Equal(t, float64(500), requireMetric(t, snaps, "mariadb_max_connections", nil).Value)
	require.Equal(t, float64(8589934592), requireMetric(t, snaps, "mariadb_innodb_buffer_pool_size_bytes", nil).Value)
	require.Equal(t, float64(1048576), requireMetric(t, snaps, "mariadb_query_cache_size_bytes", nil).Value)
	require.Equal(t, float64(100), requireMetric(t, snaps, "mariadb_thread_cache_size", nil).Value)
	require.Equal(t, float64(600), requireMetric(t, snaps, "mariadb_wait_timeout_seconds", nil).Value)
	require.Equal(t, float64(28800), requireMetric(t, snaps, "mariadb_interactive_timeout_seconds", nil).Value)

	// userstat=ON deve virar 1.
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_userstat_enabled", nil).Value)

	require.Len(t, snaps, 7, "version não deve gerar métrica")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGlobalVariablesCollectorUserstatOff(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("userstat", "OFF")
	mock.ExpectQuery("SHOW GLOBAL VARIABLES").WillReturnRows(rows)

	c := NewGlobalVariablesCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_userstat_enabled", nil).Value)
}

func TestInfoCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"VERSION()", "version_comment", "hostname", "server_id"}).
		AddRow("11.4.3-MariaDB", "MariaDB Server", "db-host01", "1")

	mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnRows(rows)

	c := NewInfoCollector(testLogger(), allFeatures(), nil)
	require.True(t, c.Enabled(), "o coletor info não pode ser desabilitado")

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Len(t, metrics, 1)

	snaps := snapshot(t, metrics)
	info := requireMetric(t, snaps, "mariadb_info", map[string]string{
		"version":         "11.4.3-MariaDB",
		"version_comment": "MariaDB Server",
		"hostname":        "db-host01",
		"server_id":       "1",
	})

	require.Equal(t, float64(1), info.Value, "info metric deve valer sempre 1")
	require.NoError(t, mock.ExpectationsWereMet())
}

// Com ConstLabels de integração PMM informadas, elas devem aparecer em toda
// linha de mariadb_info além dos quatro labels dinâmicos normais (seção 3 de
// mariadb_exporter_pmm_integration.md).
func TestInfoCollectorWithPMMConstLabels(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"VERSION()", "version_comment", "hostname", "server_id"}).
		AddRow("11.4.3-MariaDB", "MariaDB Server", "db-host01", "1")

	mock.ExpectQuery("SELECT VERSION\\(\\)").WillReturnRows(rows)

	constLabels := prometheus.Labels{
		"service_name": "mariadb-host01",
		"cluster":      "prod-cluster",
		"environment":  "production",
	}
	c := NewInfoCollector(testLogger(), allFeatures(), constLabels)

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Len(t, metrics, 1)

	snaps := snapshot(t, metrics)
	info := requireMetric(t, snaps, "mariadb_info", map[string]string{
		"version":         "11.4.3-MariaDB",
		"version_comment": "MariaDB Server",
		"hostname":        "db-host01",
		"server_id":       "1",
		"service_name":    "mariadb-host01",
		"cluster":         "prod-cluster",
		"environment":     "production",
	})

	require.Equal(t, float64(1), info.Value)
	require.NoError(t, mock.ExpectationsWereMet())
}
