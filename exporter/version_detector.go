// Package exporter contém o orquestrador dos coletores e a detecção de versão e
// de plugins do MariaDB.
package exporter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/Kevenny/mariadb-exporter/collector"
)

// FeatureRefreshInterval é o intervalo do refresh assíncrono de plugins
// (seção 9 da especificação).
const FeatureRefreshInterval = 5 * time.Minute

// ErrNotMariaDB é devolvido quando a instância conectada não é um MariaDB.
var ErrNotMariaDB = errors.New("instância não é MariaDB")

// versionRe extrai major.minor.patch do começo da string de VERSION().
// Cobre formatos como "11.4.3-MariaDB", "10.11.6-MariaDB-1:10.11.6+maria~ubu2204"
// e "5.5.5-10.6.12-MariaDB" (prefixo de compatibilidade, tratado antes).
var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// mysqlCompatPrefix é o prefixo que o MariaDB anuncia para clientes antigos
// quando a variável version tem o replicate-annotate de compatibilidade. Nesses
// casos a versão real vem depois do prefixo.
const mysqlCompatPrefix = "5.5.5-"

// DetectVersion executa SELECT VERSION() e @@version_comment, confirma que a
// instância é MariaDB e extrai major.minor.patch.
//
// Conforme a seção 2.1 da especificação, uma instância que não seja MariaDB é
// um erro fatal de startup: devolve ErrNotMariaDB encapsulado numa mensagem
// descritiva.
func DetectVersion(ctx context.Context, db *sql.DB) (*collector.VersionInfo, error) {
	var full, comment string

	// version_comment é opcional: em alguns forks/proxies ele não existe, e a
	// ausência dele não deve impedir a detecção.
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&full); err != nil {
		return nil, fmt.Errorf("falha ao executar SELECT VERSION(): %w", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT @@global.version_comment").Scan(&comment); err != nil {
		comment = ""
	}

	info := ParseVersion(full, comment)
	if !info.IsMariaDB {
		return info, fmt.Errorf(
			"%w: VERSION() retornou %q (version_comment: %q). "+
				"O mariadb_exporter coleta métricas exclusivas do MariaDB "+
				"(USER_STATISTICS, QUERY_RESPONSE_TIME, METADATA_LOCK_INFO, DISKS, SHOW ALL SLAVES STATUS) "+
				"e não funciona contra MySQL. Para MySQL use o mysqld_exporter",
			ErrNotMariaDB, full, comment,
		)
	}

	return info, nil
}

// ParseVersion interpreta a string de VERSION() sem precisar de conexão, o que
// facilita os testes.
func ParseVersion(full, comment string) *collector.VersionInfo {
	info := &collector.VersionInfo{
		Full:    full,
		Comment: comment,
	}

	haystack := strings.ToLower(full + " " + comment)
	info.IsMariaDB = strings.Contains(haystack, "mariadb")

	numeric := strings.TrimSpace(full)
	// O MariaDB pode prefixar a versão com "5.5.5-" para clientes legados; a
	// versão verdadeira vem depois desse prefixo.
	if strings.HasPrefix(numeric, mysqlCompatPrefix) {
		numeric = strings.TrimPrefix(numeric, mysqlCompatPrefix)
	}

	if m := versionRe.FindStringSubmatch(numeric); m != nil {
		info.Major, _ = strconv.Atoi(m[1])
		info.Minor, _ = strconv.Atoi(m[2])
		info.Patch, _ = strconv.Atoi(m[3])
	}

	return info
}

// FeatureDetector detecta e mantém atualizadas as feature flags da instância.
// Implementa collector.FeatureProvider e é seguro para uso concorrente.
type FeatureDetector struct {
	db     *sql.DB
	logger log.Logger

	mu       sync.RWMutex
	version  *collector.VersionInfo
	features *collector.FeatureFlags
}

// NewFeatureDetector cria o detector com a versão já conhecida.
func NewFeatureDetector(db *sql.DB, version *collector.VersionInfo, logger log.Logger) *FeatureDetector {
	return &FeatureDetector{
		db:       db,
		logger:   logger,
		version:  version,
		features: &collector.FeatureFlags{},
	}
}

// Version devolve a versão detectada.
func (d *FeatureDetector) Version() *collector.VersionInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.version
}

// Features devolve uma cópia das feature flags atuais.
func (d *FeatureDetector) Features() *collector.FeatureFlags {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.features == nil {
		return &collector.FeatureFlags{}
	}
	copied := *d.features
	return &copied
}

// pluginQuery consulta os plugins relevantes ao exporter (seção 9).
const pluginQuery = `
SELECT plugin_name, plugin_status
FROM information_schema.plugins
WHERE plugin_name IN (
    'QUERY_RESPONSE_TIME',
    'QUERY_RESPONSE_TIME_AUDIT',
    'METADATA_LOCK_INFO',
    'DISKS'
)`

const userStatQuery = `
SELECT VARIABLE_VALUE
FROM information_schema.GLOBAL_VARIABLES
WHERE VARIABLE_NAME = 'userstat'`

// Refresh reconsulta plugins e variáveis, atualizando as feature flags.
//
// Erros individuais são logados mas não abortam o refresh: uma permissão
// faltando para uma das consultas não deve zerar as demais features.
func (d *FeatureDetector) Refresh(ctx context.Context) {
	features := &collector.FeatureFlags{}

	if err := d.detectPlugins(ctx, features); err != nil {
		_ = level.Warn(d.logger).Log("msg", "falha ao detectar plugins", "err", err)
	}
	if err := d.detectUserStat(ctx, features); err != nil {
		_ = level.Warn(d.logger).Log("msg", "falha ao detectar userstat", "err", err)
	}
	if err := d.detectGalera(ctx, features); err != nil {
		_ = level.Warn(d.logger).Log("msg", "falha ao detectar wsrep/galera", "err", err)
	}
	if err := d.detectReplica(ctx, features); err != nil {
		_ = level.Warn(d.logger).Log("msg", "falha ao detectar estado de réplica", "err", err)
	}

	d.mu.Lock()
	d.features = features
	d.mu.Unlock()

	_ = level.Debug(d.logger).Log(
		"msg", "feature flags atualizadas",
		"userstat", features.HasUserStat,
		"query_response_time", features.HasQueryResponseTime,
		"metadata_lock_info", features.HasMetadataLockInfo,
		"disks", features.HasDisksPlugin,
		"galera", features.HasGalera,
		"replica", features.IsReplica,
	)
}

func (d *FeatureDetector) detectPlugins(ctx context.Context, features *collector.FeatureFlags) error {
	rows, err := d.db.QueryContext(ctx, pluginQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var name, status string
		if err := rows.Scan(&name, &status); err != nil {
			return err
		}
		if !strings.EqualFold(status, "ACTIVE") {
			continue
		}
		switch strings.ToUpper(name) {
		case "QUERY_RESPONSE_TIME", "QUERY_RESPONSE_TIME_AUDIT":
			features.HasQueryResponseTime = true
		case "METADATA_LOCK_INFO":
			features.HasMetadataLockInfo = true
		case "DISKS":
			features.HasDisksPlugin = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// O plugin instalado só entrega dados se a coleta estiver ligada; sem isso a
	// tabela QUERY_RESPONSE_TIME existe mas fica vazia.
	if features.HasQueryResponseTime {
		var value string
		err := d.db.QueryRowContext(ctx, "SELECT @@global.query_response_time_stats").Scan(&value)
		switch {
		case err != nil:
			// Variável ausente: mantém o plugin como disponível e deixa o
			// coletor decidir pelo conteúdo da tabela.
			_ = level.Debug(d.logger).Log("msg", "query_response_time_stats indisponível", "err", err)
		case !isTruthy(value):
			features.HasQueryResponseTime = false
		}
	}

	// A versão mínima do METADATA_LOCK_INFO é 10.0.7 (seção 2.2).
	if features.HasMetadataLockInfo {
		if v := d.Version(); v != nil && !v.AtLeast(10, 0, 7) {
			_ = level.Warn(d.logger).Log(
				"msg", "METADATA_LOCK_INFO requer MariaDB >= 10.0.7; coletor será desabilitado",
				"versao", v.String(),
			)
			features.HasMetadataLockInfo = false
		}
	}

	return nil
}

func (d *FeatureDetector) detectUserStat(ctx context.Context, features *collector.FeatureFlags) error {
	var value string
	if err := d.db.QueryRowContext(ctx, userStatQuery).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	features.HasUserStat = isTruthy(value)
	return nil
}

func (d *FeatureDetector) detectGalera(ctx context.Context, features *collector.FeatureFlags) error {
	var name, value string
	err := d.db.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE 'wsrep_ready'").Scan(&name, &value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	features.HasGalera = isTruthy(value)
	return nil
}

// detectReplica verifica se a instância replica de algum master. Usa
// gtid_slave_pos como sinal primário por ser barato e não exigir privilégio de
// REPLICATION CLIENT.
func (d *FeatureDetector) detectReplica(ctx context.Context, features *collector.FeatureFlags) error {
	var value string
	err := d.db.QueryRowContext(ctx, "SELECT @@global.gtid_slave_pos").Scan(&value)
	if err == nil && strings.TrimSpace(value) != "" {
		features.IsReplica = true
		return nil
	}

	// Fallback: conta as linhas de SHOW ALL SLAVES STATUS. Ausência de linhas
	// significa que a instância não é réplica.
	rows, qErr := d.db.QueryContext(ctx, "SHOW ALL SLAVES STATUS")
	if qErr != nil {
		if err != nil {
			return err
		}
		return nil
	}
	defer rows.Close()
	features.IsReplica = rows.Next()
	return rows.Err()
}

// Run dispara um refresh imediato e depois a cada FeatureRefreshInterval até o
// contexto ser cancelado.
func (d *FeatureDetector) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = FeatureRefreshInterval
	}

	d.Refresh(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Refresh(ctx)
		}
	}
}

// isTruthy interpreta os valores booleanos textuais usados pelo MariaDB.
func isTruthy(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "ON", "1", "YES", "TRUE", "ALL", "ACTIVE":
		return true
	default:
		return false
	}
}
