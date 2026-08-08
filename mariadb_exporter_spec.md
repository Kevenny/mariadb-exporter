# mariadb_exporter — Especificação Técnica para Desenvolvimento

> Documento de arquitetura e implementação destinado ao **Claude Code**.
> Todas as decisões de design, estrutura de diretórios, interfaces, métricas e
> comportamentos esperados estão documentados aqui. Leia este arquivo inteiro
> antes de escrever qualquer linha de código.

---

## 1. Contexto e motivação

O PMM (Percona Monitoring & Management) monitora MariaDB via `mysqld_exporter`,
um exporter genérico projetado para MySQL. Isso causa lacunas críticas:

- Sem dashboards dedicados às métricas exclusivas do MariaDB
- QAN não captura exemplos de queries dentro de transações (MariaDB não implementa
  `events_transactions_*` consumers do Performance Schema)
- Plugins exclusivos do MariaDB (`USER_STATISTICS`, `TABLE_STATISTICS`,
  `INDEX_STATISTICS`, `METADATA_LOCK_INFO`, `DISKS`, `query_response_time`)
  não têm coleta estruturada
- Sem detecção automática de versão MariaDB vs MySQL — o `mysqld_exporter`
  trata ambos igualmente e falha silenciosamente em recursos exclusivos

O `mariadb_exporter` resolve essas lacunas com um exporter dedicado,
compatível com Prometheus e integrável ao PMM como External Service.

---

## 2. Requisitos funcionais

### 2.1 Detecção automática de versão

- Ao iniciar, conectar ao banco e executar `SELECT VERSION()`
- Verificar se a string contém `MariaDB`
- Se não for MariaDB, logar aviso e encerrar com erro descritivo
- Extrair major.minor.patch para habilitar/desabilitar coletores por versão
  (ex: `METADATA_LOCK_INFO` disponível a partir da 10.0.7)

### 2.2 Coletores implementados

Cada coletor deve ser independente, com flag `--collector.<nome>` para
habilitar/desabilitar individualmente. Todos habilitados por padrão, exceto
os marcados como `opt-in`.

#### Coletor: `userstat`
Fonte: `information_schema.USER_STATISTICS`
Pré-requisito: `SET GLOBAL userstat = ON`
Métricas expostas:
- `mariadb_user_total_connections_total` (counter) — total de conexões
- `mariadb_user_concurrent_connections` (gauge) — conexões simultâneas atuais
- `mariadb_user_rows_read_total` (counter) — linhas lidas
- `mariadb_user_rows_sent_total` (counter) — linhas enviadas
- `mariadb_user_rows_changed_total` (counter) — linhas alteradas
- `mariadb_user_select_commands_total` (counter) — SELECTs executados
- `mariadb_user_update_commands_total` (counter) — UPDATEs executados
- `mariadb_user_other_commands_total` (counter) — outros comandos
- `mariadb_user_access_denied_total` (counter) — acessos negados
- `mariadb_user_lost_connections_total` (counter) — conexões perdidas
Labels: `user`

#### Coletor: `tablestat`
Fonte: `information_schema.TABLE_STATISTICS`
Pré-requisito: `SET GLOBAL userstat = ON`
Métricas expostas:
- `mariadb_table_rows_read_total` (counter) — linhas lidas por tabela
- `mariadb_table_rows_changed_total` (counter) — linhas alteradas por tabela
- `mariadb_table_rows_changed_x_indexes_total` (counter) — linhas × índices afetados
Labels: `schema`, `table`
Limite configurável: `--collector.tablestat.limit=500` (padrão: 500 tabelas por scrape)
Ordenar por `rows_read DESC` antes de aplicar o limite.

#### Coletor: `indexstat`
Fonte: `information_schema.INDEX_STATISTICS`
Pré-requisito: `SET GLOBAL userstat = ON`
Métricas expostas:
- `mariadb_index_rows_read_total` (counter) — leituras via índice
- `mariadb_index_unused` (gauge, valor sempre 1) — índice sem uso (rows_read = 0)
Labels: `schema`, `table`, `index`
Filtro: expor apenas índices com `rows_read = 0` para a métrica `mariadb_index_unused`
Limite configurável: `--collector.indexstat.limit=1000`

#### Coletor: `clientstat`
Fonte: `information_schema.CLIENT_STATISTICS`
Pré-requisito: `SET GLOBAL userstat = ON`
Métricas expostas:
- `mariadb_client_total_connections_total` (counter)
- `mariadb_client_rows_read_total` (counter)
- `mariadb_client_rows_sent_total` (counter)
Labels: `client` (host/IP de origem)

#### Coletor: `query_response_time`
Fonte: `information_schema.QUERY_RESPONSE_TIME`
Pré-requisito: plugin `query_response_time` instalado e `query_response_time_stats=ON`
Métricas expostas (como histogram nativo Prometheus):
- `mariadb_query_response_time_seconds_bucket` — buckets do histograma
- `mariadb_query_response_time_seconds_count` — total de queries
- `mariadb_query_response_time_seconds_sum` — soma dos tempos em segundos
Implementar como histograma Prometheus real (`prometheus.NewHistogram` com
`BucketBounds` extraídos dinamicamente da tabela).
Ignorar a linha `TOO LONG` na construção dos buckets mas contabilizar
seu `count` no total.

#### Coletor: `metadata_locks`
Fonte: `information_schema.METADATA_LOCK_INFO`
Pré-requisito: plugin `metadata_lock_info` instalado
Versão mínima: MariaDB 10.0.7
Métricas expostas:
- `mariadb_metadata_locks_total` (gauge) — total de MDL ativos
- `mariadb_metadata_lock_waiting_total` (gauge) — locks em estado WAIT
Labels: `lock_mode`, `lock_type`, `table_schema`, `table_name`

#### Coletor: `disks`
Fonte: `information_schema.DISKS`
Pré-requisito: plugin `disks` instalado
Métricas expostas:
- `mariadb_disk_total_bytes` (gauge) — capacidade total do filesystem
- `mariadb_disk_used_bytes` (gauge) — bytes usados
- `mariadb_disk_available_bytes` (gauge) — bytes disponíveis
Labels: `disk`, `path`

#### Coletor: `replication`
Fonte: `SHOW ALL SLAVES STATUS` (MariaDB) com fallback para `SHOW SLAVE STATUS`
Métricas expostas:
- `mariadb_slave_sql_running` (gauge, 0/1)
- `mariadb_slave_io_running` (gauge, 0/1)
- `mariadb_slave_seconds_behind_master` (gauge)
- `mariadb_slave_last_errno` (gauge)
- `mariadb_slave_relay_log_pos` (gauge)
Labels: `connection_name`, `master_host`, `master_port`
Nota: MariaDB suporta replicação multi-source nativa — o coletor deve iterar
sobre todos os slaves retornados por `SHOW ALL SLAVES STATUS`.

#### Coletor: `galera` (opt-in)
Fonte: `SHOW STATUS LIKE 'wsrep_%'`
Habilitado via: `--collector.galera`
Métricas expostas:
- `mariadb_galera_cluster_size` (gauge)
- `mariadb_galera_cluster_status` (gauge, 0=non-primary, 1=primary)
- `mariadb_galera_local_state` (gauge, 0=joining, 1=donor, 2=joined, 3=synced)
- `mariadb_galera_flow_control_paused` (gauge)
- `mariadb_galera_recv_queue_avg` (gauge)
- `mariadb_galera_send_queue_avg` (gauge)
Labels: `cluster_name`

#### Coletor: `innodb`
Fonte: `SHOW ENGINE INNODB STATUS` + `information_schema.INNODB_*`
Métricas expostas:
- `mariadb_innodb_buffer_pool_read_requests_total` (counter)
- `mariadb_innodb_buffer_pool_reads_total` (counter)
- `mariadb_innodb_buffer_pool_pages_total` (gauge) — por tipo (free, data, dirty)
- `mariadb_innodb_row_lock_waits_total` (counter)
- `mariadb_innodb_row_lock_time_avg_milliseconds` (gauge)
- `mariadb_innodb_deadlocks_total` (counter)
Labels: `type` onde aplicável

#### Coletor: `global_status`
Fonte: `SHOW GLOBAL STATUS`
Subconjunto selecionado de variáveis — não expor todas (alta cardinalidade).
Métricas expostas (lista explícita, não wildcard):
- `mariadb_connections_total` — Connections
- `mariadb_max_used_connections` — Max_used_connections
- `mariadb_aborted_connects_total` — Aborted_connects
- `mariadb_aborted_clients_total` — Aborted_clients
- `mariadb_bytes_received_total` — Bytes_received
- `mariadb_bytes_sent_total` — Bytes_sent
- `mariadb_questions_total` — Questions
- `mariadb_slow_queries_total` — Slow_queries
- `mariadb_open_files` — Open_files
- `mariadb_open_tables` — Open_tables
- `mariadb_table_open_cache_hits_total` — Table_open_cache_hits
- `mariadb_table_open_cache_misses_total` — Table_open_cache_misses
- `mariadb_created_tmp_tables_total` — Created_tmp_tables
- `mariadb_created_tmp_disk_tables_total` — Created_tmp_disk_tables
- `mariadb_select_full_join_total` — Select_full_join
- `mariadb_select_scan_total` — Select_scan
- `mariadb_sort_merge_passes_total` — Sort_merge_passes
- `mariadb_uptime_seconds` — Uptime

#### Coletor: `global_variables`
Fonte: `SHOW GLOBAL VARIABLES`
Subconjunto selecionado:
- `mariadb_max_connections` — max_connections
- `mariadb_innodb_buffer_pool_size_bytes` — innodb_buffer_pool_size
- `mariadb_query_cache_size_bytes` — query_cache_size
- `mariadb_thread_cache_size` — thread_cache_size
- `mariadb_wait_timeout_seconds` — wait_timeout
- `mariadb_interactive_timeout_seconds` — interactive_timeout
- `mariadb_userstat_enabled` (gauge 0/1) — userstat

#### Coletor: `info`
Fonte: `SELECT VERSION(), @@global.hostname, @@global.server_id`
Métrica especial (info metric com valor sempre 1):
- `mariadb_info` (gauge=1) — labels: `version`, `version_comment`, `hostname`, `server_id`
Sempre habilitado, não pode ser desabilitado.

### 2.3 Health endpoint

- `GET /health` — retorna 200 `{"status":"ok"}` se conectado ao banco
- `GET /health` — retorna 503 `{"status":"error","message":"..."}` se desconectado

---

## 3. Arquitetura do projeto

```
mariadb_exporter/
├── cmd/
│   └── mariadb_exporter/
│       └── main.go                  # entrypoint, flags, bootstrap
├── collector/
│   ├── collector.go                 # interface Collector + registry
│   ├── info.go                      # coletor: info (sempre ativo)
│   ├── global_status.go             # coletor: global_status
│   ├── global_variables.go          # coletor: global_variables
│   ├── userstat.go                  # coletor: userstat
│   ├── tablestat.go                 # coletor: tablestat
│   ├── indexstat.go                 # coletor: indexstat
│   ├── clientstat.go                # coletor: clientstat
│   ├── query_response_time.go       # coletor: query_response_time
│   ├── metadata_locks.go            # coletor: metadata_locks
│   ├── disks.go                     # coletor: disks
│   ├── replication.go               # coletor: replication
│   ├── galera.go                    # coletor: galera (opt-in)
│   └── innodb.go                    # coletor: innodb
├── exporter/
│   ├── exporter.go                  # Exporter struct, Describe/Collect
│   └── version_detector.go          # detecta versão MariaDB e feature flags
├── config/
│   └── config.go                    # parsing de flags e variáveis de ambiente
├── web/
│   └── handler.go                   # HTTP handlers (/metrics, /health, /)
├── Dockerfile
├── docker-compose.yml               # para desenvolvimento local
├── go.mod
├── go.sum
├── Makefile
├── README.md
└── custom_metrics/
    └── example.yml                  # exemplo de custom metrics YAML
```

---

## 4. Interface do Coletor

Todos os coletores implementam esta interface:

```go
package collector

import (
    "context"
    "database/sql"
    "github.com/prometheus/client_golang/prometheus"
)

// Collector é a interface que todo coletor deve implementar.
type Collector interface {
    // Name retorna o nome do coletor (usado em flags e logs).
    Name() string

    // Help retorna a descrição do coletor para --help.
    Help() string

    // Enabled retorna se o coletor está habilitado (baseado em flags).
    Enabled() bool

    // Collect executa as queries e envia as métricas para o channel.
    Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error
}

// VersionInfo contém informações de versão detectadas na conexão.
type VersionInfo struct {
    Major   int
    Minor   int
    Patch   int
    Full    string    // string completa, ex: "11.4.3-MariaDB"
    IsMariaDB bool
}

// FeatureFlags indica quais recursos estão disponíveis nesta instância.
type FeatureFlags struct {
    HasUserStat          bool // userstat plugin ativo
    HasQueryResponseTime bool // query_response_time plugin ativo
    HasMetadataLockInfo  bool // metadata_lock_info plugin ativo
    HasDisksPlugin       bool // disks plugin ativo
    HasGalera            bool // Galera/wsrep ativo
    IsReplica            bool // instância é réplica
}
```

---

## 5. Exporter principal

```go
package exporter

// Exporter implementa prometheus.Collector.
// Gerencia o pool de conexões e orquestra os coletores.
type Exporter struct {
    db           *sql.DB
    collectors   []collector.Collector
    version      *collector.VersionInfo
    features     *collector.FeatureFlags
    scrapeDuration prometheus.Histogram
    scrapeSuccess  prometheus.Gauge
    scrapeErrors   *prometheus.CounterVec  // label: collector
    up             prometheus.Gauge
}
```

Comportamento do `Collect()`:
1. Verificar conectividade (`db.PingContext`)
2. Atualizar `mariadb_up` (1=conectado, 0=desconectado)
3. Para cada coletor habilitado, executar em goroutine com timeout de 30s
4. Coletar erros por coletor em `mariadb_scrape_errors_total{collector="nome"}`
5. Registrar duração total em `mariadb_scrape_duration_seconds`

---

## 6. Flags de linha de comando

```
--web.listen-address       Endereço e porta (padrão: ":9104")
--web.telemetry-path       Path das métricas (padrão: "/metrics")
--web.max-requests         Máximo de scrapes simultâneos (padrão: 0 = ilimitado)

--datasource.name          DSN de conexão (padrão: env MARIADB_DSN)
--datasource.max-open      Max conexões abertas (padrão: 3)
--datasource.max-idle      Max conexões idle (padrão: 3)
--datasource.timeout       Timeout de query em segundos (padrão: 30)

--collector.userstat               Habilita coletor userstat (padrão: true)
--collector.tablestat              Habilita coletor tablestat (padrão: true)
--collector.tablestat.limit        Limite de tabelas por scrape (padrão: 500)
--collector.indexstat              Habilita coletor indexstat (padrão: true)
--collector.indexstat.limit        Limite de índices por scrape (padrão: 1000)
--collector.clientstat             Habilita coletor clientstat (padrão: true)
--collector.query_response_time    Habilita coletor QRT (padrão: true)
--collector.metadata_locks         Habilita coletor MDL (padrão: true)
--collector.disks                  Habilita coletor disks (padrão: true)
--collector.replication            Habilita coletor replication (padrão: true)
--collector.galera                 Habilita coletor galera (padrão: false)
--collector.innodb                 Habilita coletor innodb (padrão: true)
--collector.global_status          Habilita coletor global_status (padrão: true)
--collector.global_variables       Habilita coletor global_variables (padrão: true)

--log.level                Log level: debug, info, warn, error (padrão: info)
--log.format               Formato do log: text, json (padrão: text)

--version                  Mostra versão do exporter e sai
```

Todas as flags `--datasource.*` e `--web.*` também aceitam variáveis de ambiente:
`MARIADB_DSN`, `MARIADB_WEB_LISTEN_ADDRESS`, etc.

---

## 7. Formato do DSN

Usar o driver `go-sql-driver/mysql` que é compatível com MariaDB:

```
# Formato básico
mariadb://user:password@tcp(host:port)/

# Exemplos
mariadb://pmm:senha@tcp(localhost:3306)/
mariadb://pmm:senha@tcp(192.168.1.10:3306)/
mariadb://pmm:senha@unix(/var/run/mysql/mysql.sock)/

# Com parâmetros
mariadb://pmm:senha@tcp(localhost:3306)/?timeout=30s&readTimeout=30s

# Via arquivo de credenciais MySQL (.my.cnf)
mariadb://@tcp(localhost:3306)/?readTimeout=30s  # lê ~/.my.cnf automaticamente
```

O prefixo `mariadb://` deve ser convertido para `mysql://` internamente antes
de passar ao driver, já que o `go-sql-driver/mysql` usa o protocolo MySQL.

---

## 8. Dependências (go.mod)

```
module github.com/Kevenny/mariadb-exporter

go 1.22

require (
    github.com/go-sql-driver/mysql          v1.7.1
    github.com/prometheus/client_golang     v1.19.0
    github.com/prometheus/common            v0.52.0
    github.com/prometheus/exporter-toolkit  v0.11.0
    github.com/alecthomas/kingpin/v2        v2.4.0
    github.com/go-kit/log                   v0.2.1
    gopkg.in/yaml.v3                        v3.0.1
)
```

---

## 9. Detecção automática de plugins

No startup e a cada 5 minutos (refresh assíncrono), verificar quais plugins
estão ativos para ajustar os coletores:

```sql
SELECT plugin_name, plugin_status
FROM information_schema.plugins
WHERE plugin_name IN (
    'QUERY_RESPONSE_TIME',
    'QUERY_RESPONSE_TIME_AUDIT',
    'METADATA_LOCK_INFO',
    'DISKS'
)
```

Verificar `userstat`:
```sql
SELECT VARIABLE_VALUE
FROM information_schema.GLOBAL_VARIABLES
WHERE VARIABLE_NAME = 'userstat'
```

Se um plugin necessário não estiver ativo, o coletor correspondente deve:
1. Logar aviso na primeira tentativa de scrape
2. Retornar zero métricas (sem erro)
3. Expor `mariadb_collector_available{collector="nome"}` = 0

---

## 10. Suporte a custom metrics via YAML

Implementar suporte a arquivo YAML de custom metrics, similar ao `mysqld_exporter`:

```yaml
# custom_metrics/example.yml
# Executado a cada scrape de resolução média

mariadb_active_sessions_by_schema:
  query: |
    SELECT db AS schema_name,
           command,
           COUNT(*) AS total
    FROM information_schema.PROCESSLIST
    WHERE db IS NOT NULL
    GROUP BY db, command
  metrics:
    - schema_name:
        usage: "LABEL"
        description: "Schema do banco"
    - command:
        usage: "LABEL"
        description: "Tipo de comando"
    - total:
        usage: "GAUGE"
        description: "Total de sessoes ativas por schema e comando"
```

Tipos de `usage` suportados: `LABEL`, `COUNTER`, `GAUGE`.

Flag para ativar: `--custom-metrics=/caminho/para/arquivo.yml`
Múltiplos arquivos: `--custom-metrics=a.yml --custom-metrics=b.yml`

---

## 11. Integração com PMM

### Registro como External Service

```bash
# Instalar o exporter no host MariaDB
sudo systemctl enable mariadb_exporter
sudo systemctl start mariadb_exporter

# Registrar no PMM
pmm-admin add external \
  --service-name=mariadb-01 \
  --listen-port=9104 \
  --group=mariadb \
  --environment=production \
  --cluster=meu-cluster
```

### Usuário de monitoramento mínimo necessário

```sql
CREATE USER 'mariadb_exporter'@'127.0.0.1'
  IDENTIFIED BY 'senha_forte'
  WITH MAX_USER_CONNECTIONS 5;

GRANT SELECT ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT PROCESS ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT REPLICATION CLIENT ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT RELOAD ON *.* TO 'mariadb_exporter'@'127.0.0.1';

-- Para o coletor de disks (requer acesso a tabelas de sistema)
GRANT SELECT ON information_schema.* TO 'mariadb_exporter'@'127.0.0.1';

FLUSH PRIVILEGES;
```

---

## 12. Systemd unit

```ini
# /etc/systemd/system/mariadb_exporter.service
[Unit]
Description=MariaDB Exporter for Prometheus
After=network.target mariadb.service
Wants=mariadb.service

[Service]
Type=simple
User=mariadb_exporter
Group=mariadb_exporter
EnvironmentFile=-/etc/mariadb_exporter/mariadb_exporter.env
ExecStart=/usr/local/bin/mariadb_exporter \
  --web.listen-address=":9104" \
  --log.level=info
Restart=on-failure
RestartSec=5s
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

```bash
# /etc/mariadb_exporter/mariadb_exporter.env
MARIADB_DSN=mariadb://mariadb_exporter:senha@tcp(localhost:3306)/
```

---

## 13. Dockerfile

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" \
    -o mariadb_exporter ./cmd/mariadb_exporter/

FROM scratch
COPY --from=builder /build/mariadb_exporter /mariadb_exporter
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 9104
USER 65534
ENTRYPOINT ["/mariadb_exporter"]
```

---

## 14. Makefile

```makefile
VERSION     ?= $(shell git describe --tags --always --dirty)
BUILD_DATE  ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS     := -ldflags "-s -w -X main.version=$(VERSION) -X main.buildDate=$(BUILD_DATE)"

.PHONY: build test lint docker clean

build:
	go build $(LDFLAGS) -o bin/mariadb_exporter ./cmd/mariadb_exporter/

test:
	go test -v -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t mariadb_exporter:$(VERSION) \
		-t mariadb_exporter:latest \
		.

clean:
	rm -rf bin/ coverage.out

run-dev:
	MARIADB_DSN="mariadb://root:@tcp(localhost:3306)/" \
	go run ./cmd/mariadb_exporter/ --log.level=debug
```

---

## 15. Testes

Cada coletor deve ter testes unitários com banco em memória ou mock:

```go
// collector/userstat_test.go

func TestUserStatCollector(t *testing.T) {
    // Usar sqlmock para simular respostas do MariaDB
    db, mock, err := sqlmock.New()
    require.NoError(t, err)
    defer db.Close()

    // Setup expectativas do mock
    rows := sqlmock.NewRows([]string{
        "user", "total_connections", "rows_read", "rows_sent",
        "rows_changed", "select_commands", "update_commands",
        "other_commands", "access_denied", "lost_connections",
    }).AddRow("kevenny", 100, 50000, 48000, 2000, 80, 15, 5, 0, 0)

    mock.ExpectQuery("SELECT .* FROM information_schema.USER_STATISTICS").
        WillReturnRows(rows)

    // Executar coletor
    c := NewUserStatCollector()
    ch := make(chan prometheus.Metric, 100)
    err = c.Collect(context.Background(), db, ch)
    require.NoError(t, err)

    // Validar métricas geradas
    close(ch)
    metrics := collectMetrics(ch)
    assert.Equal(t, float64(100), getMetricValue(metrics, "mariadb_user_total_connections_total", "user", "kevenny"))
}
```

Usar `github.com/DATA-DOG/go-sqlmock` para mocks.
Usar `github.com/stretchr/testify` para assertions.

---

## 16. Métricas internas do exporter

Além das métricas do MariaDB, o exporter expõe suas próprias métricas:

```
mariadb_up                                         # 1 se conectado
mariadb_exporter_build_info{version,build_date}    # info do binário
mariadb_scrape_duration_seconds                    # histograma de duração
mariadb_scrape_errors_total{collector}             # erros por coletor
mariadb_collector_available{collector}             # 1 se plugin disponível
mariadb_collector_scrape_duration_seconds{collector} # duração por coletor
```

---

## 17. Comportamento de erro esperado

- Plugin não disponível → log warn + `mariadb_collector_available=0` + zero métricas (sem erro no scrape)
- Timeout de query → log error + incrementa `mariadb_scrape_errors_total` + continua demais coletores
- Banco offline → `mariadb_up=0` + expõe métricas internas + retorna rapidamente
- Query retorna 0 linhas → métricas não emitidas (sem erro)
- Permissão negada → log error com query e usuário afetado + `mariadb_scrape_errors_total`
- Versão incompatível (não MariaDB) → erro fatal no startup com mensagem clara

---

## 18. Ordem de implementação sugerida

Implementar nesta ordem para ter um exporter funcional rapidamente:

1. `cmd/mariadb_exporter/main.go` — bootstrap, flags, HTTP server
2. `exporter/version_detector.go` — detecção de versão e feature flags
3. `collector/collector.go` — interface e registry
4. `collector/info.go` — coletor mais simples, valida a estrutura
5. `exporter/exporter.go` — orquestração dos coletores
6. `web/handler.go` — /metrics e /health
7. `collector/global_status.go` — métricas básicas de status
8. `collector/global_variables.go` — variáveis de configuração
9. `collector/userstat.go` + `tablestat.go` + `indexstat.go` + `clientstat.go`
10. `collector/query_response_time.go` — histograma Prometheus real
11. `collector/metadata_locks.go` + `disks.go`
12. `collector/replication.go` — SHOW ALL SLAVES STATUS
13. `collector/innodb.go`
14. `collector/galera.go` (opt-in)
15. Suporte a custom metrics YAML
16. Testes unitários por coletor
17. Dockerfile + Makefile + systemd unit

---

## 19. Referências de código

Estudar antes de implementar:

- `mysqld_exporter` (Prometheus community):
  https://github.com/prometheus/mysqld_exporter
  — estrutura de coletores, padrão de flags, tratamento de erros

- `oracledb_exporter` (iamseth):
  https://github.com/iamseth/oracledb_exporter
  — custom metrics TOML, integração com PMM como external

- `node_exporter` (Prometheus):
  https://github.com/prometheus/node_exporter
  — padrão de flags `--collector.*`, registro dinâmico

- `prometheus/client_golang`:
  https://github.com/prometheus/client_golang
  — uso correto de Counter, Gauge, Histogram, Info metric

- MariaDB Information Schema docs:
  https://mariadb.com/kb/en/information-schema-user_statistics-table/
  https://mariadb.com/kb/en/information-schema-table_statistics-table/
  https://mariadb.com/kb/en/information-schema-index_statistics-table/
  https://mariadb.com/kb/en/query-response-time-plugin/
  https://mariadb.com/kb/en/metadata_lock_info/
  https://mariadb.com/kb/en/information-schema-disks-table/

- PMM External Services:
  https://docs.percona.com/percona-monitoring-and-management/3/install-pmm/install-pmm-client/connect-database/external.html

---

## 20. Critérios de aceitação

O exporter está pronto quando:

- [ ] `go build` sem erros ou warnings
- [ ] `go test ./...` com 100% de coletores cobertos por testes
- [ ] `golangci-lint run` sem issues
- [ ] Ao iniciar contra MariaDB 11.4, detecta versão corretamente
- [ ] Ao iniciar contra MySQL 8.0, encerra com erro descritivo
- [ ] `/metrics` retorna métricas válidas no formato Prometheus
- [ ] `/health` retorna 200 quando conectado, 503 quando desconectado
- [ ] `pmm-admin add external --listen-port=9104` funciona sem erros
- [ ] Dashboards Grafana conseguem plotar todas as métricas expostas
- [ ] Com `userstat=OFF`, coletores correspondentes retornam zero métricas sem erro
- [ ] Com plugin `query_response_time` inativo, coletor QRT retorna zero métricas sem erro
- [ ] Replication multi-source (SHOW ALL SLAVES STATUS) funciona com 0, 1 ou N slaves
- [ ] Dockerfile produz imagem funcional
- [ ] README documenta instalação, configuração e integração com PMM

---

*Documento gerado para uso exclusivo com Claude Code.*
*Autor do projeto: Kevenny.*
*Ambiente alvo: MariaDB 11.4 Community em Oracle Linux 9 / OCI.*
