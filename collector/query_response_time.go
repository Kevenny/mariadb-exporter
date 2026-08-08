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

// queryResponseTimeQuery reads the query_response_time plugin's table. Each
// row carries the bucket's upper bound (TIME), the query count in that bucket
// (COUNT) and the accumulated total time (TOTAL).
const queryResponseTimeQuery = `
SELECT TIME, COUNT, TOTAL
FROM information_schema.QUERY_RESPONSE_TIME`

// tooLongMarker is the textual value of the table's last row, which groups
// queries above the largest configured bucket.
const tooLongMarker = "TOO LONG"

// QueryResponseTimeCollector exposes the response time distribution as a
// native Prometheus histogram.
type QueryResponseTimeCollector struct {
	base
	desc *prometheus.Desc
}

// NewQueryResponseTimeCollector creates the query_response_time collector.
func NewQueryResponseTimeCollector(enabled bool, logger log.Logger, features FeatureProvider) *QueryResponseTimeCollector {
	return &QueryResponseTimeCollector{
		base: newBase("query_response_time",
			"Query response time histogram from information_schema.QUERY_RESPONSE_TIME (requires the query_response_time plugin and query_response_time_stats=ON).",
			enabled, logger, features),
		desc: newDesc("", "query_response_time_seconds",
			"Distribution of query response time in seconds.", nil),
	}
}

// Available implements Availability: depends on the query_response_time
// plugin.
func (c *QueryResponseTimeCollector) Available() bool {
	return c.featureFlags().HasQueryResponseTime
}

// Collect implements Collector.
//
// MariaDB's table carries per-bucket counts, while Prometheus's histogram
// format requires cumulative counts (each "le" bucket includes the previous
// ones). The conversion is done here.
func (c *QueryResponseTimeCollector) Collect(ctx context.Context, db *sql.DB, ch chan<- prometheus.Metric) error {
	if !c.Available() {
		c.warned.warn("msg", "query_response_time plugin inactive or query_response_time_stats=OFF; no metrics will be collected")
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

		parsedCount, err := parseFloat(string(countRaw))
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "QRT row with invalid COUNT, ignored", "time", timeStr, "err", err)
			continue
		}

		// The conversion to uint64 is done here, once, and with validation: a
		// negative or non-finite value would turn into an astronomical number
		// via two's-complement wraparound, inflating the histogram counts and
		// breaking any rate() computed on top of them.
		count, ok := toCount(parsedCount)
		if !ok {
			_ = level.Debug(c.Logger()).Log("msg", "QRT row with out-of-range COUNT, ignored", "time", timeStr, "count", parsedCount)
			continue
		}

		// TOTAL may come empty on some versions; in that case the sum simply
		// isn't incremented by this row. Non-finite values are discarded: a
		// NaN in _sum contaminates the whole sum and never leaves it.
		if total, err := parseFloat(string(totalRaw)); err == nil {
			if !math.IsNaN(total) && !math.IsInf(total, 0) && total >= 0 {
				totalSum += total
			}
		}

		// The TOO LONG row counts toward the total but does not generate a
		// bucket: its "bound" is infinite and is already represented by
		// _count in the Prometheus format (section 2.2, query_response_time).
		if strings.EqualFold(timeStr, tooLongMarker) {
			totalCount += count
			continue
		}

		parsed, err := parseFloat(timeStr)
		if err != nil {
			_ = level.Debug(c.Logger()).Log("msg", "QRT row with invalid TIME, ignored", "time", timeStr, "err", err)
			continue
		}

		upperBound, ok := sanitizeBound(parsed)
		if !ok {
			_ = level.Debug(c.Logger()).Log("msg", "QRT row with out-of-range bound, ignored", "time", timeStr)
			continue
		}

		totalCount += count
		buckets = append(buckets, bucket{upperBound: upperBound, count: count})
	}

	if err := rows.Err(); err != nil {
		return err
	}

	// Empty table (plugin just enabled, no traffic yet): no metrics and no
	// error, per section 17.
	if len(buckets) == 0 && totalCount == 0 {
		return nil
	}

	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].upperBound < buckets[j].upperBound
	})

	// Accumulates the counts: Prometheus's "le=x" bucket counts all
	// observations <= x, not just the ones in the interval.
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

// sanitizeBound protects against non-finite bounds coming from the table,
// which would invalidate the histogram.
func sanitizeBound(v float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	return v, true
}

// toCount safely converts a COUNT read from the table to uint64.
//
// A direct uint64(v) conversion in Go has undefined results for negative or
// out-of-range values: in practice, -5 becomes 18446744073709551611. Since
// this number feeds into _count and the histogram buckets, a single bad value
// would corrupt every rate query built on top of the metric.
func toCount(v float64) (uint64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > math.MaxUint64 {
		return 0, false
	}
	return uint64(v), true
}
