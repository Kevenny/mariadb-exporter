package collector

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// A tabela do MariaDB traz contagens por bucket; o histograma Prometheus exige
// contagens cumulativas. Este teste fixa essa conversão.
func TestQueryResponseTimeCollector(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"TIME", "COUNT", "TOTAL"}).
		AddRow("0.000001", 10, "0.000005").
		AddRow("0.000010", 5, "0.000040").
		AddRow("0.001000", 20, "0.010000").
		AddRow("1.000000", 3, "2.500000").
		AddRow("TOO LONG", 2, "30.000000")

	mock.ExpectQuery("SELECT TIME, COUNT, TOTAL FROM information_schema.QUERY_RESPONSE_TIME").
		WillReturnRows(rows)

	c := NewQueryResponseTimeCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Len(t, metrics, 1, "deve emitir um único histograma")

	snaps := snapshot(t, metrics)
	h := snaps[0]

	require.Equal(t, "mariadb_query_response_time_seconds", h.Name)

	// _count inclui a linha TOO LONG: 10+5+20+3+2 = 40.
	require.Equal(t, uint64(40), h.SampleCount)

	// _sum soma os TOTAL de todas as linhas, inclusive TOO LONG.
	require.InDelta(t, 32.510045, h.SampleSum, 0.000001)

	// TOO LONG não gera bucket: apenas os 4 limites numéricos.
	require.Len(t, h.Buckets, 4)

	// Contagens cumulativas.
	require.Equal(t, uint64(10), h.Buckets[0.000001])
	require.Equal(t, uint64(15), h.Buckets[0.000010])
	require.Equal(t, uint64(35), h.Buckets[0.001000])
	require.Equal(t, uint64(38), h.Buckets[1.000000])

	require.NoError(t, mock.ExpectationsWereMet())
}

// Buckets fora de ordem na tabela devem ser ordenados antes de acumular, senão
// as contagens cumulativas saem erradas.
func TestQueryResponseTimeCollectorSortsBuckets(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"TIME", "COUNT", "TOTAL"}).
		AddRow("1.000000", 3, "2.0").
		AddRow("0.001000", 20, "0.01").
		AddRow("0.000001", 10, "0.000005")

	mock.ExpectQuery("QUERY_RESPONSE_TIME").WillReturnRows(rows)

	c := NewQueryResponseTimeCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	h := snapshot(t, metrics)[0]

	require.Equal(t, uint64(10), h.Buckets[0.000001])
	require.Equal(t, uint64(30), h.Buckets[0.001000])
	require.Equal(t, uint64(33), h.Buckets[1.000000])
	require.Equal(t, uint64(33), h.SampleCount)
}

// Plugin inativo: zero métricas, sem erro (seção 20).
func TestQueryResponseTimeCollectorPluginInactive(t *testing.T) {
	db, mock := newMockDB(t)

	c := NewQueryResponseTimeCollector(true, testLogger(), noFeatures())
	require.False(t, c.Available())

	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet(), "nenhuma query deveria ter sido executada")
}

// Tabela vazia (plugin acabou de ser habilitado): sem histograma e sem erro.
func TestQueryResponseTimeCollectorEmptyTable(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("QUERY_RESPONSE_TIME").
		WillReturnRows(sqlmock.NewRows([]string{"TIME", "COUNT", "TOTAL"}))

	c := NewQueryResponseTimeCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)

	require.NoError(t, err)
	require.Empty(t, metrics)
}

// Somente a linha TOO LONG: o count é contabilizado mesmo sem nenhum bucket.
func TestQueryResponseTimeCollectorOnlyTooLong(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"TIME", "COUNT", "TOTAL"}).
		AddRow("TOO LONG", 7, "100.0")

	mock.ExpectQuery("QUERY_RESPONSE_TIME").WillReturnRows(rows)

	c := NewQueryResponseTimeCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)
	require.Len(t, metrics, 1)

	h := snapshot(t, metrics)[0]
	require.Equal(t, uint64(7), h.SampleCount)
	require.Empty(t, h.Buckets)
}

// Linhas com TIME não numérico são ignoradas sem derrubar o scrape.
func TestQueryResponseTimeCollectorIgnoresInvalidRows(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"TIME", "COUNT", "TOTAL"}).
		AddRow("0.001000", 4, "0.002").
		AddRow("invalido", 9, "1.0")

	mock.ExpectQuery("QUERY_RESPONSE_TIME").WillReturnRows(rows)

	c := NewQueryResponseTimeCollector(true, testLogger(), allFeatures())
	metrics, err := runCollect(t, c, db)
	require.NoError(t, err)

	h := snapshot(t, metrics)[0]
	require.Len(t, h.Buckets, 1)
	require.Equal(t, uint64(4), h.Buckets[0.001000])
	require.Equal(t, uint64(4), h.SampleCount, "a linha inválida não deve entrar no count")
}
