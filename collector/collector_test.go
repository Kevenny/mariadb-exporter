package collector

import (
	"context"
	"database/sql"
	"io"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// testLogger returns a silent logger for the tests.
func testLogger() log.Logger {
	return log.NewNopLogger()
}

// staticFeatures implements FeatureProvider with fixed values, allowing the
// available and unavailable plugin paths to be tested.
type staticFeatures struct {
	features *FeatureFlags
	version  *VersionInfo
}

func (s staticFeatures) Features() *FeatureFlags { return s.features }
func (s staticFeatures) Version() *VersionInfo   { return s.version }

// allFeatures enables all detectable features.
func allFeatures() staticFeatures {
	return staticFeatures{
		features: &FeatureFlags{
			HasUserStat:          true,
			HasQueryResponseTime: true,
			HasMetadataLockInfo:  true,
			HasDisksPlugin:       true,
			HasGalera:            true,
			IsReplica:            true,
		},
		version: &VersionInfo{Major: 11, Minor: 4, Patch: 3, Full: "11.4.3-MariaDB", IsMariaDB: true},
	}
}

// noFeatures disables all features.
func noFeatures() staticFeatures {
	return staticFeatures{
		features: &FeatureFlags{},
		version:  &VersionInfo{Major: 11, Minor: 4, Patch: 3, Full: "11.4.3-MariaDB", IsMariaDB: true},
	}
}

// newMockDB creates a *sql.DB with sqlmock. QueryMatcherRegexp is used because
// the exporter's queries are multiline and a literal comparison would be
// fragile.
func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// runCollect runs the collector and returns the emitted metrics.
//
// Collect runs in a goroutine and the channel is closed on completion, so the
// reader doesn't need to guess how many metrics will arrive.
func runCollect(t *testing.T, c Collector, db *sql.DB) ([]prometheus.Metric, error) {
	t.Helper()

	ch := make(chan prometheus.Metric, 1024)
	errCh := make(chan error, 1)

	go func() {
		defer close(ch)
		errCh <- c.Collect(context.Background(), db, ch)
	}()

	var metrics []prometheus.Metric
	for m := range ch {
		metrics = append(metrics, m)
	}

	return metrics, <-errCh
}

// metricSnapshot is the comparable form of a collected metric.
type metricSnapshot struct {
	Name   string
	Labels map[string]string
	Value  float64

	// Filled in only for histograms.
	SampleCount uint64
	SampleSum   float64
	Buckets     map[float64]uint64
}

// snapshot converts the metrics from the channel into inspectable structs.
func snapshot(t *testing.T, metrics []prometheus.Metric) []metricSnapshot {
	t.Helper()

	out := make([]metricSnapshot, 0, len(metrics))
	for _, m := range metrics {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))

		snap := metricSnapshot{
			Name:   metricName(t, m),
			Labels: map[string]string{},
		}
		for _, lp := range pb.GetLabel() {
			snap.Labels[lp.GetName()] = lp.GetValue()
		}

		switch {
		case pb.GetCounter() != nil:
			snap.Value = pb.GetCounter().GetValue()
		case pb.GetGauge() != nil:
			snap.Value = pb.GetGauge().GetValue()
		case pb.GetUntyped() != nil:
			snap.Value = pb.GetUntyped().GetValue()
		case pb.GetHistogram() != nil:
			h := pb.GetHistogram()
			snap.SampleCount = h.GetSampleCount()
			snap.SampleSum = h.GetSampleSum()
			snap.Buckets = make(map[float64]uint64, len(h.GetBucket()))
			for _, b := range h.GetBucket() {
				snap.Buckets[b.GetUpperBound()] = b.GetCumulativeCount()
			}
		}

		out = append(out, snap)
	}

	return out
}

// metricName extracts the fully qualified name from the Desc.
//
// Desc does not expose the name as a public field, but its String() has the
// format `Desc{fqName: "name", help: ...}`, from which the name is read.
func metricName(t *testing.T, m prometheus.Metric) string {
	t.Helper()

	desc := m.Desc().String()
	const marker = `fqName: "`
	start := indexOf(desc, marker)
	require.GreaterOrEqual(t, start, 0, "unexpected Desc format: %s", desc)
	start += len(marker)

	end := indexOf(desc[start:], `"`)
	require.GreaterOrEqual(t, end, 0, "unexpected Desc format: %s", desc)

	return desc[start : start+end]
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// findMetric locates the first metric with the given name and labels.
func findMetric(snaps []metricSnapshot, name string, labels map[string]string) (metricSnapshot, bool) {
	for _, s := range snaps {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s, true
		}
	}
	return metricSnapshot{}, false
}

// requireMetric fails the test if the metric does not exist, returning it if
// it does.
func requireMetric(t *testing.T, snaps []metricSnapshot, name string, labels map[string]string) metricSnapshot {
	t.Helper()
	s, ok := findMetric(snaps, name, labels)
	require.True(t, ok, "metric %s with labels %v not found; collected: %v", name, labels, names(snaps))
	return s
}

// requireNoMetric fails the test if the metric exists.
func requireNoMetric(t *testing.T, snaps []metricSnapshot, name string) {
	t.Helper()
	for _, s := range snaps {
		require.NotEqual(t, name, s.Name, "metric %s should not have been emitted", name)
	}
}

func names(snaps []metricSnapshot) []string {
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.Name)
	}
	return out
}

// discardWriter prevents collector logs from cluttering test output.
var _ io.Writer = io.Discard

func TestVersionInfoAtLeast(t *testing.T) {
	v := &VersionInfo{Major: 10, Minor: 0, Patch: 7}

	require.True(t, v.AtLeast(10, 0, 7), "same version should satisfy")
	require.True(t, v.AtLeast(10, 0, 6))
	require.True(t, v.AtLeast(9, 9, 9))
	require.False(t, v.AtLeast(10, 0, 8))
	require.False(t, v.AtLeast(10, 1, 0))
	require.False(t, v.AtLeast(11, 0, 0))

	var nilVersion *VersionInfo
	require.False(t, nilVersion.AtLeast(10, 0, 0), "nil version never satisfies")
}

func TestParseFloat(t *testing.T) {
	cases := []struct {
		name    string
		input   interface{}
		want    float64
		wantErr bool
	}{
		{"integer", int64(42), 42, false},
		{"numeric string", "3.5", 3.5, false},
		{"numeric bytes", []byte("120"), 120, false},
		{"ON becomes 1", "ON", 1, false},
		{"OFF becomes 0", "OFF", 0, false},
		{"YES becomes 1", "yes", 1, false},
		{"nil is an error", nil, 0, true},
		{"text is an error", "abacate", 0, true},
		{"empty is an error", "", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFloat(tc.input)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestRegistryEnabled(t *testing.T) {
	r := NewRegistry()
	r.Register(
		NewInfoCollector(testLogger(), noFeatures(), nil),
		NewGaleraCollector(false, testLogger(), noFeatures()),
		NewInnoDBCollector(true, testLogger(), noFeatures()),
		nil, // nil values are ignored
	)

	require.Len(t, r.All(), 3)
	require.ElementsMatch(t, []string{"info", "innodb"}, collectorNames(r.Enabled()))
	require.ElementsMatch(t, []string{"info", "galera", "innodb"}, r.Names())
}

func collectorNames(cs []Collector) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name())
	}
	return out
}
