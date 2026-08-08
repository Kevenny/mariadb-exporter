package collector

import (
	"context"
	"database/sql"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// diskQuery lê o plugin DISKS. Os valores de TOTAL, USED e AVAILABLE vêm em
// kibibytes (1024 bytes) e são convertidos para bytes na exposição.
const diskQuery = `
SELECT Disk, Path, Total, Used, Available
FROM information_schema.DISKS`

// kibibyte é o fator de conversão dos valores do plugin DISKS para bytes.
const kibibyte = 1024

// DisksCollector expõe a ocupação dos filesystems vista pelo servidor.
// Requer o plugin disks.
type DisksCollector struct {
	base

	total     *prometheus.Desc
	used      *prometheus.Desc
	available *prometheus.Desc
}

// NewDisksCollector cria o coletor disks.
func NewDisksCollector(enabled bool, logger log.Logger, features FeatureProvider) *DisksCollector {
	labels := []string{"disk", "path"}
	return &DisksCollector{
		base: newBase("disks", "Uso de disco de information_schema.DISKS (requer o plugin disks).", enabled, logger, features),

		total:     newDesc("disk", "total_bytes", "Capacidade total do filesystem em bytes.", labels),
		used:      newDesc("disk", "used_bytes", "Bytes usados no filesystem.", labels),
		available: newDesc("disk", "available_bytes", "Bytes disponíveis no filesystem.", labels),
	}
}

// Available implementa Availability: depende do plugin disks.
func (c *DisksCollector) Available() bool {
	return c.featureFlags().HasDisksPlugin
}

// Collect implementa Collector.
func (c *DisksCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "plugin disks inativo; nenhuma métrica será coletada")
		return nil
	}

	rows, err := db.QueryContext(ctx, diskQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			disk, path             sql.NullString
			total, used, available sql.NullFloat64
		)

		if err := rows.Scan(&disk, &path, &total, &used, &available); err != nil {
			return err
		}

		d, p := disk.String, path.String

		emitGauge(ch, c.total, scaleKiB(total), d, p)
		emitGauge(ch, c.used, scaleKiB(used), d, p)
		emitGauge(ch, c.available, scaleKiB(available), d, p)
	}

	return rows.Err()
}

// scaleKiB converte um valor em kibibytes para bytes, preservando o estado NULL.
func scaleKiB(v sql.NullFloat64) sql.NullFloat64 {
	if !v.Valid {
		return v
	}
	return sql.NullFloat64{Float64: v.Float64 * kibibyte, Valid: true}
}
