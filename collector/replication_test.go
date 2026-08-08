package collector

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// allSlavesColumns é um subconjunto representativo das colunas de
// SHOW ALL SLAVES STATUS no MariaDB.
var allSlavesColumns = []string{
	"Connection_name", "Slave_SQL_State", "Slave_IO_State", "Master_Host", "Master_User",
	"Master_Port", "Slave_IO_Running", "Slave_SQL_Running", "Relay_Log_Pos",
	"Last_Errno", "Last_Error", "Seconds_Behind_Master",
}

// Multi-source: duas conexões de replicação devem gerar séries distintas
// separadas pelo label connection_name.
func TestReplicationCollectorMultiSource(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows(allSlavesColumns).
		AddRow("dc1", "Slave has read all relay log", "Waiting for master to send event",
			"10.0.0.10", "repl", 3306, "Yes", "Yes", 4711, 0, "", 0).
		AddRow("dc2", "Slave has read all relay log", "Waiting for master to send event",
			"10.0.0.20", "repl", 3307, "Yes", "No", 991, 1236, "got fatal error", 45)

	mock.ExpectQuery("SHOW ALL SLAVES STATUS").WillReturnRows(rows)

	c := NewReplicationCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)

	dc1 := map[string]string{"connection_name": "dc1", "master_host": "10.0.0.10", "master_port": "3306"}
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_slave_sql_running", dc1).Value)
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_slave_io_running", dc1).Value)
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_slave_seconds_behind_master", dc1).Value)
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_slave_last_errno", dc1).Value)
	require.Equal(t, float64(4711), requireMetric(t, snaps, "mariadb_slave_relay_log_pos", dc1).Value)

	dc2 := map[string]string{"connection_name": "dc2", "master_host": "10.0.0.20", "master_port": "3307"}
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_slave_sql_running", dc2).Value)
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_slave_io_running", dc2).Value)
	require.Equal(t, float64(45), requireMetric(t, snaps, "mariadb_slave_seconds_behind_master", dc2).Value)
	require.Equal(t, float64(1236), requireMetric(t, snaps, "mariadb_slave_last_errno", dc2).Value)

	require.NoError(t, mock.ExpectationsWereMet())
}

// Instância que não é réplica: zero linhas, zero métricas, sem erro.
func TestReplicationCollectorNoSlaves(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SHOW ALL SLAVES STATUS").WillReturnRows(sqlmock.NewRows(allSlavesColumns))

	c := NewReplicationCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Servidor sem SHOW ALL SLAVES STATUS deve cair no SHOW SLAVE STATUS.
func TestReplicationCollectorFallbackToShowSlaveStatus(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SHOW ALL SLAVES STATUS").
		WillReturnError(errors.New("You have an error in your SQL syntax"))

	rows := sqlmock.NewRows([]string{
		"Slave_IO_State", "Master_Host", "Master_Port", "Slave_IO_Running",
		"Slave_SQL_Running", "Relay_Log_Pos", "Last_Errno", "Seconds_Behind_Master",
	}).AddRow("Waiting for master to send event", "10.0.0.10", 3306, "Yes", "Yes", 1500, 0, 2)

	mock.ExpectQuery("SHOW SLAVE STATUS").WillReturnRows(rows)

	c := NewReplicationCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)

	// Sem Connection_name, o label fica vazio (conexão default).
	labels := map[string]string{"connection_name": "", "master_host": "10.0.0.10", "master_port": "3306"}
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_slave_sql_running", labels).Value)
	require.Equal(t, float64(2), requireMetric(t, snaps, "mariadb_slave_seconds_behind_master", labels).Value)

	require.NoError(t, mock.ExpectationsWereMet())
}

// Seconds_Behind_Master NULL (replicação parada) deve omitir a métrica em vez de
// reportar 0, que seria interpretado como "réplica em dia".
func TestReplicationCollectorNullSecondsBehind(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows(allSlavesColumns).
		AddRow("", "", "", "10.0.0.10", "repl", 3306, "No", "No", 0, 1593, "erro", nil)

	mock.ExpectQuery("SHOW ALL SLAVES STATUS").WillReturnRows(rows)

	c := NewReplicationCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	requireNoMetric(t, snaps, "mariadb_slave_seconds_behind_master")

	labels := map[string]string{"master_host": "10.0.0.10"}
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_slave_sql_running", labels).Value)
	require.Equal(t, float64(1593), requireMetric(t, snaps, "mariadb_slave_last_errno", labels).Value)
}

// Slave_IO_Running = "Connecting" conta como não rodando.
func TestReplicationCollectorConnectingIsNotRunning(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows(allSlavesColumns).
		AddRow("", "", "", "10.0.0.10", "repl", 3306, "Connecting", "Yes", 0, 0, "", 0)

	mock.ExpectQuery("SHOW ALL SLAVES STATUS").WillReturnRows(rows)

	c := NewReplicationCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	labels := map[string]string{"master_host": "10.0.0.10"}
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_slave_io_running", labels).Value)
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_slave_sql_running", labels).Value)
}
