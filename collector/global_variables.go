package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// variableMapping liga uma variável de SHOW GLOBAL VARIABLES a uma métrica.
type variableMapping struct {
	variable string
	metric   string
	help     string
}

// globalVariableMappings é a lista explícita da seção 2.2. Todas as variáveis
// aqui são de configuração, portanto expostas como gauge.
var globalVariableMappings = []variableMapping{
	{"max_connections", "max_connections", "Valor de max_connections."},
	{"innodb_buffer_pool_size", "innodb_buffer_pool_size_bytes", "Tamanho do InnoDB buffer pool em bytes."},
	{"query_cache_size", "query_cache_size_bytes", "Tamanho do query cache em bytes."},
	{"thread_cache_size", "thread_cache_size", "Valor de thread_cache_size."},
	{"wait_timeout", "wait_timeout_seconds", "Valor de wait_timeout em segundos."},
	{"interactive_timeout", "interactive_timeout_seconds", "Valor de interactive_timeout em segundos."},
	{"userstat", "userstat_enabled", "1 se a variável userstat está ON, 0 caso contrário."},
}

// GlobalVariablesCollector coleta um subconjunto explícito de
// SHOW GLOBAL VARIABLES.
type GlobalVariablesCollector struct {
	base
	descs map[string]*prometheus.Desc
}

// NewGlobalVariablesCollector cria o coletor global_variables.
func NewGlobalVariablesCollector(enabled bool, logger log.Logger, features FeatureProvider) *GlobalVariablesCollector {
	c := &GlobalVariablesCollector{
		base:  newBase("global_variables", "Subconjunto selecionado de SHOW GLOBAL VARIABLES.", enabled, logger, features),
		descs: make(map[string]*prometheus.Desc, len(globalVariableMappings)),
	}

	for _, m := range globalVariableMappings {
		c.descs[strings.ToLower(m.variable)] = newDesc("", m.metric, m.help, nil)
	}

	return c
}

// Collect implementa Collector.
func (c *GlobalVariablesCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL VARIABLES")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var value sql.RawBytes

		if err := rows.Scan(&name, &value); err != nil {
			return err
		}

		key := strings.ToLower(name)
		desc, ok := c.descs[key]
		if !ok {
			continue
		}

		// parseFloat converte ON/OFF em 1/0, o que cobre userstat_enabled.
		v, err := parseFloat(string(value))
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "variável global ignorada", "variavel", name, "err", err)
			continue
		}

		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v)
	}

	return rows.Err()
}
