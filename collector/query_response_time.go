package collector

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
)

// queryResponseTimeQuery lê a tabela do plugin query_response_time. Cada linha
// traz o limite superior do bucket (TIME), a contagem de queries naquele bucket
// (COUNT) e o tempo total acumulado (TOTAL).
const queryResponseTimeQuery = `
SELECT TIME, COUNT, TOTAL
FROM information_schema.QUERY_RESPONSE_TIME`

// tooLongMarker é o valor textual da última linha da tabela, que agrupa as
// queries acima do maior bucket configurado.
const tooLongMarker = "TOO LONG"

// QueryResponseTimeCollector expõe a distribuição de tempo de resposta como um
// histograma Prometheus nativo.
type QueryResponseTimeCollector struct {
	base
	desc *prometheus.Desc
}

// NewQueryResponseTimeCollector cria o coletor query_response_time.
func NewQueryResponseTimeCollector(enabled bool, logger log.Logger, features FeatureProvider) *QueryResponseTimeCollector {
	return &QueryResponseTimeCollector{
		base: newBase("query_response_time",
			"Histograma de tempo de resposta de queries de information_schema.QUERY_RESPONSE_TIME (requer o plugin query_response_time e query_response_time_stats=ON).",
			enabled, logger, features),
		desc: newDesc("", "query_response_time_seconds",
			"Distribuição do tempo de resposta das queries em segundos.", nil),
	}
}

// Available implementa Availability: depende do plugin query_response_time.
func (c *QueryResponseTimeCollector) Available() bool {
	return c.featureFlags().HasQueryResponseTime
}

// Collect implementa Collector.
//
// A tabela do MariaDB traz contagens por bucket, enquanto o formato de
// histograma do Prometheus exige contagens cumulativas (cada bucket "le" inclui
// os anteriores). A conversão é feita aqui.
func (c *QueryResponseTimeCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "plugin query_response_time inativo ou query_response_time_stats=OFF; nenhuma métrica será coletada")
		return nil
	}

	rows, err := db.QueryContext(ctx, queryResponseTimeQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	type bucket struct {
		upperBound float64
		count      uint64
	}

	var (
		buckets    []bucket
		totalCount uint64
		totalSum   float64
	)

	for rows.Next() {
		var timeRaw, countRaw, totalRaw sql.RawBytes

		if err := rows.Scan(&timeRaw, &countRaw, &totalRaw); err != nil {
			return err
		}

		timeStr := strings.TrimSpace(string(timeRaw))

		count, err := parseFloat(string(countRaw))
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "linha de QRT com COUNT inválido, ignorada", "time", timeStr, "err", err)
			continue
		}

		// TOTAL pode vir vazio em algumas versões; nesse caso a soma apenas não
		// é incrementada por esta linha.
		if total, err := parseFloat(string(totalRaw)); err == nil {
			totalSum += total
		}

		// A linha TOO LONG entra na contagem total mas não gera bucket: seu
		// "limite" é infinito e já é representado por _count no formato
		// Prometheus (seção 2.2, query_response_time).
		if strings.EqualFold(timeStr, tooLongMarker) {
			totalCount += uint64(count)
			continue
		}

		parsed, err := parseFloat(timeStr)
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "linha de QRT com TIME inválido, ignorada", "time", timeStr, "err", err)
			continue
		}

		upperBound, ok := sanitizeBound(parsed)
		if !ok {
			_ = level.Debug(c.Logger()).Log("msg", "linha de QRT com limite fora de faixa, ignorada", "time", timeStr)
			continue
		}

		totalCount += uint64(count)
		buckets = append(buckets, bucket{upperBound: upperBound, count: uint64(count)})
	}

	if err := rows.Err(); err != nil {
		return err
	}

	// Tabela vazia (plugin recém-habilitado, sem tráfego): sem métricas e sem
	// erro, conforme a seção 17.
	if len(buckets) == 0 && totalCount == 0 {
		return nil
	}

	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].upperBound < buckets[j].upperBound
	})

	// Acumula as contagens: o bucket "le=x" do Prometheus conta todas as
	// observações <= x, não apenas as do intervalo.
	cumulative := make(map[float64]uint64, len(buckets))
	var running uint64
	for _, b := range buckets {
		running += b.count
		cumulative[b.upperBound] = running
	}

	ch <- prometheus.MustNewConstHistogram(
		c.desc,
		totalCount,
		totalSum,
		cumulative,
	)

	return nil
}

// sanitizeBound protege contra limites não finitos vindos da tabela, que
// invalidariam o histograma.
func sanitizeBound(v float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}
