#!/bin/bash
# deploy_mariadb_exporter.sh
#
# Instala e registra o mariadb_exporter em um servidor MariaDB como serviço
# systemd, e o registra no PMM como External Service (ver
# mariadb_exporter_pmm_integration.md, seções 6 e 7).
#
# Executar como root em cada servidor MariaDB, a partir do diretório raiz do
# repositório (espera encontrar ./bin/mariadb_exporter já compilado via
# `make build`).
#
# Uso:
#   MARIADB_DSN="mariadb://mariadb_exporter:SENHA@tcp(localhost:3306)/" \
#     ./packaging/deploy_mariadb_exporter.sh
#
# Nota de segurança: o DSN chega por variável de ambiente e fica visível em
# /proc/<pid>/environ enquanto o script roda, além de entrar no histórico do
# shell. Em ambientes compartilhados, prefira exportar a variável de um arquivo
# com permissão restrita (`set -a; . /root/.mariadb_dsn; set +a`) ou usar um
# gerenciador de segredos, em vez de digitar a senha na linha de comando.
#
# Variáveis de ambiente aceitas (todas opcionais exceto MARIADB_DSN):
#   MARIADB_DSN        DSN de conexão do exporter (obrigatória)
#   EXPORTER_VERSION    Versão exibida em logs (default: dev)
#   EXPORTER_USER       Usuário de sistema do serviço (default: mariadb_exporter)
#   EXPORTER_PORT       Porta de escuta (default: 9104)
#   PMM_CLUSTER         --pmm.cluster (default: vazio — sem agrupamento)
#   PMM_ENV             --pmm.environment (default: production)
#   PMM_REPLICATION_SET --pmm.replication-set (default: vazio)
#   WEB_CONFIG_FILE     --web.config.file para TLS/basic auth (default: vazio =
#                       HTTP sem autenticação). Ver packaging/web-config.yml.example

set -euo pipefail

EXPORTER_VERSION="${EXPORTER_VERSION:-dev}"
EXPORTER_USER="${EXPORTER_USER:-mariadb_exporter}"
EXPORTER_PORT="${EXPORTER_PORT:-9104}"
PMM_CLUSTER="${PMM_CLUSTER:-}"
PMM_ENV="${PMM_ENV:-production}"
PMM_REPLICATION_SET="${PMM_REPLICATION_SET:-}"
WEB_CONFIG_FILE="${WEB_CONFIG_FILE:-}"
MARIADB_DSN="${MARIADB_DSN:?defina MARIADB_DSN antes de executar este script}"

SERVICE_NAME="mariadb-$(hostname -s)"

if [[ $EUID -ne 0 ]]; then
    echo "este script precisa rodar como root" >&2
    exit 1
fi

if [[ ! -x ./bin/mariadb_exporter ]]; then
    echo "binário ./bin/mariadb_exporter não encontrado; rode 'make build' antes" >&2
    exit 1
fi

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

# 4. Criar arquivo de ambiente com o DSN.
#
# O arquivo contém a senha do usuário de monitoramento, então é criado já com a
# permissão restrita: um `cat >` seguido de chmod deixaria uma janela em que o
# arquivo fica legível conforme o umask (tipicamente 644), tempo suficiente para
# outro processo local ler a credencial.
ENV_FILE=/etc/mariadb_exporter/mariadb_exporter.env
install -o root -g "$EXPORTER_USER" -m 0640 /dev/null "$ENV_FILE"
cat > "$ENV_FILE" <<EOF
MARIADB_DSN=${MARIADB_DSN}
EOF

# 5. Montar os argumentos de PMM condicionalmente — cluster e replication-set
# são opcionais e não devem virar flags vazias no ExecStart.
PMM_ARGS="--pmm.environment=\"${PMM_ENV}\" --pmm.service-name=\"${SERVICE_NAME}\""
if [[ -n "$PMM_CLUSTER" ]]; then
    PMM_ARGS="${PMM_ARGS} --pmm.cluster=\"${PMM_CLUSTER}\""
fi
if [[ -n "$PMM_REPLICATION_SET" ]]; then
    PMM_ARGS="${PMM_ARGS} --pmm.replication-set=\"${PMM_REPLICATION_SET}\""
fi

# TLS/basic auth são opt-in via WEB_CONFIG_FILE. Quando ativos, o exporter passa
# a servir HTTPS, então tanto o health check local quanto o registro no PMM
# precisam usar o scheme correto.
WEB_ARGS=""
SCHEME="http"
CURL_TLS_OPT=""
if [[ -n "$WEB_CONFIG_FILE" ]]; then
    if [[ ! -r "$WEB_CONFIG_FILE" ]]; then
        echo "WEB_CONFIG_FILE=$WEB_CONFIG_FILE não existe ou não é legível" >&2
        exit 1
    fi
    WEB_ARGS="--web.config.file=\"${WEB_CONFIG_FILE}\""
    # Só assume HTTPS se o arquivo realmente configurar TLS: o mesmo arquivo pode
    # habilitar apenas basic auth, mantendo HTTP.
    if grep -qE "^[[:space:]]*tls_server_config:" "$WEB_CONFIG_FILE"; then
        SCHEME="https"
        # O certificado pode ser autoassinado; a verificação fica a cargo de quem
        # faz o scrape, não deste health check local.
        CURL_TLS_OPT="-k"
    fi
fi

# 6. Criar unit systemd
cat > /etc/systemd/system/mariadb_exporter.service <<EOF
[Unit]
Description=MariaDB Exporter for Prometheus / PMM
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
  ${WEB_ARGS} \\
  ${PMM_ARGS} \\
  --log.level=info \\
  --log.format=json
Restart=on-failure
RestartSec=5s
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict

[Install]
WantedBy=multi-user.target
EOF

# 7. Ativar e iniciar
systemctl daemon-reload
systemctl enable mariadb_exporter
systemctl restart mariadb_exporter

# 8. Verificar saúde antes de registrar no PMM — evita um "Connection check
# failed" no pmm-admin quando o exporter ainda não subiu.
#
# Com basic auth habilitado, o /health também exige credencial (o toolkit não
# permite excluir paths), então um 401 aqui indica que o exporter está no ar e
# respondendo — o que é suficiente para seguir com o registro.
sleep 2
HEALTH_CODE=$(curl -s ${CURL_TLS_OPT} -o /dev/null -w "%{http_code}" \
    "${SCHEME}://localhost:${EXPORTER_PORT}/health" || echo "000")
case "$HEALTH_CODE" in
    200)
        echo "exporter saudável em ${SCHEME}://:${EXPORTER_PORT}"
        ;;
    401)
        echo "exporter no ar em ${SCHEME}://:${EXPORTER_PORT} (401 — basic auth ativo, esperado)"
        ;;
    *)
        echo "exporter não respondeu em /health (HTTP ${HEALTH_CODE}); abortando registro no PMM" >&2
        systemctl status mariadb_exporter --no-pager || true
        exit 1
        ;;
esac

# 9. Registrar no PMM (idempotente: se o serviço já existir, o pmm-admin avisa
# e o script não deve falhar por isso).
PMM_REGISTER_ARGS=(
    --service-name="${SERVICE_NAME}"
    --listen-port="${EXPORTER_PORT}"
    --metrics-path="/metrics"
    --scheme="${SCHEME}"
    --group=mariadb
    --environment="${PMM_ENV}"
)
[[ -n "$PMM_CLUSTER" ]] && PMM_REGISTER_ARGS+=(--cluster="${PMM_CLUSTER}")
[[ -n "$PMM_REPLICATION_SET" ]] && PMM_REGISTER_ARGS+=(--replication-set="${PMM_REPLICATION_SET}")

if command -v pmm-admin &>/dev/null; then
    pmm-admin add external "${PMM_REGISTER_ARGS[@]}" || \
        echo "aviso: pmm-admin add external falhou (talvez o serviço já exista) — verifique com 'pmm-admin list'" >&2

    if [[ -n "$WEB_CONFIG_FILE" ]] && grep -qE "^[[:space:]]*basic_auth_users:" "$WEB_CONFIG_FILE"; then
        echo "ATENÇÃO: basic auth está ativo, mas 'pmm-admin add external' não aceita credenciais." >&2
        echo "         Configure usuário e senha no serviço pelo PMM UI (Inventory > o serviço)," >&2
        echo "         senão o scrape falhará com 401." >&2
    fi
else
    echo "aviso: pmm-admin não encontrado neste host; pulei o registro no PMM" >&2
fi

echo "deploy concluído em $(hostname) — versão ${EXPORTER_VERSION}, serviço ${SERVICE_NAME}"
