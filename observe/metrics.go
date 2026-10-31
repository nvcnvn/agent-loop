// Package observe collects runtime metrics and serves them in the Prometheus
// text exposition format.
package observe

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// buckets are histogram upper bounds in seconds, wide enough for model calls.
var buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

// Metrics holds counters, histograms and gauges. A nil *Metrics ignores
// every call, so instrumentation needs no nil checks.
type Metrics struct {
	mu     sync.Mutex
	series map[string]*series
	gauges map[string]func() float64
}

type series struct {
	name, labels string
	histogram    bool
	value        float64 // counter value, or histogram sum
	count        uint64
	buckets      []uint64
}

// New returns an empty Metrics.
func New() *Metrics {
	return &Metrics{series: map[string]*series{}, gauges: map[string]func() float64{}}
}

// Add increases the counter name{labels} by v; labels are key, value pairs.
func (m *Metrics) Add(name string, v float64, labels ...string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.get(name, labels, false).value += v
}

// Observe records v in the histogram name{labels}.
func (m *Metrics) Observe(name string, v float64, labels ...string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.get(name, labels, true)
	s.value += v
	s.count++
	for i, le := range buckets {
		if v <= le {
			s.buckets[i]++
		}
	}
}

// Gauge registers f, read on every scrape, as the gauge name.
func (m *Metrics) Gauge(name string, f func() float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = f
}

func (m *Metrics) get(name string, labels []string, histogram bool) *series {
	var b strings.Builder
	for i := 0; i+1 < len(labels); i += 2 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(labels[i] + `="` + escape(labels[i+1]) + `"`)
	}
	key := name + "{" + b.String() + "}"
	s := m.series[key]
	if s == nil {
		s = &series{name: name, labels: b.String(), histogram: histogram}
		if histogram {
			s.buckets = make([]uint64, len(buckets))
		}
		m.series[key] = s
	}
	return s
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// WriteText writes every metric in the Prometheus text format.
func (m *Metrics) WriteText(w io.Writer) {
	if m == nil {
		return
	}
	m.mu.Lock()
	all := make([]*series, 0, len(m.series))
	for _, s := range m.series {
		cp := *s
		cp.buckets = append([]uint64(nil), s.buckets...)
		all = append(all, &cp)
	}
	gauges := make(map[string]func() float64, len(m.gauges))
	for k, f := range m.gauges {
		gauges[k] = f
	}
	m.mu.Unlock()

	sort.Slice(all, func(i, j int) bool {
		if all[i].name != all[j].name {
			return all[i].name < all[j].name
		}
		return all[i].labels < all[j].labels
	})
	typed := ""
	for _, s := range all {
		if s.name != typed {
			kind := "counter"
			if s.histogram {
				kind = "histogram"
			}
			fmt.Fprintf(w, "# TYPE %s %s\n", s.name, kind)
			typed = s.name
		}
		if !s.histogram {
			fmt.Fprintf(w, "%s%s %s\n", s.name, braces(s.labels), num(s.value))
			continue
		}
		for i, le := range buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", s.name, braces(join(s.labels, `le="`+num(le)+`"`)), s.buckets[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", s.name, braces(join(s.labels, `le="+Inf"`)), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", s.name, braces(s.labels), num(s.value))
		fmt.Fprintf(w, "%s_count%s %d\n", s.name, braces(s.labels), s.count)
	}
	names := make([]string, 0, len(gauges))
	for name := range gauges {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "# TYPE %s gauge\n%s %s\n", name, name, num(gauges[name]()))
	}
}

// ServeHTTP serves the metrics for a Prometheus scrape.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.WriteText(w)
}

func braces(labels string) string {
	if labels == "" {
		return ""
	}
	return "{" + labels + "}"
}

func join(labels, extra string) string {
	if labels == "" {
		return extra
	}
	return labels + "," + extra
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
