package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// InnoDBCollector expõe métricas do engine InnoDB.
//
// A especificação cita SHOW ENGINE INNODB STATUS como fonte, mas os valores
// pedidos (buffer pool, row locks, deadlocks) estão todos disponíveis como
// contadores em SHOW GLOBAL STATUS, que é um formato estruturado e estável entre
// versões — parsear o texto livre do INNODB STATUS seria frágil. Os deadlocks
// são a exceção: só existem no texto, então há um fallback para ele.
type InnoDBCollector struct {
	base

	bufferPoolReadRequests *prometheus.Desc
	bufferPoolReads        *prometheus.Desc
	bufferPoolPages        *prometheus.Desc
	rowLockWaits           *prometheus.Desc
	rowLockTimeAvg         *prometheus.Desc
	deadlocks              *prometheus.Desc
}

// NewInnoDBCollector cria o coletor innodb.
func NewInnoDBCollector(enabled bool, logger log.Logger, features FeatureProvider) *InnoDBCollector {
	return &InnoDBCollector{
		base: newBase("innodb", "Métricas do engine InnoDB (buffer pool, row locks, deadlocks).", enabled, logger, features),

		bufferPoolReadRequests: newDesc("innodb", "buffer_pool_read_requests_total", "Total de leituras lógicas requisitadas ao buffer pool.", nil),
		bufferPoolReads:        newDesc("innodb", "buffer_pool_reads_total", "Total de leituras que o buffer pool não conseguiu satisfazer e foram ao disco.", nil),
		bufferPoolPages:        newDesc("innodb", "buffer_pool_pages_total", "Páginas do buffer pool por tipo.", []string{"type"}),
		rowLockWaits:           newDesc("innodb", "row_lock_waits_total", "Total de vezes que uma operação esperou por um row lock.", nil),
		rowLockTimeAvg:         newDesc("innodb", "row_lock_time_avg_milliseconds", "Tempo médio de espera por row lock em milissegundos.", nil),
		deadlocks:              newDesc("innodb", "deadlocks_total", "Total de deadlocks detectados pelo InnoDB.", nil),
	}
}

// innodbStatusMappings liga variáveis de SHOW GLOBAL STATUS às métricas simples
// (sem label) deste coletor.
var innodbStatusMappings = map[string]struct {
	field string
	kind  prometheus.ValueType
}{
	"innodb_buffer_pool_read_requests": {"bufferPoolReadRequests", prometheus.CounterValue},
	"innodb_buffer_pool_reads":         {"bufferPoolReads", prometheus.CounterValue},
	"innodb_row_lock_waits":            {"rowLockWaits", prometheus.CounterValue},
	"innodb_row_lock_time_avg":         {"rowLockTimeAvg", prometheus.GaugeValue},
	"innodb_deadlocks":                 {"deadlocks", prometheus.CounterValue},
}

// innodbPageTypes mapeia as variáveis de página do buffer pool para o valor do
// label `type` (seção 2.2: free, data, dirty).
var innodbPageTypes = map[string]string{
	"innodb_buffer_pool_pages_free":  "free",
	"innodb_buffer_pool_pages_data":  "data",
	"innodb_buffer_pool_pages_dirty": "dirty",
	"innodb_buffer_pool_pages_misc":  "misc",
	"innodb_buffer_pool_pages_total": "total",
}

// Collect implementa Collector.
func (c *InnoDBCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL STATUS LIKE 'Innodb_%'")
	if err != nil {
		return err
	}
	defer rows.Close()

	descByField := map[string]*prometheus.Desc{
		"bufferPoolReadRequests": c.bufferPoolReadRequests,
		"bufferPoolReads":        c.bufferPoolReads,
		"rowLockWaits":           c.rowLockWaits,
		"rowLockTimeAvg":         c.rowLockTimeAvg,
		"deadlocks":              c.deadlocks,
	}

	sawDeadlocks := false

	for rows.Next() {
		var name string
		var raw sql.RawBytes

		if err := rows.Scan(&name, &raw); err != nil {
			return err
		}

		key := strings.ToLower(name)

		if mapping, ok := innodbStatusMappings[key]; ok {
			v, err := parseFloat(string(raw))
			if err != nil {
				_ = level.Debug(c.Logger()).Log("msg", "variável InnoDB ignorada", "variavel", name, "err", err)
				continue
			}
			ch <- prometheus.MustNewConstMetric(descByField[mapping.field], mapping.kind, v)
			if key == "innodb_deadlocks" {
				sawDeadlocks = true
			}
			continue
		}

		if pageType, ok := innodbPageTypes[key]; ok {
			v, err := parseFloat(string(raw))
			if err != nil {
				_ = level.Debug(c.Logger()).Log("msg", "variável de página InnoDB ignorada", "variavel", name, "err", err)
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.bufferPoolPages, prometheus.GaugeValue, v, pageType)
		}
	}

	if err := rows.Err(); err != nil {
		return err
	}

	// Innodb_deadlocks não existe em todas as builds (é um status do XtraDB/
	// Percona presente no MariaDB, mas ausente em algumas versões). Quando falta,
	// recorre ao contador do texto de SHOW ENGINE INNODB STATUS.
	if !sawDeadlocks {
		if count, ok := c.deadlockCountFromEngineStatus(ctx, db); ok {
			ch <- prometheus.MustNewConstMetric(c.deadlocks, prometheus.CounterValue, count)
		}
	}

	return nil
}

// deadlockCountFromEngineStatus extrai o número de deadlocks do texto de
// SHOW ENGINE INNODB STATUS.
//
// O bloco LATEST DETECTED DEADLOCK não traz um contador acumulado; o que existe
// é a contagem de deadlocks na seção TRANSACTIONS de alguns builds. Quando nada
// utilizável é encontrado, devolve ok=false e nenhuma métrica é emitida.
func (c *InnoDBCollector) deadlockCountFromEngineStatus(ctx context.Context, db *sql.DB) (float64, bool) {
	var engineType, name, status sql.NullString

	row := db.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS")
	if err := row.Scan(&engineType, &name, &status); err != nil {
		_ = level.Debug(c.Logger()).Log("msg", "SHOW ENGINE INNODB STATUS indisponível", "err", err)
		return 0, false
	}

	for _, line := range strings.Split(status.String, "\n") {
		trimmed := strings.TrimSpace(line)
		// Formato observado: "Number of deadlocks 12"
		if !strings.HasPrefix(strings.ToLower(trimmed), "number of deadlocks") {
			continue
		}
		fields := strings.Fields(trimmed)
		v, err := parseFloat(fields[len(fields)-1])
		if err != nil {
			return 0, false
		}
		return v, true
	}

	return 0, false
}
