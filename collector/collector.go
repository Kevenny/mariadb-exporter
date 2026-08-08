// Package collector contém a interface Collector e a implementação de todos os
// coletores de métricas do MariaDB.
package collector

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// Namespace é o prefixo de todas as métricas expostas pelo exporter.
const Namespace = "mariadb"

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
	Major     int
	Minor     int
	Patch     int
	Full      string // string completa, ex: "11.4.3-MariaDB"
	Comment   string // valor de @@version_comment
	IsMariaDB bool
}

// AtLeast informa se a versão detectada é maior ou igual a major.minor.patch.
func (v *VersionInfo) AtLeast(major, minor, patch int) bool {
	if v == nil {
		return false
	}
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}

// String devolve a versão em formato major.minor.patch.
func (v *VersionInfo) String() string {
	if v == nil {
		return "desconhecida"
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
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

// FeatureProvider entrega a visão mais recente das features detectadas. O
// exporter atualiza essa visão periodicamente (seção 9 da especificação), por
// isso os coletores consultam através desta interface em vez de guardar uma
// cópia do struct.
type FeatureProvider interface {
	Features() *FeatureFlags
	Version() *VersionInfo
}

// Availability é implementada por coletores que dependem de um plugin ou
// variável de ambiente do servidor. O exporter usa o resultado para publicar
// mariadb_collector_available{collector="nome"}.
type Availability interface {
	// Available informa se as dependências do coletor estão satisfeitas.
	Available() bool
}

// warnOnce emite um aviso apenas na primeira ocorrência, evitando poluir o log a
// cada scrape quando um plugin está permanentemente ausente (seção 9, item 1).
type warnOnce struct {
	once   sync.Once
	logger log.Logger
}

func (w *warnOnce) warn(keyvals ...interface{}) {
	w.once.Do(func() {
		if w.logger == nil {
			return
		}
		_ = level.Warn(w.logger).Log(keyvals...)
	})
}

// base fornece a implementação comum de Name, Help e Enabled, além do logger e
// do acesso às features detectadas. Todos os coletores embutem este struct.
type base struct {
	name     string
	help     string
	enabled  bool
	logger   log.Logger
	features FeatureProvider
	warned   warnOnce
}

func newBase(name, help string, enabled bool, logger log.Logger, features FeatureProvider) base {
	scoped := log.With(logger, "collector", name)
	return base{
		name:     name,
		help:     help,
		enabled:  enabled,
		logger:   scoped,
		features: features,
		warned:   warnOnce{logger: scoped},
	}
}

func (b *base) Name() string       { return b.name }
func (b *base) Help() string       { return b.help }
func (b *base) Enabled() bool      { return b.enabled }
func (b *base) Logger() log.Logger { return b.logger }

// featureFlags devolve as features atuais, nunca nil, para simplificar o uso.
func (b *base) featureFlags() *FeatureFlags {
	if b.features == nil {
		return &FeatureFlags{}
	}
	if f := b.features.Features(); f != nil {
		return f
	}
	return &FeatureFlags{}
}

// newDesc é um atalho para prometheus.NewDesc com o namespace do exporter já
// aplicado. Se subsystem for vazio, o nome fica namespace_name.
func newDesc(subsystem, name, help string, labels []string) *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, subsystem, name),
		help,
		labels,
		nil,
	)
}

// parseFloat converte para float64 os diversos formatos que o driver MySQL pode
// devolver (nil, []byte, string, números). Valores não numéricos retornam erro.
func parseFloat(value interface{}) (float64, error) {
	switch v := value.(type) {
	case nil:
		return 0, fmt.Errorf("valor nulo")
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	case []byte:
		return parseFloatString(string(v))
	case string:
		return parseFloatString(v)
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("tipo não numérico %T", value)
	}
}

// parseFloatString interpreta strings numéricas e também os valores booleanos
// textuais que o MariaDB usa em variáveis e em SHOW STATUS.
func parseFloatString(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("string vazia")
	}
	switch strings.ToUpper(s) {
	case "ON", "YES", "TRUE", "ENABLED":
		return 1, nil
	case "OFF", "NO", "FALSE", "DISABLED", "NONE":
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("valor não numérico %q", s)
	}
	return f, nil
}

// Registry mantém a lista de coletores construídos e disponíveis ao exporter.
type Registry struct {
	collectors []Collector
}

// NewRegistry cria um registry vazio.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adiciona coletores ao registry, ignorando valores nil.
func (r *Registry) Register(collectors ...Collector) {
	for _, c := range collectors {
		if c != nil {
			r.collectors = append(r.collectors, c)
		}
	}
}

// All devolve todos os coletores registrados.
func (r *Registry) All() []Collector {
	out := make([]Collector, len(r.collectors))
	copy(out, r.collectors)
	return out
}

// Enabled devolve apenas os coletores habilitados por flag.
func (r *Registry) Enabled() []Collector {
	var out []Collector
	for _, c := range r.collectors {
		if c.Enabled() {
			out = append(out, c)
		}
	}
	return out
}

// Names devolve os nomes de todos os coletores registrados.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.collectors))
	for _, c := range r.collectors {
		out = append(out, c.Name())
	}
	return out
}
