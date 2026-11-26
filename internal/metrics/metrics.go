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
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hayfever/mayfly/internal/model"
)

// Label values that do not name a domain state.
const (
	// RouteUnmatched labels a request that matched no registered route. The
	// label is a route template, never a raw path, so the set stays small.
	RouteUnmatched = "unmatched"
	// ResultOK and ResultError label an outcome that is only success or
	// failure, such as a proxied model call. ResultBusy, ResultOverBudget,
	// and ResultPersistFailed label a model call the budget refused before any
	// upstream request.
	ResultOK            = "ok"
	ResultError         = "error"
	ResultBusy          = "busy"
	ResultOverBudget    = "over_budget"
	ResultPersistFailed = "persist_failed"
	// AttemptSucceeded, AttemptFailed, and AttemptRetryLater label one queue
	// job attempt.
	AttemptSucceeded  = "succeeded"
	AttemptFailed     = "failed"
	AttemptRetryLater = "retry_later"
	// ComponentQueue, ComponentSession, and ComponentHTTP label a recovered
	// panic. ComponentOther keeps the label set closed when a caller passes an
	// unknown component.
	ComponentQueue   = "queue"
	ComponentSession = "session"
	ComponentHTTP    = "http"
	ComponentOther   = "other"
	// AuthViaBearer and AuthViaWeb label a refused API credential. The set is
	// closed: another value is dropped rather than creating a series.
	AuthViaBearer = "bearer"
	AuthViaWeb    = "web"
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
	panics           *prometheus.CounterVec
	authFailures     *prometheus.CounterVec
	httpRequests     *prometheus.CounterVec
	httpDuration     *prometheus.HistogramVec
	llmRequests      *prometheus.CounterVec
	llmDuration      *prometheus.HistogramVec
	llmTokens        *prometheus.CounterVec
	llmEstimated     *prometheus.CounterVec
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
		panics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_panics_total",
			Help: "Panics recovered by this process, by component.",
		}, []string{"component"}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_auth_failures_total",
			Help: "Refused API credentials, by authentication method.",
		}, []string{"via"}),
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
			Help: "Model calls by model and result. A call the budget refused was not forwarded and is labeled busy, over_budget, or persist_failed.",
		}, []string{"model", "result"}),
		llmDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mayfly_llm_request_duration_seconds",
			Help:    "Model call duration in seconds, by model.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"model"}),
		llmTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_llm_tokens_total",
			Help: "Tokens charged to sessions, by model and kind. Includes estimated tokens.",
		}, []string{"model", "kind"}),
		llmEstimated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mayfly_llm_usage_estimated_total",
			Help: "Model calls charged from a size estimate or a transport-failure reservation, by model.",
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
		// The standard collectors expose go_goroutines, go_memstats_*, and
		// process_* so a soak test can watch runtime growth without a
		// separate profile endpoint.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.sessionsCreated,
		m.sessionsFinished,
		m.queueAttempts,
		m.panics,
		m.authFailures,
		m.httpRequests,
		m.httpDuration,
		m.llmRequests,
		m.llmDuration,
		m.llmTokens,
		m.llmEstimated,
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

// Panic records one panic that a recover kept from ending the process. The
// component label is closed: an unknown value is recorded as ComponentOther.
func (m *Metrics) Panic(component string) {
	if m == nil {
		return
	}
	switch component {
	case ComponentQueue, ComponentSession, ComponentHTTP:
	default:
		component = ComponentOther
	}
	m.panics.WithLabelValues(component).Inc()
}

// AuthFailure records one refused API credential. via is AuthViaBearer or
// AuthViaWeb; another value is dropped so the label set stays closed.
func (m *Metrics) AuthFailure(via string) {
	if m == nil {
		return
	}
	switch via {
	case AuthViaBearer, AuthViaWeb:
		m.authFailures.WithLabelValues(via).Inc()
	}
}

// HTTPRequest records one served request. route must be a route template.
func (m *Metrics) HTTPRequest(route, method string, code int, duration time.Duration) {
	if m == nil {
		return
	}
	method = methodLabel(method)
	m.httpRequests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
	m.httpDuration.WithLabelValues(route, method).Observe(duration.Seconds())
}

// methodLabel bounds the method label. Every request is recorded, including
// unauthenticated ones, and a client chooses the method token freely, so a
// method outside the standard set is recorded as OTHER.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	}
	return "OTHER"
}

// LLMRequest records one proxied model call.
func (m *Metrics) LLMRequest(modelName, result string, duration time.Duration) {
	if m == nil {
		return
	}
	m.llmRequests.WithLabelValues(modelName, result).Inc()
	m.llmDuration.WithLabelValues(modelName).Observe(duration.Seconds())
}

// LLMTokens records the tokens one settled model call added to a session's
// ledger. estimated reports that the charge came from a size estimate or a
// transport-failure reservation rather than from reported usage. The caller
// passes non-negative counts, so a counter cannot go backwards.
func (m *Metrics) LLMTokens(modelName string, prompt, completion int64, estimated bool) {
	if m == nil {
		return
	}
	if prompt > 0 {
		m.llmTokens.WithLabelValues(modelName, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.llmTokens.WithLabelValues(modelName, "completion").Add(float64(completion))
	}
	if estimated {
		m.llmEstimated.WithLabelValues(modelName).Inc()
	}
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
