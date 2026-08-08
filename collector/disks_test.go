package collector

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// The DISKS plugin reports in kibibytes; the exporter exposes bytes.
func TestDisksCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"Disk", "Path", "Total", "Used", "Available"}).
		AddRow("/dev/sda1", "/", 104857600, 52428800, 52428800).
		AddRow("/dev/sdb1", "/var/lib/mysql", 209715200, 10485760, 199229440)

	mock.ExpectQuery("SELECT .* FROM information_schema.DISKS").WillReturnRows(rows)

	c := NewDisksCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	root := map[string]string{"disk": "/dev/sda1", "path": "/"}

	require.Equal(t, float64(104857600)*1024, requireMetric(t, snaps, "mariadb_disk_total_bytes", root).Value)
	require.Equal(t, float64(52428800)*1024, requireMetric(t, snaps, "mariadb_disk_used_bytes", root).Value)
	require.Equal(t, float64(52428800)*1024, requireMetric(t, snaps, "mariadb_disk_available_bytes", root).Value)

	datadir := map[string]string{"disk": "/dev/sdb1", "path": "/var/lib/mysql"}
	require.Equal(t, float64(10485760)*1024, requireMetric(t, snaps, "mariadb_disk_used_bytes", datadir).Value)

	require.Len(t, snaps, 6)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisksCollectorPluginInactive(t *testing.T) {
	db, mock := newMockDB(t)

	c := NewDisksCollector(true, testLogger(), noFeatures())
	require.False(t, c.Available())

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMetadataLocksCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"LOCK_MODE", "LOCK_TYPE", "TABLE_SCHEMA", "TABLE_NAME"}).
		AddRow("MDL_SHARED_READ", "Table metadata lock", "vendas", "pedidos").
		AddRow("MDL_SHARED_READ", "Table metadata lock", "vendas", "pedidos").
		AddRow("MDL_EXCLUSIVE", "Table metadata lock", "vendas", "itens")

	mock.ExpectQuery("SELECT .* FROM information_schema.METADATA_LOCK_INFO").WillReturnRows(rows)

	c := NewMetadataLocksCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)

	// Two identical rows are aggregated into a single series with value 2.
	shared := map[string]string{
		"lock_mode": "MDL_SHARED_READ", "lock_type": "Table metadata lock",
		"table_schema": "vendas", "table_name": "pedidos",
	}
	require.Equal(t, float64(2), requireMetric(t, snaps, "mariadb_metadata_locks_total", shared).Value)
	require.Equal(t, float64(0), requireMetric(t, snaps, "mariadb_metadata_lock_waiting_total", shared).Value)

	exclusive := map[string]string{"lock_mode": "MDL_EXCLUSIVE", "table_name": "itens"}
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_metadata_locks_total", exclusive).Value)
}

// Locks whose mode indicates waiting should also appear in the waiting metric.
func TestMetadataLocksCollectorWaiting(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"LOCK_MODE", "LOCK_TYPE", "TABLE_SCHEMA", "TABLE_NAME"}).
		AddRow("MDL_SHARED_WRITE_WAIT", "Table metadata lock", "vendas", "pedidos")

	mock.ExpectQuery("METADATA_LOCK_INFO").WillReturnRows(rows)

	c := NewMetadataLocksCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	labels := map[string]string{"lock_mode": "MDL_SHARED_WRITE_WAIT", "table_name": "pedidos"}

	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_metadata_locks_total", labels).Value)
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_metadata_lock_waiting_total", labels).Value)
}

func TestMetadataLocksCollectorPluginInactive(t *testing.T) {
	db, mock := newMockDB(t)

	c := NewMetadataLocksCollector(true, testLogger(), noFeatures())
	metrics, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Locks that are not table-scoped (GLOBAL, SCHEMA) come with empty
// schema/table.
func TestMetadataLocksCollectorGlobalLock(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"LOCK_MODE", "LOCK_TYPE", "TABLE_SCHEMA", "TABLE_NAME"}).
		AddRow("MDL_INTENTION_EXCLUSIVE", "Global read lock", nil, nil)

	mock.ExpectQuery("METADATA_LOCK_INFO").WillReturnRows(rows)

	c := NewMetadataLocksCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	snaps := snapshot(t, metrics)
	labels := map[string]string{
		"lock_mode": "MDL_INTENTION_EXCLUSIVE", "lock_type": "Global read lock",
		"table_schema": "", "table_name": "",
	}
	require.Equal(t, float64(1), requireMetric(t, snaps, "mariadb_metadata_locks_total", labels).Value)
}
