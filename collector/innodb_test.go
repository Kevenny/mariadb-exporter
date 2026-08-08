package collector

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestInnoDBCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("Innodb_buffer_pool_read_requests", "9876543").
		AddRow("Innodb_buffer_pool_reads", "1234").
		AddRow("Innodb_buffer_pool_pages_free", "5000").
		AddRow("Innodb_buffer_pool_pages_data", "60000").
		AddRow("Innodb_buffer_pool_pages_dirty", "250").
		AddRow("Innodb_row_lock_waits", "17").
		AddRow("Innodb_row_lock_time_avg", "35").
		AddRow("Innodb_deadlocks", "4").
		AddRow("Innodb_pages_written", "999")

	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'Innodb_%'").WillReturnRows(rows)

	c := NewInnoDBCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)

	require.Equal(t, float64(9876543), requireMetric(t, snaps, "mariadb_innodb_buffer_pool_read_requests_total", nil).Value)
	require.Equal(t, float64(1234), requireMetric(t, snaps, "mariadb_innodb_buffer_pool_reads_total", nil).Value)
	require.Equal(t, float64(17), requireMetric(t, snaps, "mariadb_innodb_row_lock_waits_total", nil).Value)
	require.Equal(t, float64(35), requireMetric(t, snaps, "mariadb_innodb_row_lock_time_avg_milliseconds", nil).Value)
	require.Equal(t, float64(4), requireMetric(t, snaps, "mariadb_innodb_deadlocks_total", nil).Value)

	// Páginas do buffer pool são diferenciadas pelo label type.
	require.Equal(t, float64(5000), requireMetric(t, snaps, "mariadb_innodb_buffer_pool_pages_total", map[string]string{"type": "free"}).Value)
	require.Equal(t, float64(60000), requireMetric(t, snaps, "mariadb_innodb_buffer_pool_pages_total", map[string]string{"type": "data"}).Value)
	require.Equal(t, float64(250), requireMetric(t, snaps, "mariadb_innodb_buffer_pool_pages_total", map[string]string{"type": "dirty"}).Value)

	// Innodb_pages_written não está mapeada e deve ser ignorada.
	require.Len(t, snaps, 8)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Sem Innodb_deadlocks no SHOW STATUS, o coletor recorre ao texto de
// SHOW ENGINE INNODB STATUS.
func TestInnoDBCollectorDeadlockFallback(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("Innodb_buffer_pool_reads", "10")

	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'Innodb_%'").WillReturnRows(rows)

	status := "------------\nTRANSACTIONS\n------------\nNumber of deadlocks 9\nTrx id counter 12345\n"
	mock.ExpectQuery("SHOW ENGINE INNODB STATUS").
		WillReturnRows(sqlmock.NewRows([]string{"Type", "Name", "Status"}).AddRow("InnoDB", "", status))

	c := NewInnoDBCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	require.Equal(t, float64(9), requireMetric(t, snaps, "mariadb_innodb_deadlocks_total", nil).Value)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Se o fallback também não tiver a informação, nenhuma métrica de deadlock é
// emitida e o scrape continua sem erro.
func TestInnoDBCollectorDeadlockUnavailable(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'Innodb_%'").
		WillReturnRows(sqlmock.NewRows([]string{"Variable_name", "Value"}).AddRow("Innodb_buffer_pool_reads", "10"))
	mock.ExpectQuery("SHOW ENGINE INNODB STATUS").
		WillReturnError(errors.New("access denied"))

	c := NewInnoDBCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	requireNoMetric(t, snaps, "mariadb_innodb_deadlocks_total")
	require.Equal(t, float64(10), requireMetric(t, snaps, "mariadb_innodb_buffer_pool_reads_total", nil).Value)
}

func TestGaleraCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("wsrep_cluster_name", "prod-cluster").
		AddRow("wsrep_cluster_size", "3").
		AddRow("wsrep_cluster_status", "Primary").
		AddRow("wsrep_local_state", "4").
		AddRow("wsrep_flow_control_paused", "0.015").
		AddRow("wsrep_local_recv_queue_avg", "0.25").
		AddRow("wsrep_local_send_queue_avg", "0.10").
		AddRow("wsrep_ready", "ON")

	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'wsrep_%'").WillReturnRows(rows)

	c := NewGaleraCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	cluster := map[string]string{"cluster_name": "prod-cluster"}

	require.Equal(t, float64(3), requireMetric(t, snaps, "mariadb_galera_cluster_size", cluster).Value)
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_galera_cluster_status", cluster).Value, "Primary deve virar 1")

	// wsrep_local_state nativo 4 (synced) mapeia para 3 na escala da especificação.
	require.Equal(t, float64(3), requireMetric(t, snaps, "mariadb_galera_local_state", cluster).Value)

	require.InDelta(t, 0.015, requireMetric(t, snaps, "mariadb_galera_flow_control_paused", cluster).Value, 0.0001)
	require.InDelta(t, 0.25, requireMetric(t, snaps, "mariadb_galera_recv_queue_avg", cluster).Value, 0.0001)
	require.InDelta(t, 0.10, requireMetric(t, snaps, "mariadb_galera_send_queue_avg", cluster).Value, 0.0001)

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGaleraCollectorNonPrimary(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("wsrep_cluster_name", "prod-cluster").
		AddRow("wsrep_cluster_status", "non-Primary").
		AddRow("wsrep_local_state", "1")

	mock.ExpectQuery("SHOW GLOBAL STATUS LIKE 'wsrep_%'").WillReturnRows(rows)

	c := NewGaleraCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	cluster := map[string]string{"cluster_name": "prod-cluster"}

	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_galera_cluster_status", cluster).Value)
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_galera_local_state", cluster).Value, "state nativo 1 (joining) vira 0")
}

// Galera inativo: zero métricas, sem erro.
func TestGaleraCollectorInactive(t *testing.T) {
	db, mock := newMockDB(t)

	c := NewGaleraCollector(true, testLogger(), noFeatures())
	require.False(t, c.Available())

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet())
}

// O coletor galera é opt-in: desabilitado por padrão.
func TestGaleraCollectorIsOptIn(t *testing.T) {
	c := NewGaleraCollector(false, testLogger(), allFeatures())
	require.False(t, c.Enabled())
	require.Equal(t, "galera", c.Name())
}
