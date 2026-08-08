package collector

import (
	"context"
	"database/sql"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

const clientStatQuery = `
SELECT CLIENT,
       TOTAL_CONNECTIONS,
       ROWS_READ,
       ROWS_SENT
FROM information_schema.CLIENT_STATISTICS`

// ClientStatCollector coleta information_schema.CLIENT_STATISTICS, agregando por
// host/IP de origem. Requer `SET GLOBAL userstat = ON`.
type ClientStatCollector struct {
	base

	totalConnections *prometheus.Desc
	rowsRead         *prometheus.Desc
	rowsSent         *prometheus.Desc
}

// NewClientStatCollector cria o coletor clientstat.
func NewClientStatCollector(enabled bool, logger log.Logger, features FeatureProvider) *ClientStatCollector {
	labels := []string{"client"}
	return &ClientStatCollector{
		base: newBase("clientstat", "Estatísticas por cliente de information_schema.CLIENT_STATISTICS (requer userstat=ON).", enabled, logger, features),

		totalConnections: newDesc("client", "total_connections_total", "Total de conexões originadas do cliente.", labels),
		rowsRead:         newDesc("client", "rows_read_total", "Total de linhas lidas pelo cliente.", labels),
		rowsSent:         newDesc("client", "rows_sent_total", "Total de linhas enviadas ao cliente.", labels),
	}
}

// Available implementa Availability: depende da variável userstat.
func (c *ClientStatCollector) Available() bool {
	return c.featureFlags().HasUserStat
}

// Collect implementa Collector.
func (c *ClientStatCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "userstat está OFF; nenhuma métrica será coletada. Habilite com SET GLOBAL userstat = ON")
		return nil
	}

	rows, err := db.QueryContext(ctx, clientStatQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			client                        sql.NullString
			totalConn, rowsRead, rowsSent sql.NullFloat64
		)

		if err := rows.Scan(&client, &totalConn, &rowsRead, &rowsSent); err != nil {
			return err
		}

		cl := client.String

		emitCounter(ch, c.totalConnections, totalConn, cl)
		emitCounter(ch, c.rowsRead, rowsRead, cl)
		emitCounter(ch, c.rowsSent, rowsSent, cl)
	}

	return rows.Err()
}
