package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// ReplicationCollector coleta o estado das réplicas.
//
// O MariaDB suporta replicação multi-source nativa, então a fonte primária é
// SHOW ALL SLAVES STATUS, que devolve uma linha por conexão de replicação. Em
// servidores muito antigos (ou builds sem o comando) há fallback para
// SHOW SLAVE STATUS, que devolve no máximo uma linha (seção 2.2, replication).
type ReplicationCollector struct {
	base

	sqlRunning          *prometheus.Desc
	ioRunning           *prometheus.Desc
	secondsBehindMaster *prometheus.Desc
	lastErrno           *prometheus.Desc
	relayLogPos         *prometheus.Desc
}

// NewReplicationCollector cria o coletor replication.
func NewReplicationCollector(enabled bool, logger log.Logger, features FeatureProvider) *ReplicationCollector {
	labels := []string{"connection_name", "master_host", "master_port"}
	return &ReplicationCollector{
		base: newBase("replication", "Estado da replicação via SHOW ALL SLAVES STATUS (suporta multi-source).", enabled, logger, features),

		sqlRunning:          newDesc("slave", "sql_running", "1 se a thread SQL da réplica está rodando, 0 caso contrário.", labels),
		ioRunning:           newDesc("slave", "io_running", "1 se a thread de I/O da réplica está rodando, 0 caso contrário.", labels),
		secondsBehindMaster: newDesc("slave", "seconds_behind_master", "Atraso da réplica em relação ao master em segundos.", labels),
		lastErrno:           newDesc("slave", "last_errno", "Código do último erro da réplica (0 = sem erro).", labels),
		relayLogPos:         newDesc("slave", "relay_log_pos", "Posição atual no relay log.", labels),
	}
}

// Collect implementa Collector.
func (c *ReplicationCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	rows, err := db.QueryContext(ctx, "SHOW ALL SLAVES STATUS")
	if err != nil {
		_ = level.Debug(c.Logger()).Log("msg", "SHOW ALL SLAVES STATUS indisponível, usando SHOW SLAVE STATUS", "err", err)

		rows, err = db.QueryContext(ctx, "SHOW SLAVE STATUS")
		if err != nil {
			return err
		}
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	// O conjunto de colunas varia bastante entre versões do MariaDB, por isso a
	// leitura é feita em um mapa indexado pelo nome da coluna em minúsculas em
	// vez de posições fixas.
	for rows.Next() {
		values := make([]sql.RawBytes, len(columns))
		scanArgs := make([]interface{}, len(columns))
		for i := range values {
			scanArgs[i] = &values[i]
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return err
		}

		row := make(map[string]string, len(columns))
		for i, col := range columns {
			row[strings.ToLower(col)] = string(values[i])
		}

		// Sem Connection_name (single-source ou fallback), o label fica vazio,
		// que é o identificador da conexão default no MariaDB.
		connectionName := row["connection_name"]
		masterHost := row["master_host"]
		masterPort := row["master_port"]

		labels := []string{connectionName, masterHost, masterPort}

		emitBool(ch, c.sqlRunning, row["slave_sql_running"], labels...)
		emitBool(ch, c.ioRunning, row["slave_io_running"], labels...)

		// Seconds_Behind_Master é NULL quando a replicação está parada; nesse
		// caso a métrica é omitida em vez de reportar 0, que seria lido como
		// "réplica em dia".
		emitNumeric(ch, c.secondsBehindMaster, row["seconds_behind_master"], prometheus.GaugeValue, labels...)
		emitNumeric(ch, c.lastErrno, row["last_errno"], prometheus.GaugeValue, labels...)
		emitNumeric(ch, c.relayLogPos, row["relay_log_pos"], prometheus.GaugeValue, labels...)
	}

	// Zero linhas significa que a instância não é réplica: nenhuma métrica e
	// nenhum erro (seção 20, replicação com 0, 1 ou N slaves).
	return rows.Err()
}

// emitBool converte Yes/No/ON/OFF em 1/0.
func emitBool(ch chan<- prometheus.Metric, desc *prometheus.Desc, raw string, labels ...string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}

	var value float64
	switch strings.ToUpper(raw) {
	case "YES", "ON", "1", "TRUE":
		value = 1
	case "NO", "OFF", "0", "FALSE":
		value = 0
	case "CONNECTING":
		// Connecting não é "rodando": a thread existe mas ainda não replica.
		value = 0
	default:
		return
	}

	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}

// emitNumeric envia o valor se ele for numérico; strings vazias e NULL são
// silenciosamente ignoradas.
func emitNumeric(ch chan<- prometheus.Metric, desc *prometheus.Desc, raw string, kind prometheus.ValueType, labels ...string) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "NULL") {
		return
	}

	v, err := parseFloat(raw)
	if err != nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(desc, kind, v, labels...)
}
