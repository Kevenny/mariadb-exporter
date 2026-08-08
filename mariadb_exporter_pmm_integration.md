# mariadb_exporter — Integração com Percona PMM

> Documento complementar à especificação principal `mariadb_exporter_spec.md`.
> Cobre exclusivamente a integração do exporter com o PMM 3.x como External Service.
> Leia este arquivo após implementar o exporter base.

---

## 1. Conceito fundamental — como o PMM enxerga o exporter

O PMM **não aceita datasources externos arbitrários**. Internamente, o datasource
é fixo: **VictoriaMetrics**. O que se registra no PMM são **targets de scrape**
(External Services), e o PMM faz o pull das métricas automaticamente para dentro
do VictoriaMetrics, tornando-as disponíveis no Grafana via datasource "Prometheus".

```
mariadb_exporter (:9104)
        ↑
        │  HTTP GET /metrics (a cada 10s)
        │
   pmm-agent
        │
        ↓
  VictoriaMetrics (PMM interno)
        │
        ↓
   Grafana (datasource: Prometheus)
```

O exporter nunca "envia" métricas para o PMM — ele apenas as expõe via HTTP
e aguarda o pull do pmm-agent.

---

## 2. Pré-requisitos

### 2.1 No host do MariaDB

- `mariadb_exporter` instalado e rodando em `:9104`
- `pmm-client` instalado e conectado ao PMM Server
- Porta `9104` acessível pelo pmm-agent (localhost ou rede interna)
- Usuário `mariadb_exporter` criado no MariaDB (ver Seção 5)

### 2.2 No PMM Server

- PMM 3.x instalado e acessível
- pmm-client conectado: `pmm-admin status` deve retornar `Connected`

Verificar conexão:
```bash
pmm-admin status
```

Saída esperada:
```
Agent ID: /agent_id/xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
Agent Version: 2.x.x
PMM Server: https://pmm-server:443
...
Connection: Connected
```

---

## 3. Flags adicionais obrigatórias no exporter para PMM

Adicionar ao `cmd/mariadb_exporter/main.go` além das flags do spec principal:

```
--pmm.service-name     Nome do serviço no PMM inventory
                       Padrão: $(hostname)-mariadb
                       Exemplo: mariadb-central-rech

--pmm.cluster          Nome do cluster para agrupamento no PMM
                       Exemplo: rech-cloud

--pmm.environment      Ambiente (production, staging, dev)
                       Padrão: production

--pmm.replication-set  Nome do replication set (opcional)
                       Exemplo: mariadb-11-4-primary
```

Essas flags populam ConstLabels nas métricas internas do exporter, permitindo
que os filtros de ambiente e cluster do PMM funcionem corretamente nos dashboards.

### Implementação das ConstLabels

```go
// exporter/exporter.go

func NewExporter(cfg *config.Config, collectors []collector.Collector) *Exporter {
    constLabels := prometheus.Labels{
        "service_name": cfg.PMM.ServiceName,
        "cluster":      cfg.PMM.Cluster,
        "environment":  cfg.PMM.Environment,
    }
    if cfg.PMM.ReplicationSet != "" {
        constLabels["replication_set"] = cfg.PMM.ReplicationSet
    }

    return &Exporter{
        up: prometheus.NewGauge(prometheus.GaugeOpts{
            Name:        "mariadb_up",
            Help:        "Instância MariaDB disponível (1=sim, 0=não).",
            ConstLabels: constLabels,
        }),
        // demais métricas internas também recebem constLabels
    }
}
```

---

## 4. Endpoint /health compatível com PMM

O PMM executa um `GET /health` durante o `pmm-admin add external` para validar
a conectividade antes de registrar o serviço. Se retornar qualquer código != 200,
o registro é rejeitado com `Connection check failed`.

### Implementação obrigatória

```go
// web/handler.go

func HealthHandler(db *sql.DB) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
        defer cancel()

        w.Header().Set("Content-Type", "application/json")

        if err := db.PingContext(ctx); err != nil {
            w.WriteHeader(http.StatusServiceUnavailable) // 503
            json.NewEncoder(w).Encode(map[string]string{
                "status":  "error",
                "message": err.Error(),
            })
            return
        }

        w.WriteHeader(http.StatusOK) // 200 — PMM aceita o registro
        json.NewEncoder(w).Encode(map[string]string{
            "status": "ok",
        })
    }
}
```

### Rotas HTTP obrigatórias

```
GET /metrics   → métricas Prometheus (scrape endpoint)
GET /health    → health check para PMM (deve retornar 200 quando saudável)
GET /          → página HTML com links para /metrics e /health (informativo)
```

---

## 5. Usuário MariaDB com privilégios mínimos

Executar em **cada instância** MariaDB monitorada:

```sql
-- Criar usuário dedicado ao exporter
CREATE USER 'mariadb_exporter'@'127.0.0.1'
  IDENTIFIED BY 'SENHA_FORTE_AQUI'
  WITH MAX_USER_CONNECTIONS 5;

-- Privilégios mínimos necessários por coletor:
-- global_status, global_variables, info → SELECT
-- replication → REPLICATION CLIENT
-- userstat, tablestat, indexstat → SELECT (já coberto)
-- metadata_locks, disks → SELECT (já coberto)
-- innodb → PROCESS
-- flush de contadores (opcional) → RELOAD

GRANT SELECT    ON *.*  TO 'mariadb_exporter'@'127.0.0.1';
GRANT PROCESS   ON *.*  TO 'mariadb_exporter'@'127.0.0.1';
GRANT REPLICATION CLIENT ON *.* TO 'mariadb_exporter'@'127.0.0.1';

-- RELOAD só se quiser suportar FLUSH QUERY_RESPONSE_TIME via exporter
-- GRANT RELOAD ON *.* TO 'mariadb_exporter'@'127.0.0.1';

FLUSH PRIVILEGES;

-- Verificar privilégios aplicados
SHOW GRANTS FOR 'mariadb_exporter'@'127.0.0.1';
```

---

## 6. Métodos de registro no PMM

### Método A — pmm-admin CLI (recomendado)

Exporter rodando no **mesmo host** que o pmm-client:

```bash
pmm-admin add external \
  --service-name=mariadb-$(hostname) \
  --listen-port=9104 \
  --metrics-path=/metrics \
  --scheme=http \
  --group=mariadb \
  --environment=production \
  --cluster=rech-cloud \
  --replication-set=mariadb-11-4
```

Saída esperada:
```
External Service added.
Service ID  : /service_id/xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
Service name: mariadb-rech-cloud-01
Group       : mariadb
```

---

### Método B — pmm-admin external-serverless

Exporter rodando em host **diferente** do pmm-client (ou sem pmm-client no host):

```bash
pmm-admin add external-serverless \
  --external-name=mariadb-central-rech \
  --host=192.168.1.50 \
  --listen-port=9104 \
  --metrics-path=/metrics \
  --scheme=http
```

---

### Método C — Interface web PMM

```
PMM UI → PMM Configuration
       → PMM Inventory
       → Add Service
       → External Service

Campos:
  Service name : mariadb-central-rech
  Host         : 127.0.0.1 (ou IP do host)
  Port         : 9104
  Metrics path : /metrics
  Group        : mariadb
  Environment  : production
  Cluster      : rech-cloud
```

---

## 7. Instalação nos 5 servidores MariaDB da Rech

Script de deploy completo para Oracle Linux 9:

```bash
#!/bin/bash
# deploy_mariadb_exporter.sh
# Executar como root em cada servidor MariaDB

set -euo pipefail

EXPORTER_VERSION="1.0.0"
EXPORTER_USER="mariadb_exporter"
EXPORTER_PORT="9104"
PMM_CLUSTER="rech-cloud"
PMM_ENV="production"
MARIADB_DSN="${MARIADB_DSN:-mariadb://mariadb_exporter:SENHA@tcp(localhost:3306)/}"

# 1. Criar usuário de sistema para o exporter
if ! id "$EXPORTER_USER" &>/dev/null; then
    useradd --system --no-create-home --shell /sbin/nologin "$EXPORTER_USER"
fi

# 2. Instalar binário
install -o root -g root -m 0755 \
    ./bin/mariadb_exporter \
    /usr/local/bin/mariadb_exporter

# 3. Criar diretório de configuração
mkdir -p /etc/mariadb_exporter
chmod 750 /etc/mariadb_exporter
chown root:"$EXPORTER_USER" /etc/mariadb_exporter

# 4. Criar arquivo de ambiente com DSN
cat > /etc/mariadb_exporter/mariadb_exporter.env <<EOF
MARIADB_DSN=${MARIADB_DSN}
EOF
chmod 640 /etc/mariadb_exporter/mariadb_exporter.env
chown root:"$EXPORTER_USER" /etc/mariadb_exporter/mariadb_exporter.env

# 5. Criar unit systemd
cat > /etc/systemd/system/mariadb_exporter.service <<EOF
[Unit]
Description=MariaDB Exporter for Prometheus / PMM
Documentation=https://github.com/rech-informatica/mariadb_exporter
After=network.target mariadb.service
Wants=mariadb.service

[Service]
Type=simple
User=${EXPORTER_USER}
Group=${EXPORTER_USER}
EnvironmentFile=/etc/mariadb_exporter/mariadb_exporter.env
ExecStart=/usr/local/bin/mariadb_exporter \\
  --web.listen-address=":${EXPORTER_PORT}" \\
  --web.telemetry-path="/metrics" \\
  --pmm.cluster="${PMM_CLUSTER}" \\
  --pmm.environment="${PMM_ENV}" \\
  --pmm.service-name="mariadb-\$(hostname -s)" \\
  --log.level=info \\
  --log.format=json
Restart=on-failure
RestartSec=5s
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ReadWritePaths=/var/log/mariadb_exporter

[Install]
WantedBy=multi-user.target
EOF

# 6. Ativar e iniciar
systemctl daemon-reload
systemctl enable mariadb_exporter
systemctl start mariadb_exporter

# 7. Verificar saúde
sleep 2
curl -sf http://localhost:${EXPORTER_PORT}/health && echo " → exporter saudavel"

# 8. Registrar no PMM
pmm-admin add external \
  --service-name="mariadb-$(hostname -s)" \
  --listen-port="${EXPORTER_PORT}" \
  --metrics-path="/metrics" \
  --scheme=http \
  --group=mariadb \
  --environment="${PMM_ENV}" \
  --cluster="${PMM_CLUSTER}"

echo "Deploy concluido em $(hostname)"
```

Uso:
```bash
# Em cada servidor:
MARIADB_DSN="mariadb://mariadb_exporter:SENHA@tcp(localhost:3306)/" \
  bash deploy_mariadb_exporter.sh
```

---

## 8. Verificação pós-registro

### 8.1 Verificar inventory do PMM

```bash
pmm-admin list
```

Saída esperada (uma linha por servidor):
```
Service type  Service name              Address and port  Service ID
External      mariadb-rech-cloud-01    127.0.0.1:9104    /service_id/...
External      mariadb-rech-cloud-02    127.0.0.1:9104    /service_id/...
External      mariadb-rech-cloud-03    127.0.0.1:9104    /service_id/...
External      mariadb-rech-cloud-04    127.0.0.1:9104    /service_id/...
External      mariadb-rech-cloud-05    127.0.0.1:9104    /service_id/...
```

### 8.2 Verificar métricas chegando

```bash
# No host do exporter
curl -s http://localhost:9104/metrics | grep -E "^mariadb_up|^mariadb_info"
```

Saída esperada:
```
mariadb_info{cluster="rech-cloud",environment="production",hostname="rech-cloud-01",service_name="mariadb-rech-cloud-01",version="11.4.3-MariaDB"} 1
mariadb_up{cluster="rech-cloud",environment="production",service_name="mariadb-rech-cloud-01"} 1
```

### 8.3 Verificar no Grafana do PMM

```
PMM UI → Dashboards → Advanced Data Exploration

Datasource : Prometheus
Metric     : mariadb_up
             mariadb_user_rows_read_total
             mariadb_query_response_time_seconds_bucket
             mariadb_disk_available_bytes

Filtrar por: service_name, cluster, environment
```

Todas as métricas do exporter devem aparecer com prefixo `mariadb_`.

### 8.4 Verificar scrape status no VictoriaMetrics

```
PMM UI → PMM Configuration → Diagnostics → Exporters Details

ou via URL direta:
https://<pmm-server>/prometheus/targets

Procurar por: job="external-<service-name>"
Status esperado: UP
```

---

## 9. Criação do dashboard Grafana no PMM

Após as métricas chegarem ao PMM, criar dashboard dedicado:

```
PMM UI → Dashboards → + New Dashboard → Add Panel

Para cada panel, usar:
  Datasource: Prometheus
  Query: <métrica do exporter>
```

### Queries de referência para os panels principais

```promql
# Disponibilidade das instâncias
mariadb_up{cluster="rech-cloud"}

# Latência média geral (em ms)
rate(mariadb_query_response_time_seconds_sum[5m])
  / rate(mariadb_query_response_time_seconds_count[5m]) * 1000

# Top 10 usuários por linhas lidas
topk(10, rate(mariadb_user_rows_read_total[5m]))

# Top 10 tabelas por linhas lidas
topk(10, rate(mariadb_table_rows_read_total[5m]))

# Índices sem uso (contagem por instância)
count by (service_name) (mariadb_index_unused == 1)

# Uso de disco por instância
mariadb_disk_used_bytes / mariadb_disk_total_bytes * 100

# Slaves com atraso de replicação > 30s
mariadb_slave_seconds_behind_master > 30

# MDL locks ativos
mariadb_metadata_locks_total > 0

# Percentil 99 de latência (requer histograma real)
histogram_quantile(0.99,
  rate(mariadb_query_response_time_seconds_bucket[5m])
)

# Queries críticas (> 1s) por minuto
rate(mariadb_query_response_time_seconds_bucket{
  le="1.0",
  cluster="rech-cloud"
}[1m])
```

### Exportar dashboard como JSON para versionamento

```
PMM UI → Dashboard → Share → Export → Save to file → mariadb_dashboard.json
```

Incluir o `mariadb_dashboard.json` no repositório do `mariadb_exporter` em:
```
mariadb_exporter/
└── dashboards/
    └── mariadb_overview.json
```

---

## 10. Alertas no PMM

Após as métricas estarem disponíveis, criar alertas via PMM Alerting:

```
PMM UI → Alerting → Alert Rules → + New Alert Rule

Datasource: Prometheus
```

### Regras de alerta sugeridas

```yaml
# mariadb_alerts.yml — importar via PMM Alerting

groups:
  - name: mariadb_availability
    rules:
      - alert: MariaDBDown
        expr: mariadb_up == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Instância MariaDB indisponível"
          description: "{{ $labels.service_name }} está offline há mais de 1 minuto."

      - alert: MariaDBHighLatency
        expr: |
          histogram_quantile(0.99,
            rate(mariadb_query_response_time_seconds_bucket[5m])
          ) > 1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Latência P99 acima de 1s"
          description: "{{ $labels.service_name }} com P99={{ $value }}s nos últimos 5 minutos."

      - alert: MariaDBReplicationLag
        expr: mariadb_slave_seconds_behind_master > 60
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Atraso de replicação alto"
          description: "{{ $labels.service_name }} com atraso de {{ $value }}s."

      - alert: MariaDBDiskAlmostFull
        expr: |
          (mariadb_disk_used_bytes / mariadb_disk_total_bytes) > 0.85
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Disco do MariaDB acima de 85%"
          description: "{{ $labels.service_name }} — path={{ $labels.path }} em {{ $value | humanizePercentage }}."

      - alert: MariaDBMetadataLockAccumulation
        expr: mariadb_metadata_locks_total > 10
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Muitos MDL locks ativos"
          description: "{{ $labels.service_name }} com {{ $value }} metadata locks ativos."
```

---

## 11. Remoção do serviço do PMM

Se precisar remover uma instância do inventory:

```bash
# Listar serviços registrados
pmm-admin list

# Remover pelo nome
pmm-admin remove external mariadb-rech-cloud-01

# Parar e desabilitar o exporter no host
systemctl stop mariadb_exporter
systemctl disable mariadb_exporter
```

---

## 12. Atualização do exporter sem downtime de métricas

```bash
# 1. Compilar nova versão
make build

# 2. Copiar para o host
scp bin/mariadb_exporter usuario@servidor:/usr/local/bin/mariadb_exporter.new

# 3. Substituir binário (systemd reinicia automaticamente pelo Restart=on-failure)
mv /usr/local/bin/mariadb_exporter.new /usr/local/bin/mariadb_exporter
systemctl restart mariadb_exporter

# 4. Verificar saúde após restart
sleep 2
curl -sf http://localhost:9104/health
pmm-admin list | grep mariadb
```

O PMM aguarda até 30s para o target voltar ao estado UP — não é necessário
remover e re-registrar o serviço para atualizações de binário.

---

## 13. Critérios de aceitação da integração PMM

- [ ] `pmm-admin add external` executa sem erros em todos os 5 servidores
- [ ] `pmm-admin list` mostra todos os 5 serviços com status `UP`
- [ ] `mariadb_up{cluster="rech-cloud"}` retorna 5 séries no Grafana
- [ ] Advanced Data Exploration mostra métricas com prefixo `mariadb_`
- [ ] Filtros por `cluster`, `environment` e `service_name` funcionam nos panels
- [ ] Histograma `mariadb_query_response_time_seconds_bucket` permite calcular P99 via `histogram_quantile`
- [ ] Alertas disparam corretamente ao simular banco offline (`systemctl stop mariadb`)
- [ ] Dashboard JSON exportado e versionado no repositório em `dashboards/`
- [ ] Atualização de binário não requer re-registro no PMM

---

*Documento complementar ao `mariadb_exporter_spec.md`.*
*Autor do projeto: Kevenny — Rech Informática.*
*Ambiente alvo: PMM 3.x + MariaDB 11.4 Community em Oracle Linux 9 / OCI.*
