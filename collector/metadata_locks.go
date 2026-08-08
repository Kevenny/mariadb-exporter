package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// metadataLockQuery lê o plugin metadata_lock_info. As colunas TABLE_SCHEMA e
// TABLE_NAME ficam vazias para locks que não são de tabela (ex: GLOBAL, SCHEMA).
const metadataLockQuery = `
SELECT LOCK_MODE, LOCK_TYPE, TABLE_SCHEMA, TABLE_NAME
FROM information_schema.METADATA_LOCK_INFO`

// MetadataLocksCollector agrega os metadata locks ativos por modo, tipo e
// tabela. Requer o plugin metadata_lock_info e MariaDB >= 10.0.7.
type MetadataLocksCollector struct {
	base

	total   *prometheus.Desc
	waiting *prometheus.Desc
}

// NewMetadataLocksCollector cria o coletor metadata_locks.
func NewMetadataLocksCollector(enabled bool, logger log.Logger, features FeatureProvider) *MetadataLocksCollector {
	labels := []string{"lock_mode", "lock_type", "table_schema", "table_name"}
	return &MetadataLocksCollector{
		base: newBase("metadata_locks",
			"Metadata locks ativos de information_schema.METADATA_LOCK_INFO (requer o plugin metadata_lock_info e MariaDB >= 10.0.7).",
			enabled, logger, features),

		total:   newDesc("metadata_locks", "total", "Total de metadata locks ativos agrupados por modo, tipo e tabela.", labels),
		waiting: newDesc("metadata_lock", "waiting_total", "Total de metadata locks em espera agrupados por modo, tipo e tabela.", labels),
	}
}

// Available implementa Availability: depende do plugin metadata_lock_info e da
// versão mínima, ambos já resolvidos pelo detector de features.
func (c *MetadataLocksCollector) Available() bool {
	return c.featureFlags().HasMetadataLockInfo
}

// Collect implementa Collector.
func (c *MetadataLocksCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "plugin metadata_lock_info inativo (ou MariaDB < 10.0.7); nenhuma métrica será coletada")
		return nil
	}

	rows, err := db.QueryContext(ctx, metadataLockQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Agrupa em memória: a tabela lista um lock por linha, e o que interessa é a
	// contagem por combinação de labels.
	type key struct {
		mode, lockType, schema, table string
	}
	totals := make(map[key]float64)
	waits := make(map[key]float64)

	for rows.Next() {
		var mode, lockType, schema, table sql.NullString

		if err := rows.Scan(&mode, &lockType, &schema, &table); err != nil {
			return err
		}

		k := key{
			mode:     mode.String,
			lockType: lockType.String,
			schema:   schema.String,
			table:    table.String,
		}

		totals[k]++

		// Locks pendentes aparecem com LOCK_MODE contendo "WAIT" (ex:
		// MDL_SHARED_WRITE aguardando vira um estado de espera reportado no modo).
		if strings.Contains(strings.ToUpper(k.mode), "WAIT") {
			waits[k]++
		} else if _, seen := waits[k]; !seen {
			// Garante que a série de espera exista com 0 quando há locks ativos
			// da mesma combinação, evitando gaps no gráfico.
			waits[k] = 0
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}

	for k, v := range totals {
		ch <- prometheus.MustNewConstMetric(
			c.total, prometheus.GaugeValue, v,
			sanitizeLabel(k.mode), sanitizeLabel(k.lockType),
			sanitizeLabel(k.schema), sanitizeLabel(k.table),
		)
	}
	for k, v := range waits {
		ch <- prometheus.MustNewConstMetric(
			c.waiting, prometheus.GaugeValue, v,
			sanitizeLabel(k.mode), sanitizeLabel(k.lockType),
			sanitizeLabel(k.schema), sanitizeLabel(k.table),
		)
	}

	return nil
}
