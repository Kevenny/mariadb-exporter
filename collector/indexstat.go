package collector

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// indexStatQueryTmpl orders by ROWS_READ DESC before the limit, keeping the
// most-used indexes when the cutoff is applied.
const indexStatQueryTmpl = `
SELECT TABLE_SCHEMA,
       TABLE_NAME,
       INDEX_NAME,
       ROWS_READ
FROM information_schema.INDEX_STATISTICS
ORDER BY ROWS_READ DESC
LIMIT %d`

// IndexStatCollector collects information_schema.INDEX_STATISTICS.
// Requires `SET GLOBAL userstat = ON`.
type IndexStatCollector struct {
	base
	limit int

	rowsRead *prometheus.Desc
	unused   *prometheus.Desc
}

// NewIndexStatCollector creates the indexstat collector with the limit of
// indexes per scrape.
func NewIndexStatCollector(enabled bool, limit int, logger log.Logger, features FeatureProvider) *IndexStatCollector {
	labels := []string{"schema", "table", "index"}
	return &IndexStatCollector{
		base:  newBase("indexstat", "Per-index statistics from information_schema.INDEX_STATISTICS (requires userstat=ON).", enabled, logger, features),
		limit: limit,

		rowsRead: newDesc("index", "rows_read_total", "Total rows read through the index.", labels),
		unused:   newDesc("index", "unused", "Present with value 1 when the index recorded no reads (rows_read = 0).", labels),
	}
}

// Available implements Availability: depends on the userstat variable.
func (c *IndexStatCollector) Available() bool {
	return c.featureFlags().HasUserStat
}

// Collect implements Collector.
func (c *IndexStatCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "userstat is OFF; no metrics will be collected. Enable with SET GLOBAL userstat = ON")
		return nil
	}

	limit := c.limit
	if limit <= 0 {
		limit = 1000
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(indexStatQueryTmpl, limit))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			schema, table, index sql.NullString
			rowsRead             sql.NullFloat64
		)

		if err := rows.Scan(&schema, &table, &index, &rowsRead); err != nil {
			return err
		}

		s, t, i := schema.String, table.String, index.String

		emitCounter(ch, c.rowsRead, rowsRead, s, t, i)

		// mariadb_index_unused is emitted only for indexes with no reads, with
		// a fixed value of 1 — it works as a marker for dead-index alerts.
		if rowsRead.Valid && rowsRead.Float64 == 0 {
			ch <- prometheus.MustNewConstMetric(
				c.unused, prometheus.GaugeValue, 1,
				sanitizeLabel(s), sanitizeLabel(t), sanitizeLabel(i),
			)
		}
	}

	return rows.Err()
}
