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

// ClientStatCollector collects information_schema.CLIENT_STATISTICS,
// aggregating by source host/IP. Requires `SET GLOBAL userstat = ON`.
type ClientStatCollector struct {
	base

	totalConnections *prometheus.Desc
	rowsRead         *prometheus.Desc
	rowsSent         *prometheus.Desc
}

// NewClientStatCollector creates the clientstat collector.
func NewClientStatCollector(enabled bool, logger log.Logger, features FeatureProvider) *ClientStatCollector {
	labels := []string{"client"}
	return &ClientStatCollector{
		base: newBase("clientstat", "Per-client statistics from information_schema.CLIENT_STATISTICS (requires userstat=ON).", enabled, logger, features),

		totalConnections: newDesc("client", "total_connections_total", "Total connections originating from the client.", labels),
		rowsRead:         newDesc("client", "rows_read_total", "Total rows read by the client.", labels),
		rowsSent:         newDesc("client", "rows_sent_total", "Total rows sent to the client.", labels),
	}
}

// Available implements Availability: depends on the userstat variable.
func (c *ClientStatCollector) Available() bool {
	return c.featureFlags().HasUserStat
}

// Collect implements Collector.
func (c *ClientStatCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "userstat is OFF; no metrics will be collected. Enable with SET GLOBAL userstat = ON")
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
