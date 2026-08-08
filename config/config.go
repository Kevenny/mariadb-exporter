// Package config concentra o parsing de flags de linha de comando e variáveis
// de ambiente do mariadb_exporter, além da normalização do DSN.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
)

// Web agrupa as configurações do servidor HTTP.
type Web struct {
	ListenAddress string
	TelemetryPath string
	MaxRequests   int
}

// DataSource agrupa as configurações de conexão com o MariaDB.
type DataSource struct {
	Name    string
	MaxOpen int
	MaxIdle int
	Timeout time.Duration
}

// Collectors agrupa os toggles e limites de cada coletor.
type Collectors struct {
	UserStat          bool
	TableStat         bool
	TableStatLimit    int
	IndexStat         bool
	IndexStatLimit    int
	ClientStat        bool
	QueryResponseTime bool
	MetadataLocks     bool
	Disks             bool
	Replication       bool
	Galera            bool
	InnoDB            bool
	GlobalStatus      bool
	GlobalVariables   bool
}

// Log agrupa as configurações de logging.
type Log struct {
	Level  string
	Format string
}

// Config é a configuração completa do exporter.
type Config struct {
	Web           Web
	DataSource    DataSource
	Collectors    Collectors
	Log           Log
	CustomMetrics []string
}

// envDefault retorna o valor da primeira variável de ambiente definida entre as
// informadas; caso nenhuma esteja definida, retorna fallback. Serve para dar às
// flags um default proveniente do ambiente, conforme a seção 6 da especificação.
func envDefault(fallback string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return fallback
}

// Register declara todas as flags no app kingpin informado e devolve o Config
// que será populado quando app.Parse for chamado.
func Register(app *kingpin.Application) *Config {
	cfg := &Config{}

	app.Flag("web.listen-address", "Endereço e porta onde o exporter escuta.").
		Default(envDefault(":9104", "MARIADB_WEB_LISTEN_ADDRESS")).
		StringVar(&cfg.Web.ListenAddress)
	app.Flag("web.telemetry-path", "Path sob o qual as métricas são expostas.").
		Default(envDefault("/metrics", "MARIADB_WEB_TELEMETRY_PATH")).
		StringVar(&cfg.Web.TelemetryPath)
	app.Flag("web.max-requests", "Máximo de scrapes simultâneos (0 = ilimitado).").
		Default(envDefault("0", "MARIADB_WEB_MAX_REQUESTS")).
		IntVar(&cfg.Web.MaxRequests)

	app.Flag("datasource.name", "DSN de conexão com o MariaDB (default: env MARIADB_DSN).").
		Default(envDefault("", "MARIADB_DSN", "MARIADB_DATASOURCE_NAME")).
		StringVar(&cfg.DataSource.Name)
	app.Flag("datasource.max-open", "Máximo de conexões abertas no pool.").
		Default(envDefault("3", "MARIADB_DATASOURCE_MAX_OPEN")).
		IntVar(&cfg.DataSource.MaxOpen)
	app.Flag("datasource.max-idle", "Máximo de conexões idle no pool.").
		Default(envDefault("3", "MARIADB_DATASOURCE_MAX_IDLE")).
		IntVar(&cfg.DataSource.MaxIdle)
	app.Flag("datasource.timeout", "Timeout de query em segundos.").
		Default(envDefault("30s", "MARIADB_DATASOURCE_TIMEOUT")).
		DurationVar(&cfg.DataSource.Timeout)

	app.Flag("collector.userstat", "Habilita o coletor userstat.").
		Default("true").BoolVar(&cfg.Collectors.UserStat)
	app.Flag("collector.tablestat", "Habilita o coletor tablestat.").
		Default("true").BoolVar(&cfg.Collectors.TableStat)
	app.Flag("collector.tablestat.limit", "Limite de tabelas por scrape.").
		Default("500").IntVar(&cfg.Collectors.TableStatLimit)
	app.Flag("collector.indexstat", "Habilita o coletor indexstat.").
		Default("true").BoolVar(&cfg.Collectors.IndexStat)
	app.Flag("collector.indexstat.limit", "Limite de índices por scrape.").
		Default("1000").IntVar(&cfg.Collectors.IndexStatLimit)
	app.Flag("collector.clientstat", "Habilita o coletor clientstat.").
		Default("true").BoolVar(&cfg.Collectors.ClientStat)
	app.Flag("collector.query_response_time", "Habilita o coletor query_response_time.").
		Default("true").BoolVar(&cfg.Collectors.QueryResponseTime)
	app.Flag("collector.metadata_locks", "Habilita o coletor metadata_locks.").
		Default("true").BoolVar(&cfg.Collectors.MetadataLocks)
	app.Flag("collector.disks", "Habilita o coletor disks.").
		Default("true").BoolVar(&cfg.Collectors.Disks)
	app.Flag("collector.replication", "Habilita o coletor replication.").
		Default("true").BoolVar(&cfg.Collectors.Replication)
	app.Flag("collector.galera", "Habilita o coletor galera (opt-in).").
		Default("false").BoolVar(&cfg.Collectors.Galera)
	app.Flag("collector.innodb", "Habilita o coletor innodb.").
		Default("true").BoolVar(&cfg.Collectors.InnoDB)
	app.Flag("collector.global_status", "Habilita o coletor global_status.").
		Default("true").BoolVar(&cfg.Collectors.GlobalStatus)
	app.Flag("collector.global_variables", "Habilita o coletor global_variables.").
		Default("true").BoolVar(&cfg.Collectors.GlobalVariables)

	app.Flag("custom-metrics", "Arquivo YAML de custom metrics (repetível).").
		PlaceHolder("ARQUIVO").StringsVar(&cfg.CustomMetrics)

	app.Flag("log.level", "Log level: debug, info, warn, error.").
		Default(envDefault("info", "MARIADB_LOG_LEVEL")).
		EnumVar(&cfg.Log.Level, "debug", "info", "warn", "error")
	app.Flag("log.format", "Formato do log: text, json.").
		Default(envDefault("text", "MARIADB_LOG_FORMAT")).
		EnumVar(&cfg.Log.Format, "text", "json")

	return cfg
}

// Validate confere se a configuração mínima está presente.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.DataSource.Name) == "" {
		return fmt.Errorf("DSN não informado: use --datasource.name ou a variável de ambiente MARIADB_DSN")
	}
	if _, err := NormalizeDSN(c.DataSource.Name); err != nil {
		return err
	}
	if c.Collectors.TableStatLimit < 0 {
		return fmt.Errorf("--collector.tablestat.limit não pode ser negativo")
	}
	if c.Collectors.IndexStatLimit < 0 {
		return fmt.Errorf("--collector.indexstat.limit não pode ser negativo")
	}
	return nil
}

// NormalizeDSN converte o DSN informado pelo usuário para o formato aceito pelo
// driver go-sql-driver/mysql.
//
// A especificação usa o prefixo `mariadb://` por clareza semântica, mas o driver
// fala o protocolo MySQL e não entende esquemas de URL. Esta função remove o
// prefixo (`mariadb://` ou `mysql://`) e devolve o DSN nativo, por exemplo:
//
//	mariadb://pmm:senha@tcp(localhost:3306)/  ->  pmm:senha@tcp(localhost:3306)/
//
// DSNs já no formato nativo do driver são devolvidos inalterados.
func NormalizeDSN(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", fmt.Errorf("DSN vazio")
	}

	for _, scheme := range []string{"mariadb://", "mysql://"} {
		if strings.HasPrefix(strings.ToLower(dsn), scheme) {
			dsn = dsn[len(scheme):]
			break
		}
	}

	// O driver exige a barra que separa endereço e nome do banco. Sem ela, um
	// DSN como "user:senha@tcp(host:3306)" é rejeitado no Open.
	if !strings.Contains(dsn, "/") {
		dsn += "/"
	}

	return dsn, nil
}

// RedactDSN remove a senha do DSN para que ele possa aparecer em logs.
func RedactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	head, tail := dsn[:at], dsn[at:]

	// O prefixo de esquema é preservado à parte: sem isso, o ':' de
	// "mariadb://" seria confundido com o separador da senha em DSNs sem
	// credenciais, corrompendo a URL.
	prefix := ""
	if idx := strings.Index(head, "://"); idx >= 0 {
		prefix, head = head[:idx+3], head[idx+3:]
	}

	colon := strings.LastIndex(head, ":")
	if colon < 0 {
		return dsn
	}
	// Evita mascarar a porta de um DSN sem credenciais, ex: "tcp(host:3306)".
	if strings.ContainsAny(head[colon:], "()") {
		return dsn
	}
	return prefix + head[:colon] + ":***" + tail
}

// dsnQueryParams extrai os parâmetros de query do DSN, se houver. Usado apenas
// em testes e diagnósticos.
func dsnQueryParams(dsn string) (url.Values, error) {
	idx := strings.Index(dsn, "?")
	if idx < 0 {
		return url.Values{}, nil
	}
	return url.ParseQuery(dsn[idx+1:])
}
