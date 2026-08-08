package collector

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestTableStatCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ROWS_READ", "ROWS_CHANGED", "ROWS_CHANGED_X_INDEXES"}).
		AddRow("vendas", "pedidos", 900000, 1200, 3600).
		AddRow("vendas", "itens", 500000, 800, 1600)

	mock.ExpectQuery("SELECT .* FROM information_schema.TABLE_STATISTICS").WillReturnRows(rows)

	c := NewTableStatCollector(true, 500, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	pedidos := map[string]string{"schema": "vendas", "table": "pedidos"}

	require.Equal(t, float64(900000), requireMetric(t, snaps, "mariadb_table_rows_read_total", pedidos).Value)
	require.Equal(t, float64(1200), requireMetric(t, snaps, "mariadb_table_rows_changed_total", pedidos).Value)
	require.Equal(t, float64(3600), requireMetric(t, snaps, "mariadb_table_rows_changed_x_indexes_total", pedidos).Value)
	require.Len(t, snaps, 6)
	require.NoError(t, mock.ExpectationsWereMet())
}

// O limite configurado deve aparecer na cláusula LIMIT, e a ordenação por
// ROWS_READ DESC deve estar presente para que o corte preserve as tabelas mais
// movimentadas.
func TestTableStatCollectorAppliesLimitAndOrder(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`ORDER BY ROWS_READ DESC\s+LIMIT 25`).
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ROWS_READ", "ROWS_CHANGED", "ROWS_CHANGED_X_INDEXES"}))

	c := NewTableStatCollector(true, 25, testLogger(), allFeatures())
	_, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Limite zero ou negativo cai no padrão de 500 em vez de gerar "LIMIT 0".
func TestTableStatCollectorDefaultLimit(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`LIMIT 500`).
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ROWS_READ", "ROWS_CHANGED", "ROWS_CHANGED_X_INDEXES"}))

	c := NewTableStatCollector(true, 0, testLogger(), allFeatures())
	_, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTableStatCollectorUserStatOff(t *testing.T) {
	db, mock := newMockDB(t)

	c := NewTableStatCollector(true, 500, testLogger(), noFeatures())
	metrics, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Zero linhas não é erro e não emite métricas (seção 17).
func TestTableStatCollectorNoRows(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SELECT .* FROM information_schema.TABLE_STATISTICS").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "ROWS_READ", "ROWS_CHANGED", "ROWS_CHANGED_X_INDEXES"}))

	c := NewTableStatCollector(true, 500, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.Empty(t, metrics)
}

func TestIndexStatCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "ROWS_READ"}).
		AddRow("vendas", "pedidos", "PRIMARY", 750000).
		AddRow("vendas", "pedidos", "idx_cliente", 0)

	mock.ExpectQuery("SELECT .* FROM information_schema.INDEX_STATISTICS").WillReturnRows(rows)

	c := NewIndexStatCollector(true, 1000, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)

	usado := map[string]string{"schema": "vendas", "table": "pedidos", "index": "PRIMARY"}
	require.Equal(t, float64(750000), requireMetric(t, snaps, "mariadb_index_rows_read_total", usado).Value)

	// mariadb_index_unused só existe para o índice sem leituras, sempre com valor 1.
	naoUsado := map[string]string{"schema": "vendas", "table": "pedidos", "index": "idx_cliente"}
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_index_rows_read_total", naoUsado).Value)
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_index_unused", naoUsado).Value)

	_, ok := findMetric(snaps, "mariadb_index_unused", usado)
	require.False(t, ok, "índice com leituras não deve gerar mariadb_index_unused")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIndexStatCollectorDefaultLimit(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`LIMIT 1000`).
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "ROWS_READ"}))

	c := NewIndexStatCollector(true, 0, testLogger(), allFeatures())
	_, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestIndexStatCollectorUserStatOff(t *testing.T) {
	db, mock := newMockDB(t)

	c := NewIndexStatCollector(true, 1000, testLogger(), noFeatures())
	metrics, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet())
}
