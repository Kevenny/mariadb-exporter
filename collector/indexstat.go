package collector

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// indexStatQueryTmpl ordena por ROWS_READ DESC antes do limite, mantendo os
// índices mais usados quando o corte é aplicado.
const indexStatQueryTmpl = `
SELECT TABLE_SCHEMA,
       TABLE_NAME,
       INDEX_NAME,
       ROWS_READ
FROM information_schema.INDEX_STATISTICS
ORDER BY ROWS_READ DESC
LIMIT %d`

// IndexStatCollector coleta information_schema.INDEX_STATISTICS.
// Requer `SET GLOBAL userstat = ON`.
type IndexStatCollector struct {
	base
	limit int

	rowsRead *prometheus.Desc
	unused   *prometheus.Desc
}

// NewIndexStatCollector cria o coletor indexstat com o limite de índices por
// scrape.
func NewIndexStatCollector(enabled bool, limit int, logger log.Logger, features FeatureProvider) *IndexStatCollector {
	labels := []string{"schema", "table", "index"}
	return &IndexStatCollector{
		base:  newBase("indexstat", "Estatísticas por índice de information_schema.INDEX_STATISTICS (requer userstat=ON).", enabled, logger, features),
		limit: limit,

		rowsRead: newDesc("index", "rows_read_total", "Total de linhas lidas através do índice.", labels),
		unused:   newDesc("index", "unused", "Presente com valor 1 quando o índice não registrou leituras (rows_read = 0).", labels),
	}
}

// Available implementa Availability: depende da variável userstat.
func (c *IndexStatCollector) Available() bool {
	return c.featureFlags().HasUserStat
}

// Collect implementa Collector.
func (c *IndexStatCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "userstat está OFF; nenhuma métrica será coletada. Habilite com SET GLOBAL userstat = ON")
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

		// mariadb_index_unused é emitida apenas para índices sem leitura, com
		// valor fixo 1 — funciona como um marcador para alertas de índice morto.
		if rowsRead.Valid && rowsRead.Float64 == 0 {
			ch <- prometheus.MustNewConstMetric(
				c.unused, prometheus.GaugeValue, 1,
				sanitizeLabel(s), sanitizeLabel(t), sanitizeLabel(i),
			)
		}
	}

	return rows.Err()
}
