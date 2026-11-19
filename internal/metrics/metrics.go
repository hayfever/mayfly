// Package metrics owns the control-plane Prometheus registry, the collectors
// that describe control-plane state, and the HTTP handler that serves them.
// Call sites depend on this package only for recording; the registry never
// depends on the packages it describes.
package metrics

import (
	"context"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hayfever/mayfly/internal/model"
)

// Label values that do not name a domain state.
const (
	// RouteUnmatched labels a request that matched no registered route. The
	// label is a route template, never a raw path, so the set stays small.
	RouteUnmatched = "unmatched"
	// ResultOK and ResultError label an outcome that is only success or
	// failure, such as a proxied model call.
	ResultOK    = "ok"
	ResultError = "error"
	// AttemptSucceeded, AttemptFailed, and AttemptRetryLater label one queue
	// job attempt.
	AttemptSucceeded  = "succeeded"
	AttemptFailed     = "failed"
	AttemptRetryLater = "retry_later"
)

// queueScrapeTimeout bounds the queue read that one scrape starts, so a slow
// queue backend cannot hold the metrics endpoint open.
const queueScrapeTimeout = 2 * time.Second

// sessionStates lists the states the session gauge always reports, so a state
// at zero stays visible in dashboards.
var sessionStates = []model.SessionStatus{
	model.StatusPending,
	model.StatusRunning,
	model.StatusClosing,
	model.StatusCompleted,
	model.StatusFailed,
	model.StatusExpired,
}

// BuildInfo identifies the running binary. ldflags set these values at
// release time.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// QueueDepth is the number of queued jobs in each state.
type QueueDepth struct {
	Pending int
	Leased  int
	Dead    int
}

// Sources supplies the state that only a scrape can measure. A nil function
// leaves its gauge out of the registry.
type Sources struct {
	// Sessions returns the number of sessions in each state.
	Sessions func() map[model.SessionStatus]int
	// Queue returns the queue depth. An error leaves the gauge unset for that
	// scrape.
	Queue func(ctx context.Context) (QueueDepth, error)
}

// Metrics owns the registry and every collector Mayfly exports. A nil
// *Metrics accepts every recording call and drops it, so call sites can stay
// free of nil checks.
type Metrics struct {
	registry *prometheus.Registry

	sessionsCreated  prometheus.Counter
	sessionsFinished *prometheus.CounterVec
	queueAttempts    *prometheus.CounterVec
	httpRequests     *prometheus.CounterVec
	httpDuration     *prometheus.HistogramVec
	llmRequests      *prometheus.CounterVec
	llmDuration      *prometheus.HistogramVec
	gatewayCalls     *prometheus.CounterVec
}

// New builds the registry and registers every collector.
func New(src Sources, build BuildInfo) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		sessionsCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mayfly_sessions_created_total",
			Help: "Sessions accepted by this process. A repeated idempotent create is not counted.",
		}),
		sessionsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_sessions_finished_total",
			Help: "Sessions that reached a terminal state, by result.",
		}, []string{"result"}),
		queueAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_queue_job_attempts_total",
			Help: "Queue job attempts handled by this process, by result.",
		}, []string{"result"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_http_requests_total",
			Help: "HTTP requests served, by route template, method, and status code.",
		}, []string{"route", "method", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mayfly_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, by route template and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		llmRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_llm_requests_total",
			Help: "Model calls proxied to the upstream provider, by model and result.",
		}, []string{"model", "result"}),
		llmDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mayfly_llm_request_duration_seconds",
			Help:    "Model call duration in seconds, by model.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"model"}),
		gatewayCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_gateway_calls_total",
			Help: "Tool calls the gateway finished or refused, by tool and result.",
		}, []string{"tool", "result"}),
	}
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mayfly_build_info",
		Help: "Build identity of the running control plane; the value is always 1.",
	}, []string{"version", "commit", "date", "go_version"})
	buildInfo.WithLabelValues(build.Version, build.Commit, build.Date, runtime.Version()).Set(1)

	m.registry.MustRegister(
		m.sessionsCreated,
		m.sessionsFinished,
		m.queueAttempts,
		m.httpRequests,
		m.httpDuration,
		m.llmRequests,
		m.llmDuration,
		m.gatewayCalls,
		buildInfo,
	)
	if src.Sessions != nil {
		m.registry.MustRegister(&stateCollector{
			desc: prometheus.NewDesc(
				"mayfly_sessions",
				"Current sessions by state, including sessions restored from the record store.",
				[]string{"state"}, nil,
			),
			counts: src.Sessions,
			states: sessionStates,
		})
	}
	if src.Queue != nil {
		m.registry.MustRegister(&queueCollector{
			desc: prometheus.NewDesc(
				"mayfly_queue_jobs",
				"Queued jobs by state.",
				[]string{"state"}, nil,
			),
			depth: src.Queue,
		})
	}
	return m
}

// Handler serves the registry in the Prometheus text format at /metrics.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	return mux
}

// SessionCreated records one accepted session.
func (m *Metrics) SessionCreated() {
	if m == nil {
		return
	}
	m.sessionsCreated.Inc()
}

// SessionFinished records one session that reached a terminal state.
func (m *Metrics) SessionFinished(status model.SessionStatus) {
	if m == nil {
		return
	}
	m.sessionsFinished.WithLabelValues(string(status)).Inc()
}

// QueueJobAttempt records one finished queue job attempt.
func (m *Metrics) QueueJobAttempt(result string) {
	if m == nil {
		return
	}
	m.queueAttempts.WithLabelValues(result).Inc()
}

// HTTPRequest records one served request. route must be a route template.
func (m *Metrics) HTTPRequest(route, method string, code int, duration time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	m.httpDuration.WithLabelValues(route, method).Observe(duration.Seconds())
}

// LLMRequest records one proxied model call.
func (m *Metrics) LLMRequest(modelName, result string, duration time.Duration) {
	if m == nil {
		return
	}
	m.llmRequests.WithLabelValues(modelName, result).Inc()
	m.llmDuration.WithLabelValues(modelName).Observe(duration.Seconds())
}

// ToolCall records one finished or refused tool call. It satisfies the tool
// gateway's call observer.
func (m *Metrics) ToolCall(tool, result string) {
	if m == nil {
		return
	}
	m.gatewayCalls.WithLabelValues(tool, result).Inc()
}

// stateCollector reports one gauge series per state, including states at zero.
type stateCollector struct {
	desc   *prometheus.Desc
	counts func() map[model.SessionStatus]int
	states []model.SessionStatus
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.counts()
	emitted := make(map[model.SessionStatus]bool, len(counts))
	for _, state := range c.states {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(counts[state]), string(state))
		emitted[state] = true
	}
	for state, n := range counts {
		if emitted[state] {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), string(state))
	}
}

// queueCollector reports the queue depth at scrape time. A queue error leaves
// the gauge unset, so a failed read never reports a false zero.
type queueCollector struct {
	desc  *prometheus.Desc
	depth func(ctx context.Context) (QueueDepth, error)
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), queueScrapeTimeout)
	defer cancel()
	depth, err := c.depth(ctx)
	if err != nil {
		return
	}
	for state, n := range map[string]int{
		"pending": depth.Pending,
		"leased":  depth.Leased,
		"dead":    depth.Dead,
	} {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), state)
	}
}
