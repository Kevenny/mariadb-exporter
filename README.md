# mariadb_exporter

Exporter Prometheus **dedicado ao MariaDB**, criado para cobrir as lacunas do
`mysqld_exporter` quando o alvo é MariaDB — em especial os plugins e recursos que
não existem no MySQL e por isso não têm coleta estruturada.

O que o `mysqld_exporter` não entrega e este exporter sim:

- **Estatísticas de `userstat`** — `USER_STATISTICS`, `TABLE_STATISTICS`,
  `INDEX_STATISTICS` e `CLIENT_STATISTICS`, incluindo detecção de índices sem uso
- **`query_response_time`** exposto como histograma Prometheus nativo
- **`METADATA_LOCK_INFO`** — metadata locks ativos e em espera
- **`DISKS`** — ocupação dos filesystems vista pelo próprio servidor
- **Replicação multi-source** via `SHOW ALL SLAVES STATUS`
- **Detecção automática de versão**: se a instância não for MariaDB, o exporter
  encerra no startup com mensagem clara em vez de falhar silenciosamente
- **Detecção automática de plugins**: um plugin desligado gera zero métricas e
  `mariadb_collector_available=0`, sem transformar o scrape em erro

---

## Sumário

- [Instalação](#instalação)
- [Configuração](#configuração)
- [Usuário de monitoramento](#usuário-de-monitoramento)
- [Pré-requisitos por coletor](#pré-requisitos-por-coletor)
- [Métricas expostas](#métricas-expostas)
- [Custom metrics](#custom-metrics)
- [Integração com PMM](#integração-com-pmm)
- [Endpoints](#endpoints)
- [Desenvolvimento](#desenvolvimento)
- [Troubleshooting](#troubleshooting)

---

## Instalação

### A partir do código-fonte

Requer Go 1.22 ou superior.

```bash
git clone https://github.com/Kevenny/mariadb-exporter.git
cd mariadb-exporter
make build          # gera bin/mariadb_exporter
```

### Docker

```bash
make docker

docker run -d --name mariadb_exporter \
  -p 9104:9104 \
  -e MARIADB_DSN="mariadb://mariadb_exporter:senha@tcp(10.0.0.10:3306)/" \
  mariadb_exporter:latest
```

### Systemd (Oracle Linux 9 / RHEL / Ubuntu)

```bash
# Binário
sudo install -m 0755 bin/mariadb_exporter /usr/local/bin/mariadb_exporter

# Usuário de serviço sem shell nem home
sudo useradd --system --no-create-home --shell /sbin/nologin mariadb_exporter

# Configuração (contém a senha — 640, dono root, grupo do serviço)
sudo mkdir -p /etc/mariadb_exporter
sudo install -m 0640 -o root -g mariadb_exporter \
  packaging/systemd/mariadb_exporter.env /etc/mariadb_exporter/mariadb_exporter.env
sudo vi /etc/mariadb_exporter/mariadb_exporter.env   # ajuste o MARIADB_DSN

# Unit
sudo install -m 0644 packaging/systemd/mariadb_exporter.service \
  /etc/systemd/system/mariadb_exporter.service

sudo systemctl daemon-reload
sudo systemctl enable --now mariadb_exporter
sudo systemctl status mariadb_exporter
```

---

## Configuração

O DSN é obrigatório e pode vir da flag `--datasource.name` ou da variável de
ambiente `MARIADB_DSN`.

### Formato do DSN

O prefixo `mariadb://` é aceito por clareza semântica e convertido internamente
antes de chegar ao driver, que fala o protocolo MySQL.

```bash
# TCP
mariadb://pmm:senha@tcp(localhost:3306)/
mariadb://pmm:senha@tcp(192.168.1.10:3306)/

# Socket Unix
mariadb://pmm:senha@unix(/var/run/mysql/mysql.sock)/

# Com parâmetros do driver
mariadb://pmm:senha@tcp(localhost:3306)/?timeout=30s&readTimeout=30s

# Sem credenciais: o driver lê ~/.my.cnf
mariadb://@tcp(localhost:3306)/?readTimeout=30s
```

> A senha nunca aparece nos logs — o DSN é mascarado antes de ser logado.

### Flags

| Flag | Padrão | Descrição |
| --- | --- | --- |
| `--web.listen-address` | `:9104` | Endereço e porta de escuta |
| `--web.telemetry-path` | `/metrics` | Path das métricas |
| `--web.max-requests` | `0` | Máximo de scrapes simultâneos (0 = ilimitado) |
| `--datasource.name` | env `MARIADB_DSN` | DSN de conexão |
| `--datasource.max-open` | `3` | Máximo de conexões abertas |
| `--datasource.max-idle` | `3` | Máximo de conexões idle |
| `--datasource.timeout` | `30s` | Timeout de query |
| `--collector.userstat` | `true` | Coletor `userstat` |
| `--collector.tablestat` | `true` | Coletor `tablestat` |
| `--collector.tablestat.limit` | `500` | Limite de tabelas por scrape |
| `--collector.indexstat` | `true` | Coletor `indexstat` |
| `--collector.indexstat.limit` | `1000` | Limite de índices por scrape |
| `--collector.clientstat` | `true` | Coletor `clientstat` |
| `--collector.query_response_time` | `true` | Coletor de tempo de resposta |
| `--collector.metadata_locks` | `true` | Coletor de metadata locks |
| `--collector.disks` | `true` | Coletor de uso de disco |
| `--collector.replication` | `true` | Coletor de replicação |
| `--collector.galera` | `false` | Coletor Galera (**opt-in**) |
| `--collector.innodb` | `true` | Coletor InnoDB |
| `--collector.global_status` | `true` | Coletor `global_status` |
| `--collector.global_variables` | `true` | Coletor `global_variables` |
| `--custom-metrics` | — | Arquivo YAML de custom metrics (repetível) |
| `--pmm.service-name` | `$(hostname)-mariadb` | Nome do serviço no PMM inventory |
| `--pmm.cluster` | — | Nome do cluster para agrupamento no PMM |
| `--pmm.environment` | `production` | Ambiente (production, staging, dev) |
| `--pmm.replication-set` | — | Nome do replication set no PMM (opcional) |
| `--log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `--log.format` | `text` | `text` ou `json` |
| `--version` | — | Mostra a versão e sai |

As flags `--pmm.*` populam ConstLabels (`service_name`, `cluster`, `environment`,
`replication_set`) em `mariadb_info`, `mariadb_up` e nas demais métricas internas
do exporter — são o que permite os dashboards do PMM filtrarem por essas
dimensões. Campos não informados simplesmente não geram label (nenhuma métrica
ganha um label vazio). Veja [Integração com PMM](#integração-com-pmm) para o
fluxo completo.

Qualquer coletor pode ser desligado com o prefixo `--no-`, por exemplo
`--no-collector.tablestat`.

### Variáveis de ambiente

As flags de `--web.*` e `--datasource.*` também leem do ambiente:

| Variável | Flag equivalente |
| --- | --- |
| `MARIADB_DSN` | `--datasource.name` |
| `MARIADB_WEB_LISTEN_ADDRESS` | `--web.listen-address` |
| `MARIADB_WEB_TELEMETRY_PATH` | `--web.telemetry-path` |
| `MARIADB_WEB_MAX_REQUESTS` | `--web.max-requests` |
| `MARIADB_DATASOURCE_MAX_OPEN` | `--datasource.max-open` |
| `MARIADB_DATASOURCE_MAX_IDLE` | `--datasource.max-idle` |
| `MARIADB_DATASOURCE_TIMEOUT` | `--datasource.timeout` |
| `MARIADB_PMM_SERVICE_NAME` | `--pmm.service-name` |
| `MARIADB_PMM_CLUSTER` | `--pmm.cluster` |
| `MARIADB_PMM_ENVIRONMENT` | `--pmm.environment` |
| `MARIADB_PMM_REPLICATION_SET` | `--pmm.replication-set` |
| `MARIADB_LOG_LEVEL` | `--log.level` |
| `MARIADB_LOG_FORMAT` | `--log.format` |

A flag explícita sempre tem precedência sobre a variável de ambiente.

---

## Usuário de monitoramento

Privilégios mínimos necessários:

```sql
CREATE USER 'mariadb_exporter'@'127.0.0.1'
  IDENTIFIED BY 'senha_forte'
  WITH MAX_USER_CONNECTIONS 5;

GRANT SELECT              ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT PROCESS             ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT REPLICATION CLIENT  ON *.* TO 'mariadb_exporter'@'127.0.0.1';
GRANT RELOAD              ON *.* TO 'mariadb_exporter'@'127.0.0.1';

FLUSH PRIVILEGES;
```

Para que se usa cada privilégio:

| Privilégio | Necessário para |
| --- | --- |
| `SELECT` | Tabelas de `information_schema` (userstat, QRT, MDL, DISKS) |
| `PROCESS` | `SHOW ENGINE INNODB STATUS`, `information_schema.PROCESSLIST` |
| `REPLICATION CLIENT` | `SHOW ALL SLAVES STATUS` |
| `RELOAD` | Operações administrativas de flush |

---

## Pré-requisitos por coletor

Cada coletor com dependência externa publica
`mariadb_collector_available{collector="nome"}`. Quando a dependência não está
satisfeita, o coletor **não gera erro** — apenas zero métricas e um aviso no log.

| Coletor | Pré-requisito | Como habilitar |
| --- | --- | --- |
| `userstat`, `tablestat`, `indexstat`, `clientstat` | variável `userstat=ON` | `SET GLOBAL userstat = ON` |
| `query_response_time` | plugin + `query_response_time_stats=ON` | `INSTALL SONAME 'query_response_time'; SET GLOBAL query_response_time_stats = ON;` |
| `metadata_locks` | plugin `metadata_lock_info`, MariaDB >= 10.0.7 | `INSTALL SONAME 'metadata_lock_info';` |
| `disks` | plugin `disks` | `INSTALL SONAME 'disks';` |
| `galera` | wsrep ativo + `--collector.galera` | cluster Galera em execução |
| `replication` | `REPLICATION CLIENT` | — |

Para tornar as mudanças permanentes, use o arquivo de configuração do servidor:

```ini
# /etc/my.cnf.d/monitoring.cnf
[mariadb]
userstat = 1
plugin_load_add = query_response_time
query_response_time_stats = ON
plugin_load_add = metadata_lock_info
plugin_load_add = disks
```

---

## Métricas expostas

### Instância

| Métrica | Tipo | Labels |
| --- | --- | --- |
| `mariadb_info` | gauge (=1) | `version`, `version_comment`, `hostname`, `server_id` |

### `global_status` (lista explícita, sem wildcard)

`mariadb_connections_total`, `mariadb_max_used_connections`,
`mariadb_aborted_connects_total`, `mariadb_aborted_clients_total`,
`mariadb_bytes_received_total`, `mariadb_bytes_sent_total`,
`mariadb_questions_total`, `mariadb_slow_queries_total`, `mariadb_open_files`,
`mariadb_open_tables`, `mariadb_table_open_cache_hits_total`,
`mariadb_table_open_cache_misses_total`, `mariadb_created_tmp_tables_total`,
`mariadb_created_tmp_disk_tables_total`, `mariadb_select_full_join_total`,
`mariadb_select_scan_total`, `mariadb_sort_merge_passes_total`,
`mariadb_uptime_seconds`

### `global_variables`

`mariadb_max_connections`, `mariadb_innodb_buffer_pool_size_bytes`,
`mariadb_query_cache_size_bytes`, `mariadb_thread_cache_size`,
`mariadb_wait_timeout_seconds`, `mariadb_interactive_timeout_seconds`,
`mariadb_userstat_enabled`

### `userstat` — label `user`

`mariadb_user_total_connections_total`, `mariadb_user_concurrent_connections`,
`mariadb_user_rows_read_total`, `mariadb_user_rows_sent_total`,
`mariadb_user_rows_changed_total`, `mariadb_user_select_commands_total`,
`mariadb_user_update_commands_total`, `mariadb_user_other_commands_total`,
`mariadb_user_access_denied_total`, `mariadb_user_lost_connections_total`

### `tablestat` — labels `schema`, `table`

`mariadb_table_rows_read_total`, `mariadb_table_rows_changed_total`,
`mariadb_table_rows_changed_x_indexes_total`

As tabelas são ordenadas por `rows_read DESC` antes do corte, então o limite
preserva as mais movimentadas.

### `indexstat` — labels `schema`, `table`, `index`

`mariadb_index_rows_read_total`, `mariadb_index_unused` (gauge=1, presente apenas
para índices com `rows_read = 0`)

### `clientstat` — label `client`

`mariadb_client_total_connections_total`, `mariadb_client_rows_read_total`,
`mariadb_client_rows_sent_total`

### `query_response_time`

`mariadb_query_response_time_seconds` — histograma Prometheus nativo, com
`_bucket`, `_count` e `_sum`. Os limites dos buckets vêm da própria tabela; a
linha `TOO LONG` não gera bucket mas entra no `_count` e no `_sum`.

### `metadata_locks` — labels `lock_mode`, `lock_type`, `table_schema`, `table_name`

`mariadb_metadata_locks_total`, `mariadb_metadata_lock_waiting_total`

### `disks` — labels `disk`, `path`

`mariadb_disk_total_bytes`, `mariadb_disk_used_bytes`,
`mariadb_disk_available_bytes`

### `replication` — labels `connection_name`, `master_host`, `master_port`

`mariadb_slave_sql_running`, `mariadb_slave_io_running`,
`mariadb_slave_seconds_behind_master`, `mariadb_slave_last_errno`,
`mariadb_slave_relay_log_pos`

Multi-source é suportado: cada conexão de replicação vira uma série própria,
distinguida por `connection_name`.

### `innodb`

`mariadb_innodb_buffer_pool_read_requests_total`,
`mariadb_innodb_buffer_pool_reads_total`,
`mariadb_innodb_buffer_pool_pages_total{type}` (`free`, `data`, `dirty`, `misc`,
`total`), `mariadb_innodb_row_lock_waits_total`,
`mariadb_innodb_row_lock_time_avg_milliseconds`, `mariadb_innodb_deadlocks_total`

### `galera` (opt-in) — label `cluster_name`

`mariadb_galera_cluster_size`, `mariadb_galera_cluster_status` (0=non-primary,
1=primary), `mariadb_galera_local_state` (0=joining, 1=donor, 2=joined,
3=synced), `mariadb_galera_flow_control_paused`,
`mariadb_galera_recv_queue_avg`, `mariadb_galera_send_queue_avg`

### Métricas internas do exporter

| Métrica | Descrição |
| --- | --- |
| `mariadb_up` | 1 se conectado ao banco |
| `mariadb_exporter_build_info{version,build_date,go_version}` | Info do binário |
| `mariadb_scrape_duration_seconds` | Histograma da duração total do scrape |
| `mariadb_scrape_success` | 1 se nenhum coletor falhou |
| `mariadb_scrape_errors_total{collector}` | Erros por coletor |
| `mariadb_collector_available{collector}` | 1 se as dependências do coletor estão OK |
| `mariadb_collector_scrape_duration_seconds{collector}` | Duração por coletor |

---

## Custom metrics

Métricas próprias podem ser definidas em YAML, sem recompilar:

```yaml
mariadb_active_sessions_by_schema:
  query: |
    SELECT db AS schema_name, command, COUNT(*) AS total
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

```bash
mariadb_exporter --custom-metrics=/etc/mariadb_exporter/custom.yml
```

- `usage` aceita `LABEL`, `COUNTER` e `GAUGE`
- Vários arquivos: repita a flag (`--custom-metrics=a.yml --custom-metrics=b.yml`)
- Com mais de uma coluna de valor, o nome da coluna é sufixado ao nome da métrica
- Um YAML inválido é erro de configuração e impede o startup, em vez de falhar
  silenciosamente no primeiro scrape

Veja [custom_metrics/example.yml](custom_metrics/example.yml) para mais exemplos.

---

## Integração com PMM

Documentação completa e detalhada em
[mariadb_exporter_pmm_integration.md](mariadb_exporter_pmm_integration.md).
Resumo do fluxo:

```
mariadb_exporter (:9104) → pull do pmm-agent → VictoriaMetrics (PMM) → Grafana
```

O PMM não aceita datasources externos arbitrários — internamente ele é sempre
VictoriaMetrics. O que se registra é um **target de scrape** (External
Service); o exporter nunca envia métricas, apenas aguarda o pull.

### 1. Configurar as flags `--pmm.*`

```bash
mariadb_exporter \
  --pmm.service-name="mariadb-$(hostname -s)" \
  --pmm.cluster="meu-cluster" \
  --pmm.environment=production
```

Isso faz `mariadb_info`, `mariadb_up` e as demais métricas internas carregarem
os labels `service_name`, `cluster` e `environment`, usados pelos filtros dos
dashboards.

### 2. Registrar como External Service

```bash
pmm-admin add external \
  --service-name="mariadb-$(hostname -s)" \
  --listen-port=9104 \
  --metrics-path=/metrics \
  --scheme=http \
  --group=mariadb \
  --environment=production \
  --cluster=meu-cluster
```

Conferir: `pmm-admin list | grep mariadb` deve mostrar o serviço com status UP.

### 3. Deploy automatizado

[packaging/deploy_mariadb_exporter.sh](packaging/deploy_mariadb_exporter.sh)
automatiza os passos 1 e 2 num host novo — instala o binário, cria o serviço
systemd com as flags `--pmm.*` já preenchidas e registra no PMM:

```bash
MARIADB_DSN="mariadb://mariadb_exporter:SENHA@tcp(localhost:3306)/" \
PMM_CLUSTER="meu-cluster" \
  sudo -E ./packaging/deploy_mariadb_exporter.sh
```

### 4. Dashboard e alertas versionados

- [dashboards/mariadb_overview.json](dashboards/mariadb_overview.json) —
  dashboard com variáveis de template `cluster`/`environment`/`service_name`,
  cobrindo disponibilidade, latência (P99 via histograma real), top usuários e
  tabelas por linhas lidas, índices sem uso, disco e replicação. Importar via
  PMM UI → Dashboards → Import, ou pela API do Grafana embutido:

  ```bash
  curl -k -u admin:admin -X POST https://<pmm-server>/graph/api/dashboards/db \
    -H "Content-Type: application/json" \
    -d "{\"dashboard\": $(cat dashboards/mariadb_overview.json), \"folderTitle\": \"MariaDB\", \"overwrite\": true}"
  ```

- [dashboards/mariadb_alerts.yml](dashboards/mariadb_alerts.yml) — 5 regras de
  alerta prontas (instância down, latência P99 alta, replicação atrasada,
  disco quase cheio, acúmulo de metadata locks). Importar via PMM UI →
  Alerting → Alert Rules.

### 5. Verificação pós-registro

```bash
# Métricas chegando com os labels do PMM
curl -s http://localhost:9104/metrics | grep -E "^mariadb_up|^mariadb_info"

# No Grafana do PMM: Dashboards → Advanced Data Exploration
# Datasource: Prometheus (VictoriaMetrics interno)
# Filtrar por: service_name, cluster, environment
```

---

## Endpoints

| Endpoint | Descrição |
| --- | --- |
| `GET /metrics` | Métricas no formato Prometheus |
| `GET /health` | `200 {"status":"ok"}` conectado; `503 {"status":"error","message":"..."}` desconectado |
| `GET /` | Página de índice com links |

O `/health` é adequado para health check de load balancer, Kubernetes probe ou
`healthcheck` do Docker.

---

## Desenvolvimento

```bash
make build      # compila em bin/
make test       # go test -race com cobertura
make vet        # go vet
make lint       # golangci-lint
make fmt        # gofmt -s -w
make docker     # imagem Docker
```

### Ambiente local completo

O `docker-compose.yml` sobe um MariaDB 11.4 com todos os plugins habilitados e o
exporter apontado para ele:

```bash
docker compose up -d --build

curl -s localhost:9104/health
curl -s localhost:9104/metrics | grep mariadb_collector_available
```

### Estrutura

```
cmd/mariadb_exporter/    entrypoint, flags, bootstrap do servidor HTTP
collector/               interface Collector + um arquivo por coletor
exporter/                orquestração dos coletores, detecção de versão/plugins
config/                  parsing de flags e variáveis de ambiente
web/                     handlers /metrics, /health e /
packaging/systemd/       unit e EnvironmentFile
custom_metrics/          exemplo de custom metrics YAML
```

### Como adicionar um coletor

1. Crie `collector/meu_coletor.go` embutindo `base` e implementando
   `Collect(ctx, db, ch) error`
2. Se ele depender de um plugin, implemente também `Available() bool` e adicione
   a detecção em `exporter/version_detector.go`
3. Adicione a flag em `config/config.go` e registre-o em `buildCollectors`
   (`cmd/mariadb_exporter/main.go`)
4. Escreva o teste com `sqlmock`, cobrindo o caminho felizes **e** o de
   dependência ausente

---

## Troubleshooting

**O exporter encerra com "instância não é MariaDB"**
Comportamento esperado ao apontar para MySQL. Este exporter depende de tabelas e
comandos exclusivos do MariaDB; para MySQL use o `mysqld_exporter`.

**`mariadb_collector_available{collector="userstat"} = 0`**
A variável `userstat` está `OFF`. Habilite com `SET GLOBAL userstat = ON` e
torne permanente no `my.cnf`.

**Nenhuma métrica de `query_response_time`**
Confirme os dois requisitos — plugin instalado **e** coleta ligada:

```sql
SELECT plugin_name, plugin_status FROM information_schema.plugins
 WHERE plugin_name LIKE 'QUERY_RESPONSE%';
SELECT @@global.query_response_time_stats;
```

**`mariadb_up = 0`**
Verifique o DSN, o firewall e se o usuário tem permissão de conexão a partir do
host do exporter. O log traz o erro do driver; o `/health` traz a mesma
mensagem via HTTP.

**Muitas séries de `tablestat` / `indexstat`**
Reduza os limites: `--collector.tablestat.limit=100`,
`--collector.indexstat.limit=200`. As tabelas e índices são ordenados por
`rows_read DESC`, então o corte mantém os mais relevantes.

**Timeout nos scrapes**
Aumente `--datasource.timeout` e considere desabilitar os coletores mais caros
em instâncias com muitas tabelas (`--no-collector.tablestat`).

---

## Notas de implementação

Pontos em que a realidade do servidor difere da especificação original e como
foram tratados — todos validados contra um MariaDB 11.4.12 real.

**`USER_STATISTICS` não tem coluna `ROWS_CHANGED`**
A especificação pede a métrica `mariadb_user_rows_changed_total`, mas a tabela
`information_schema.USER_STATISTICS` do MariaDB decompõe esse dado em
`ROWS_DELETED`, `ROWS_INSERTED` e `ROWS_UPDATED` (diferente de
`TABLE_STATISTICS`, que realmente tem `ROWS_CHANGED`). A métrica é calculada como
a soma das três colunas, preservando a semântica pedida. Sem isso o coletor
`userstat` falharia com `Unknown column 'ROWS_CHANGED'`.

**Dois nomes de métrica divergem da convenção do Prometheus**
O `promtool check metrics` aponta dois avisos de estilo:

- `mariadb_innodb_buffer_pool_pages_total` é um gauge com sufixo `_total`
- `mariadb_innodb_row_lock_time_avg_milliseconds` usa milissegundos em vez da
  unidade base `seconds`

Ambos os nomes são exigidos literalmente pela especificação (seção 2.2) e foram
mantidos: renomeá-los quebraria os dashboards que consultam esses nomes. O
restante da saída passa no `promtool` sem observações.

**Fonte das métricas do InnoDB**
A especificação cita `SHOW ENGINE INNODB STATUS` como fonte. Os valores pedidos
(buffer pool, row locks) estão todos disponíveis como contadores em
`SHOW GLOBAL STATUS`, que é estruturado e estável entre versões — parsear o texto
livre do `INNODB STATUS` seria frágil. O texto é usado apenas como fallback para
`mariadb_innodb_deadlocks_total`, que não existe como variável de status em todas
as builds.

**Versão de `prometheus/common`**
A especificação indica `v0.52.0`, que nunca foi publicada no repositório
upstream. O projeto usa `v0.52.3`, a release real mais próxima.

---

## Licença

Uso interno.
