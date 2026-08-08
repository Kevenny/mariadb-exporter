package collector

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// tableStatQueryTmpl ordena por ROWS_READ DESC antes de aplicar o limite, para
// que o corte preserve as tabelas mais movimentadas (seção 2.2, tablestat).
const tableStatQueryTmpl = `
SELECT TABLE_SCHEMA,
       TABLE_NAME,
       ROWS_READ,
       ROWS_CHANGED,
       ROWS_CHANGED_X_INDEXES
FROM information_schema.TABLE_STATISTICS
ORDER BY ROWS_READ DESC
LIMIT %d`

// TableStatCollector coleta information_schema.TABLE_STATISTICS.
// Requer `SET GLOBAL userstat = ON`.
type TableStatCollector struct {
	base
	limit int

	rowsRead            *prometheus.Desc
	rowsChanged         *prometheus.Desc
	rowsChangedXIndexes *prometheus.Desc
}

// NewTableStatCollector cria o coletor tablestat com o limite de tabelas por
// scrape.
func NewTableStatCollector(enabled bool, limit int, logger log.Logger, features FeatureProvider) *TableStatCollector {
	labels := []string{"schema", "table"}
	return &TableStatCollector{
		base:  newBase("tablestat", "Estatísticas por tabela de information_schema.TABLE_STATISTICS (requer userstat=ON).", enabled, logger, features),
		limit: limit,

		rowsRead:            newDesc("table", "rows_read_total", "Total de linhas lidas na tabela.", labels),
		rowsChanged:         newDesc("table", "rows_changed_total", "Total de linhas alteradas na tabela.", labels),
		rowsChangedXIndexes: newDesc("table", "rows_changed_x_indexes_total", "Total de linhas alteradas multiplicado pelo número de índices afetados.", labels),
	}
}

// Available implementa Availability: depende da variável userstat.
func (c *TableStatCollector) Available() bool {
	return c.featureFlags().HasUserStat
}

// Collect implementa Collector.
func (c *TableStatCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "userstat está OFF; nenhuma métrica será coletada. Habilite com SET GLOBAL userstat = ON")
		return nil
	}

	limit := c.limit
	if limit <= 0 {
		limit = 500
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(tableStatQueryTmpl, limit))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			schema, table                          sql.NullString
			rowsRead, rowsChanged, rowsChangedXIdx sql.NullFloat64
		)

		if err := rows.Scan(&schema, &table, &rowsRead, &rowsChanged, &rowsChangedXIdx); err != nil {
			return err
		}

		s, t := schema.String, table.String

		emitCounter(ch, c.rowsRead, rowsRead, s, t)
		emitCounter(ch, c.rowsChanged, rowsChanged, s, t)
		emitCounter(ch, c.rowsChangedXIndexes, rowsChangedXIdx, s, t)
	}

	return rows.Err()
}
