package collector

import (
	"context"
	"database/sql"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
)

// userStatQuery lê information_schema.USER_STATISTICS.
//
// A tabela não tem uma coluna ROWS_CHANGED: o MariaDB a decompõe em
// ROWS_DELETED, ROWS_INSERTED e ROWS_UPDATED (diferente de TABLE_STATISTICS, que
// tem ROWS_CHANGED). A soma das três reproduz a semântica de "linhas alteradas"
// pedida pela especificação.
const userStatQuery = `
SELECT USER,
       TOTAL_CONNECTIONS,
       CONCURRENT_CONNECTIONS,
       ROWS_READ,
       ROWS_SENT,
       ROWS_DELETED + ROWS_INSERTED + ROWS_UPDATED AS ROWS_CHANGED,
       SELECT_COMMANDS,
       UPDATE_COMMANDS,
       OTHER_COMMANDS,
       ACCESS_DENIED,
       LOST_CONNECTIONS
FROM information_schema.USER_STATISTICS`

// UserStatCollector coleta information_schema.USER_STATISTICS.
// Requer `SET GLOBAL userstat = ON`.
type UserStatCollector struct {
	base

	totalConnections      *prometheus.Desc
	concurrentConnections *prometheus.Desc
	rowsRead              *prometheus.Desc
	rowsSent              *prometheus.Desc
	rowsChanged           *prometheus.Desc
	selectCommands        *prometheus.Desc
	updateCommands        *prometheus.Desc
	otherCommands         *prometheus.Desc
	accessDenied          *prometheus.Desc
	lostConnections       *prometheus.Desc
}

// NewUserStatCollector cria o coletor userstat.
func NewUserStatCollector(enabled bool, logger log.Logger, features FeatureProvider) *UserStatCollector {
	labels := []string{"user"}
	return &UserStatCollector{
		base: newBase("userstat", "Estatísticas por usuário de information_schema.USER_STATISTICS (requer userstat=ON).", enabled, logger, features),

		totalConnections:      newDesc("user", "total_connections_total", "Total de conexões feitas pelo usuário.", labels),
		concurrentConnections: newDesc("user", "concurrent_connections", "Conexões simultâneas atuais do usuário.", labels),
		rowsRead:              newDesc("user", "rows_read_total", "Total de linhas lidas pelo usuário.", labels),
		rowsSent:              newDesc("user", "rows_sent_total", "Total de linhas enviadas ao usuário.", labels),
		rowsChanged:           newDesc("user", "rows_changed_total", "Total de linhas alteradas pelo usuário.", labels),
		selectCommands:        newDesc("user", "select_commands_total", "Total de comandos SELECT executados pelo usuário.", labels),
		updateCommands:        newDesc("user", "update_commands_total", "Total de comandos UPDATE executados pelo usuário.", labels),
		otherCommands:         newDesc("user", "other_commands_total", "Total de outros comandos executados pelo usuário.", labels),
		accessDenied:          newDesc("user", "access_denied_total", "Total de acessos negados ao usuário.", labels),
		lostConnections:       newDesc("user", "lost_connections_total", "Total de conexões perdidas do usuário.", labels),
	}
}

// Available implementa Availability: depende da variável userstat.
func (c *UserStatCollector) Available() bool {
	return c.featureFlags().HasUserStat
}

// Collect implementa Collector.
func (c *UserStatCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "userstat está OFF; nenhuma métrica será coletada. Habilite com SET GLOBAL userstat = ON")
		return nil
	}

	rows, err := db.QueryContext(ctx, userStatQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			user                              sql.NullString
			totalConn, concurrentConn         sql.NullFloat64
			rowsRead, rowsSent, rowsChanged   sql.NullFloat64
			selectCmds, updateCmds, otherCmds sql.NullFloat64
			accessDenied, lostConn            sql.NullFloat64
		)

		if err := rows.Scan(
			&user, &totalConn, &concurrentConn,
			&rowsRead, &rowsSent, &rowsChanged,
			&selectCmds, &updateCmds, &otherCmds,
			&accessDenied, &lostConn,
		); err != nil {
			return err
		}

		u := user.String

		emitCounter(ch, c.totalConnections, totalConn, u)
		emitGauge(ch, c.concurrentConnections, concurrentConn, u)
		emitCounter(ch, c.rowsRead, rowsRead, u)
		emitCounter(ch, c.rowsSent, rowsSent, u)
		emitCounter(ch, c.rowsChanged, rowsChanged, u)
		emitCounter(ch, c.selectCommands, selectCmds, u)
		emitCounter(ch, c.updateCommands, updateCmds, u)
		emitCounter(ch, c.otherCommands, otherCmds, u)
		emitCounter(ch, c.accessDenied, accessDenied, u)
		emitCounter(ch, c.lostConnections, lostConn, u)
	}

	return rows.Err()
}

// emitCounter envia um counter apenas se o valor não for NULL. Valores NULL são
// omitidos em vez de virarem zero, para não inventar dado que o servidor não deu.
func emitCounter(ch chan<- prometheus.Metric, desc *prometheus.Desc, v sql.NullFloat64, labels ...string) {
	if !v.Valid {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, v.Float64, labels...)
}

// emitGauge envia um gauge apenas se o valor não for NULL.
func emitGauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, v sql.NullFloat64, labels ...string) {
	if !v.Valid {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v.Float64, labels...)
}
