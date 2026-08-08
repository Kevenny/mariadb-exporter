package collector

import (
	"context"
	"database/sql"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// statusMapping liga uma variável de SHOW GLOBAL STATUS a uma métrica exposta.
type statusMapping struct {
	variable string // nome da variável no MariaDB
	metric   string // nome da métrica, sem o namespace
	help     string
	kind     prometheus.ValueType
}

// globalStatusMappings é a lista explícita da seção 2.2. Deliberadamente não é
// wildcard: expor todo o SHOW GLOBAL STATUS geraria cardinalidade alta.
var globalStatusMappings = []statusMapping{
	{"Connections", "connections_total", "Total de tentativas de conexão ao servidor.", prometheus.CounterValue},
	{"Max_used_connections", "max_used_connections", "Número máximo de conexões simultâneas já registrado.", prometheus.GaugeValue},
	{"Aborted_connects", "aborted_connects_total", "Total de tentativas de conexão ao servidor que falharam.", prometheus.CounterValue},
	{"Aborted_clients", "aborted_clients_total", "Total de conexões abortadas por clientes que encerraram sem fechar a conexão.", prometheus.CounterValue},
	{"Bytes_received", "bytes_received_total", "Total de bytes recebidos de todos os clientes.", prometheus.CounterValue},
	{"Bytes_sent", "bytes_sent_total", "Total de bytes enviados a todos os clientes.", prometheus.CounterValue},
	{"Questions", "questions_total", "Total de statements executados pelo servidor.", prometheus.CounterValue},
	{"Slow_queries", "slow_queries_total", "Total de queries que excederam long_query_time.", prometheus.CounterValue},
	{"Open_files", "open_files", "Número de arquivos abertos no momento.", prometheus.GaugeValue},
	{"Open_tables", "open_tables", "Número de tabelas abertas no momento.", prometheus.GaugeValue},
	{"Table_open_cache_hits", "table_open_cache_hits_total", "Total de acertos no cache de tabelas abertas.", prometheus.CounterValue},
	{"Table_open_cache_misses", "table_open_cache_misses_total", "Total de falhas no cache de tabelas abertas.", prometheus.CounterValue},
	{"Created_tmp_tables", "created_tmp_tables_total", "Total de tabelas temporárias criadas em memória.", prometheus.CounterValue},
	{"Created_tmp_disk_tables", "created_tmp_disk_tables_total", "Total de tabelas temporárias criadas em disco.", prometheus.CounterValue},
	{"Select_full_join", "select_full_join_total", "Total de joins sem índice que exigiram varredura completa.", prometheus.CounterValue},
	{"Select_scan", "select_scan_total", "Total de SELECTs que fizeram full table scan na primeira tabela.", prometheus.CounterValue},
	{"Sort_merge_passes", "sort_merge_passes_total", "Total de passes de merge sort executados.", prometheus.CounterValue},
	{"Uptime", "uptime_seconds", "Tempo em segundos desde o start do servidor.", prometheus.GaugeValue},
}

// GlobalStatusCollector coleta um subconjunto explícito de SHOW GLOBAL STATUS.
type GlobalStatusCollector struct {
	base
	descs map[string]*prometheus.Desc
	kinds map[string]prometheus.ValueType
}

// NewGlobalStatusCollector cria o coletor global_status.
func NewGlobalStatusCollector(enabled bool, logger log.Logger, features FeatureProvider) *GlobalStatusCollector {
	c := &GlobalStatusCollector{
		base:  newBase("global_status", "Subconjunto selecionado de SHOW GLOBAL STATUS.", enabled, logger, features),
		descs: make(map[string]*prometheus.Desc, len(globalStatusMappings)),
		kinds: make(map[string]prometheus.ValueType, len(globalStatusMappings)),
	}

	for _, m := range globalStatusMappings {
		// A chave é minúscula porque o MariaDB não é consistente na
		// capitalização das variáveis entre versões.
		key := strings.ToLower(m.variable)
		c.descs[key] = newDesc("", m.metric, m.help, nil)
		c.kinds[key] = m.kind
	}

	return c
}

// Collect implementa Collector.
func (c *GlobalStatusCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	rows, err := db.QueryContext(ctx, "SHOW GLOBAL STATUS")
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

		v, err := parseFloat(string(value))
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "variável de status ignorada", "variavel", name, "err", err)
			continue
		}

		ch <- prometheus.MustNewConstMetric(desc, c.kinds[key], v)
	}

	return rows.Err()
}
