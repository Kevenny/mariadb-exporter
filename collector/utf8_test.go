package collector

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// Labels vindos do banco podem conter bytes que não formam UTF-8 válido: nomes
// de usuário, tabela ou índice em latin1, ou um blob binário. O client_golang
// entra em pânico ao construir uma métrica com label inválido, o que derrubaria
// o processo inteiro do exporter durante um scrape.
//
// Estes testes cobrem os coletores nativos cujos labels vêm direto do servidor.

func TestUserStatInvalidUTF8InUserLabel(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"USER", "TOTAL_CONNECTIONS", "CONCURRENT_CONNECTIONS", "ROWS_READ", "ROWS_SENT",
		"ROWS_CHANGED", "SELECT_COMMANDS", "UPDATE_COMMANDS", "OTHER_COMMANDS",
		"ACCESS_DENIED", "LOST_CONNECTIONS",
	}).AddRow([]byte{0xff, 0xfe, 'u', 's', 'r'}, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0)

	mock.ExpectQuery("USER_STATISTICS").WillReturnRows(rows)

	c := NewUserStatCollector(true, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido no label user derrubou o coletor")
}

func TestTableStatInvalidUTF8InLabels(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"TABLE_SCHEMA", "TABLE_NAME", "ROWS_READ", "ROWS_CHANGED", "ROWS_CHANGED_X_INDEXES",
	}).AddRow([]byte{0xc3, 0x28}, []byte{0xa0, 0xa1}, 10, 2, 4)

	mock.ExpectQuery("TABLE_STATISTICS").WillReturnRows(rows)

	c := NewTableStatCollector(true, 500, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido em schema/table derrubou o coletor")
}

func TestIndexStatInvalidUTF8InLabels(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"TABLE_SCHEMA", "TABLE_NAME", "INDEX_NAME", "ROWS_READ",
	}).AddRow("demo", "pedidos", []byte{0xf0, 0x28, 0x8c, 0x28}, 0)

	mock.ExpectQuery("INDEX_STATISTICS").WillReturnRows(rows)

	c := NewIndexStatCollector(true, 1000, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido no nome do índice derrubou o coletor")
}

func TestClientStatInvalidUTF8InLabel(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"CLIENT", "TOTAL_CONNECTIONS", "ROWS_READ", "ROWS_SENT"}).
		AddRow([]byte{0xff}, 1, 2, 3)

	mock.ExpectQuery("CLIENT_STATISTICS").WillReturnRows(rows)

	c := NewClientStatCollector(true, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido no label client derrubou o coletor")
}

func TestInfoCollectorInvalidUTF8InLabel(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"VERSION()", "version_comment", "hostname", "server_id"}).
		AddRow("11.4.3-MariaDB", []byte{0xff, 0xfe}, "host", "1")

	mock.ExpectQuery("SELECT VERSION").WillReturnRows(rows)

	c := NewInfoCollector(testLogger(), allFeatures(), nil)

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido no version_comment derrubou o coletor")
}

func TestMetadataLocksInvalidUTF8InLabels(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"LOCK_MODE", "LOCK_TYPE", "TABLE_SCHEMA", "TABLE_NAME"}).
		AddRow("MDL_SHARED_READ", "Table metadata lock", "demo", []byte{0x80, 0x81})

	mock.ExpectQuery("METADATA_LOCK_INFO").WillReturnRows(rows)

	c := NewMetadataLocksCollector(true, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido no nome da tabela derrubou o coletor")
}

func TestDisksInvalidUTF8InLabels(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Disk", "Path", "Total", "Used", "Available"}).
		AddRow([]byte{0xfe, 0xff}, "/", 100, 50, 50)

	mock.ExpectQuery("information_schema.DISKS").WillReturnRows(rows)

	c := NewDisksCollector(true, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido no nome do disco derrubou o coletor")
}

func TestReplicationInvalidUTF8InLabels(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows(allSlavesColumns).
		AddRow([]byte{0xff, 0x01}, "", "", "10.0.0.10", "repl", 3306, "Yes", "Yes", 1, 0, "", 0)

	mock.ExpectQuery("SHOW ALL SLAVES STATUS").WillReturnRows(rows)

	c := NewReplicationCollector(true, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido no connection_name derrubou o coletor")
}

func TestGaleraInvalidUTF8InClusterName(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Variable_name", "Value"}).
		AddRow("wsrep_cluster_name", []byte{0xff, 0xfe}).
		AddRow("wsrep_cluster_size", "3")

	mock.ExpectQuery("wsrep_").WillReturnRows(rows)

	c := NewGaleraCollector(true, testLogger(), allFeatures())

	require.NotPanics(t, func() {
		metrics, err := runCollect(t, c, db)
		require.NoError(t, err)
		requireAllLabelsValidUTF8(t, snapshot(t, metrics))
	}, "UTF-8 inválido no wsrep_cluster_name derrubou o coletor")
}

// requireAllLabelsValidUTF8 confere que nenhum label emitido tem UTF-8
// inválido — bytes inválidos corrompem a saída de /metrics para todos os
// scrapes, não só para a série afetada.
func requireAllLabelsValidUTF8(t *testing.T, snaps []metricSnapshot) {
	t.Helper()
	for _, s := range snaps {
		for k, v := range s.Labels {
			require.True(t, isValidUTF8(v),
				"métrica %s: label %q com UTF-8 inválido (%q)", s.Name, k, v)
		}
	}
}
