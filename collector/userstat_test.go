package collector

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestUserStatCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"USER", "TOTAL_CONNECTIONS", "CONCURRENT_CONNECTIONS", "ROWS_READ", "ROWS_SENT",
		"ROWS_CHANGED", "SELECT_COMMANDS", "UPDATE_COMMANDS", "OTHER_COMMANDS",
		"ACCESS_DENIED", "LOST_CONNECTIONS",
	}).AddRow("kevenny", 100, 3, 50000, 48000, 2000, 80, 15, 5, 0, 0).
		AddRow("app", 42, 1, 900, 850, 10, 20, 2, 1, 7, 3)

	mock.ExpectQuery("SELECT .* FROM information_schema.USER_STATISTICS").WillReturnRows(rows)

	c := NewUserStatCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	kevenny := map[string]string{"user": "kevenny"}

	require.Equal(t, float64(100), requireMetric(t, snaps, "mariadb_user_total_connections_total", kevenny).Value)
	require.Equal(t, float64(3), requireMetric(t, snaps, "mariadb_user_concurrent_connections", kevenny).Value)
	require.Equal(t, float64(50000), requireMetric(t, snaps, "mariadb_user_rows_read_total", kevenny).Value)
	require.Equal(t, float64(48000), requireMetric(t, snaps, "mariadb_user_rows_sent_total", kevenny).Value)
	require.Equal(t, float64(2000), requireMetric(t, snaps, "mariadb_user_rows_changed_total", kevenny).Value)
	require.Equal(t, float64(80), requireMetric(t, snaps, "mariadb_user_select_commands_total", kevenny).Value)
	require.Equal(t, float64(15), requireMetric(t, snaps, "mariadb_user_update_commands_total", kevenny).Value)
	require.Equal(t, float64(5), requireMetric(t, snaps, "mariadb_user_other_commands_total", kevenny).Value)
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_user_access_denied_total", kevenny).Value)
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_user_lost_connections_total", kevenny).Value)

	app := map[string]string{"user": "app"}
	require.Equal(t, float64(42), requireMetric(t, snaps, "mariadb_user_total_connections_total", app).Value)
	require.Equal(t, float64(7), requireMetric(t, snaps, "mariadb_user_access_denied_total", app).Value)

	// 10 métricas por usuário, 2 usuários.
	require.Len(t, snaps, 20)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Com userstat=OFF o coletor não deve nem executar a query, e deve retornar
// zero métricas sem erro (seção 20 da especificação).
func TestUserStatCollectorUserStatOff(t *testing.T) {
	db, mock := newMockDB(t)

	c := NewUserStatCollector(true, testLogger(), noFeatures())
	require.False(t, c.Available())

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet(), "nenhuma query deveria ter sido executada")
}

// Valores NULL são omitidos em vez de virarem zero.
func TestUserStatCollectorNullValues(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"USER", "TOTAL_CONNECTIONS", "CONCURRENT_CONNECTIONS", "ROWS_READ", "ROWS_SENT",
		"ROWS_CHANGED", "SELECT_COMMANDS", "UPDATE_COMMANDS", "OTHER_COMMANDS",
		"ACCESS_DENIED", "LOST_CONNECTIONS",
	}).AddRow("nulo", 10, nil, nil, 5, 0, 0, 0, 0, 0, 0)

	mock.ExpectQuery("SELECT .* FROM information_schema.USER_STATISTICS").WillReturnRows(rows)

	c := NewUserStatCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	labels := map[string]string{"user": "nulo"}

	require.Equal(t, float64(10), requireMetric(t, snaps, "mariadb_user_total_connections_total", labels).Value)
	requireNoMetric(t, snaps, "mariadb_user_concurrent_connections")
	requireNoMetric(t, snaps, "mariadb_user_rows_read_total")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClientStatCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"CLIENT", "TOTAL_CONNECTIONS", "ROWS_READ", "ROWS_SENT"}).
		AddRow("10.0.0.5", 12, 300, 280).
		AddRow("localhost", 4, 10, 9)

	mock.ExpectQuery("SELECT .* FROM information_schema.CLIENT_STATISTICS").WillReturnRows(rows)

	c := NewClientStatCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	ip := map[string]string{"client": "10.0.0.5"}

	require.Equal(t, float64(12), requireMetric(t, snaps, "mariadb_client_total_connections_total", ip).Value)
	require.Equal(t, float64(300), requireMetric(t, snaps, "mariadb_client_rows_read_total", ip).Value)
	require.Equal(t, float64(280), requireMetric(t, snaps, "mariadb_client_rows_sent_total", ip).Value)
	require.Len(t, snaps, 6)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClientStatCollectorUserStatOff(t *testing.T) {
	db, _ := newMockDB(t)

	c := NewClientStatCollector(true, testLogger(), noFeatures())
	metrics, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.Empty(t, metrics)
}
