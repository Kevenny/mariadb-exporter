package collector

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"
)

// Usage é o tipo de uso de uma coluna no YAML de custom metrics.
const (
	usageLabel   = "LABEL"
	usageCounter = "COUNTER"
	usageGauge   = "GAUGE"
)

// columnSpec descreve como uma coluna do result set deve ser tratada.
type columnSpec struct {
	Usage       string `yaml:"usage"`
	Description string `yaml:"description"`
}

// metricSpec é a definição de uma métrica customizada no YAML.
//
// O formato segue a especificação (seção 10): a chave do mapa é o nome da
// métrica, `query` é o SQL e `metrics` é uma lista de mapas de coluna -> spec.
type metricSpec struct {
	Query   string                  `yaml:"query"`
	Metrics []map[string]columnSpec `yaml:"metrics"`
}

// customMetric é a forma compilada de um metricSpec, pronta para o scrape.
type customMetric struct {
	name       string
	query      string
	labelCols  []string
	valueCols  []string
	descs      map[string]*prometheus.Desc
	valueKinds map[string]prometheus.ValueType
}

// CustomMetricsCollector executa queries definidas pelo usuário em arquivos YAML.
type CustomMetricsCollector struct {
	base
	metrics []customMetric
}

// NewCustomMetricsCollector carrega os arquivos YAML informados e devolve o
// coletor. Um arquivo inválido é um erro de configuração e aborta o startup.
func NewCustomMetricsCollector(paths []string, logger log.Logger, features FeatureProvider) (*CustomMetricsCollector, error) {
	c := &CustomMetricsCollector{
		base: newBase("custom_metrics", "Métricas definidas pelo usuário em arquivos YAML.", len(paths) > 0, logger, features),
	}

	for _, path := range paths {
		metrics, err := loadCustomMetricsFile(path)
		if err != nil {
			return nil, fmt.Errorf("custom metrics %q: %w", path, err)
		}
		c.metrics = append(c.metrics, metrics...)
	}

	return c, nil
}

// loadCustomMetricsFile lê e compila um arquivo YAML de custom metrics.
func loadCustomMetricsFile(path string) ([]customMetric, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw map[string]metricSpec
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("YAML inválido: %w", err)
	}

	// Os nomes são ordenados para que a ordem de registro seja determinística
	// entre execuções, já que a iteração de mapa em Go é aleatória.
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]customMetric, 0, len(names))
	for _, name := range names {
		compiled, err := compileCustomMetric(name, raw[name])
		if err != nil {
			return nil, err
		}
		out = append(out, *compiled)
	}

	return out, nil
}

// metricNameRe e labelNameRe são as gramáticas de nome do Prometheus. Um nome
// fora delas faz o client_golang entrar em pânico ao construir a métrica, o que
// derrubaria o exporter em runtime — por isso a validação acontece no load, onde
// o erro é apenas uma falha de configuração no startup.
var (
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// compileCustomMetric valida o spec e pré-calcula os Descs.
func compileCustomMetric(name string, spec metricSpec) (*customMetric, error) {
	if strings.TrimSpace(spec.Query) == "" {
		return nil, fmt.Errorf("métrica %q: campo query é obrigatório", name)
	}
	if len(spec.Metrics) == 0 {
		return nil, fmt.Errorf("métrica %q: campo metrics é obrigatório", name)
	}
	if !metricNameRe.MatchString(name) {
		return nil, fmt.Errorf(
			"métrica %q: nome inválido para o Prometheus (use apenas letras, dígitos, _ e :, começando por letra, _ ou :)",
			name,
		)
	}

	cm := &customMetric{
		name:       name,
		query:      spec.Query,
		descs:      make(map[string]*prometheus.Desc),
		valueKinds: make(map[string]prometheus.ValueType),
	}

	// Primeiro passe: separa colunas de label das de valor, pois os Descs das
	// colunas de valor precisam conhecer todos os labels.
	type valueCol struct {
		column      string
		description string
		kind        prometheus.ValueType
	}
	var valueCols []valueCol

	// Uma coluna só pode ter um papel: repetida como LABEL geraria labels
	// duplicados no Desc (pânico no client_golang), e declarada como LABEL e
	// valor ao mesmo tempo seria ambígua.
	seen := make(map[string]string)

	for _, entry := range spec.Metrics {
		for column, colSpec := range entry {
			usage := strings.ToUpper(strings.TrimSpace(colSpec.Usage))

			if previous, dup := seen[column]; dup {
				return nil, fmt.Errorf(
					"métrica %q: coluna %q declarada mais de uma vez (como %s e %s)",
					name, column, previous, usage,
				)
			}
			seen[column] = usage

			switch usage {
			case usageLabel:
				if !labelNameRe.MatchString(column) {
					return nil, fmt.Errorf(
						"métrica %q: %q não é um nome de label válido para o Prometheus (use apenas letras, dígitos e _, começando por letra ou _)",
						name, column,
					)
				}
				cm.labelCols = append(cm.labelCols, column)
			case usageCounter:
				valueCols = append(valueCols, valueCol{column, colSpec.Description, prometheus.CounterValue})
			case usageGauge:
				valueCols = append(valueCols, valueCol{column, colSpec.Description, prometheus.GaugeValue})
			default:
				return nil, fmt.Errorf(
					"métrica %q, coluna %q: usage %q inválido (use LABEL, COUNTER ou GAUGE)",
					name, column, colSpec.Usage,
				)
			}
		}
	}

	if len(valueCols) == 0 {
		return nil, fmt.Errorf("métrica %q: é necessária ao menos uma coluna COUNTER ou GAUGE", name)
	}

	for _, vc := range valueCols {
		help := vc.description
		if help == "" {
			help = fmt.Sprintf("Custom metric %s (coluna %s).", name, vc.column)
		}

		// Com uma única coluna de valor, o nome da métrica é o nome declarado no
		// YAML; com várias, o nome da coluna é sufixado para evitar colisão.
		metricName := name
		if len(valueCols) > 1 {
			metricName = name + "_" + vc.column
			// O sufixo entra no nome final, então precisa manter o nome válido.
			if !metricNameRe.MatchString(metricName) {
				return nil, fmt.Errorf(
					"métrica %q: a coluna %q gera o nome inválido %q para o Prometheus",
					name, vc.column, metricName,
				)
			}
		}

		cm.valueCols = append(cm.valueCols, vc.column)
		cm.descs[vc.column] = prometheus.NewDesc(metricName, help, cm.labelCols, nil)
		cm.valueKinds[vc.column] = vc.kind
	}

	return cm, nil
}

// Collect implementa Collector. Um erro em uma query não impede a execução das
// demais; o primeiro erro é devolvido ao final.
func (c *CustomMetricsCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	var firstErr error

	for _, cm := range c.metrics {
		if err := c.collectOne(ctx, db, ch, cm); err != nil {
			_ = level.Error(c.Logger()).Log("msg", "custom metric falhou", "metrica", cm.name, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("custom metric %q: %w", cm.name, err)
			}
		}
	}

	return firstErr
}

func (c *CustomMetricsCollector) collectOne(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric, cm customMetric) error {
	rows, err := db.QueryContext(ctx, cm.query)
	if err != nil {
		return err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	// Índice de coluna por nome (case-insensitive) para localizar labels e
	// valores independentemente da ordem no SELECT.
	index := make(map[string]int, len(columns))
	for i, col := range columns {
		index[strings.ToLower(col)] = i
	}

	for _, want := range append(append([]string{}, cm.labelCols...), cm.valueCols...) {
		if _, ok := index[strings.ToLower(want)]; !ok {
			return fmt.Errorf("coluna %q declarada no YAML não existe no result set da query", want)
		}
	}

	for rows.Next() {
		values := make([]sql.RawBytes, len(columns))
		scanArgs := make([]interface{}, len(columns))
		for i := range values {
			scanArgs[i] = &values[i]
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return err
		}

		// Colunas de label podem conter qualquer coisa — inclusive blobs
		// binários, dependendo da query do operador. Sanitizar evita o pânico do
		// client_golang em labels que não são UTF-8 válido.
		labelValues := make([]string, 0, len(cm.labelCols))
		for _, col := range cm.labelCols {
			labelValues = append(labelValues, sanitizeLabel(string(values[index[strings.ToLower(col)]])))
		}

		for _, col := range cm.valueCols {
			raw := string(values[index[strings.ToLower(col)]])

			v, err := parseFloat(raw)
			if err != nil {
				_ = level.Debug(c.Logger()).Log(
					"msg", "valor de custom metric ignorado",
					"metrica", cm.name, "coluna", col, "valor", raw, "err", err,
				)
				continue
			}

			ch <- prometheus.MustNewConstMetric(
				cm.descs[col], cm.valueKinds[col], v, labelValues...,
			)
		}
	}

	return rows.Err()
}
