//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetProbeBody is one model call the test sends through the internal proxy
// with the session token. max_tokens fixes a small reservation, and the model
// matches the session so the broker forwards the body unchanged.
const budgetProbeBody = `{"model":"stub","max_tokens":1,"messages":[{"role":"user","content":"budget probe"}]}`

// TestSessionBudgetExhaustion spends a session's request cap through the
// internal proxy. The charge that reaches the cap appends an error event and
// closes the session with reason llm_budget_exhausted, and the next call is
// refused with 429 llm_budget_exhausted and no Retry-After. The agent process
// is suspended so only this test spends the budget and the refusal can be
// observed before the runtime finalizes.
func TestSessionBudgetExhaustion(t *testing.T) {
	s := env.createSession(t, map[string]any{
		"prompt": "budget exhaustion " + randomHex(4),
		"agent":  "budget-requests",
	})
	if s.LLMBudget == nil || s.LLMBudget.MaxRequests != 6 || s.LLMBudget.MaxTokens != 0 {
		t.Fatalf("session budget = %+v, want max_requests 6", s.LLMBudget)
	}
	env.waitStatus(t, s.ID, "running", 2*time.Minute)
	waitFirstReply(t, s.ID)

	resume := stopSessionAgent(t, s.ID)
	defer resume()
	token := sessionToken(t, s.ID)

	limit := s.LLMBudget.MaxRequests
	for range int(limit) + 1 {
		cur, r := env.getSession(t, s.ID)
		expectStatus(t, r, http.StatusOK, "get session")
		if cur.LLMUsage == nil {
			t.Fatalf("budgeted session has no usage")
		}
		if cur.LLMUsage.Requests >= limit {
			break
		}
		if rec := env.proxyCall(t, token, budgetProbeBody); rec.Status != http.StatusOK {
			t.Fatalf("probe under the cap = %s", rec)
		}
	}
	spent, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if spent.LLMUsage == nil || spent.LLMUsage.Requests != limit {
		t.Fatalf("requests after spending the cap = %+v, want %d", spent.LLMUsage, limit)
	}

	rec := env.proxyCall(t, token, budgetProbeBody)
	expectStatus(t, rec, http.StatusTooManyRequests, "call after exhaustion")
	if got := rec.Header.Get("Retry-After"); got != "" {
		t.Fatalf("exhausted refusal has Retry-After %q", got)
	}
	var refused struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body, &refused); err != nil {
		t.Fatalf("refusal body: %v (%s)", err, rec.Body)
	}
	if refused.Code != "llm_budget_exhausted" {
		t.Fatalf("refusal code = %q body=%s", refused.Code, rec.Body)
	}
	after, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if after.LLMUsage == nil || *after.LLMUsage != *spent.LLMUsage {
		t.Fatalf("refused call changed usage: %+v -> %+v", spent.LLMUsage, after.LLMUsage)
	}

	// The charging call, not the refused one, appended the events.
	waitEventWithin(t, s.ID, "budget exhaustion error event", time.Minute, func(ev event) bool {
		return ev.Type == "error" && ev.Actor == "mayfly" && strings.Contains(ev.Text, "budget exhausted")
	})
	waitEventWithin(t, s.ID, "budget close event", time.Minute, func(ev event) bool {
		if ev.Type != "control" {
			return false
		}
		reason, _ := ev.Data["reason"].(string)
		return reason == "llm_budget_exhausted"
	})

	resume()
	final := env.waitStatus(t, s.ID, "completed", 2*time.Minute)
	if !strings.Contains(final.Error, "budget exhausted") {
		t.Fatalf("completed session error = %q", final.Error)
	}
	if final.LLMBudget == nil || final.LLMBudget.MaxRequests != limit {
		t.Fatalf("budget after close = %+v", final.LLMBudget)
	}
}

// TestSessionLLMBusyDoesNotCharge overlaps two proxy calls of one budgeted
// session. The second gets 429 llm_busy with Retry-After and leaves the running
// call's ledger alone, and a retry after the first finishes is admitted.
func TestSessionLLMBusyDoesNotCharge(t *testing.T) {
	s := env.createSession(t, map[string]any{
		"prompt": "budget overlap " + randomHex(4),
		"agent":  "budget-overlap",
	})
	env.waitStatus(t, s.ID, "running", 2*time.Minute)
	waitFirstReply(t, s.ID)
	token := sessionToken(t, s.ID)

	before, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if before.LLMUsage == nil {
		t.Fatal("budgeted session has no usage")
	}

	// The stub sleeps five seconds before this reply, so the call holds the
	// session's only in-flight slot while the second call arrives.
	const slowBody = `{"model":"stub","max_tokens":1,"messages":[{"role":"user","content":"resume-slow-final overlap"}]}`
	type outcome struct {
		rec response
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		rec, err := env.proxy(token, slowBody)
		first <- outcome{rec, err}
	}()
	eventually(t, 30*time.Second, "first call admitted", func() (bool, string) {
		cur, r := env.getSession(t, s.ID)
		if r.Status != http.StatusOK || cur.LLMUsage == nil {
			return false, r.String()
		}
		return cur.LLMUsage.Requests == before.LLMUsage.Requests+1, fmt.Sprintf("requests=%d", cur.LLMUsage.Requests)
	})
	select {
	case got := <-first:
		t.Fatalf("the slow call finished before the overlap: %s err=%v", got.rec, got.err)
	default:
	}

	during, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	rec := env.proxyCall(t, token, budgetProbeBody)
	expectStatus(t, rec, http.StatusTooManyRequests, "overlapping call")
	if got := rec.Header.Get("Retry-After"); got == "" || got == "0" {
		t.Fatalf("busy refusal Retry-After = %q", got)
	}
	var refused struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body, &refused); err != nil {
		t.Fatalf("refusal body: %v (%s)", err, rec.Body)
	}
	if refused.Code != "llm_busy" {
		t.Fatalf("refusal code = %q body=%s", refused.Code, rec.Body)
	}
	afterBusy, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if afterBusy.LLMUsage == nil || *afterBusy.LLMUsage != *during.LLMUsage {
		t.Fatalf("busy refusal settled the running call: %+v -> %+v", during.LLMUsage, afterBusy.LLMUsage)
	}

	var got outcome
	select {
	case got = <-first:
	case <-time.After(20 * time.Second):
		t.Fatal("the slow call did not finish")
	}
	if got.err != nil {
		t.Fatalf("slow call: %v", got.err)
	}
	expectStatus(t, got.rec, http.StatusOK, "slow call")

	// The running call's charge landed once it finished.
	settled, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if settled.LLMUsage == nil || settled.LLMUsage.Requests != before.LLMUsage.Requests+1 ||
		settled.LLMUsage.PromptTokens+settled.LLMUsage.CompletionTokens <=
			before.LLMUsage.PromptTokens+before.LLMUsage.CompletionTokens {
		t.Fatalf("first call was not charged: before %+v after %+v", before.LLMUsage, settled.LLMUsage)
	}

	// The slot was released: a retry is admitted, not refused as busy.
	if rec := env.proxyCall(t, token, budgetProbeBody); rec.Status != http.StatusOK {
		t.Fatalf("retry after the first call = %s", rec)
	}
	retried, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if retried.LLMUsage == nil || retried.LLMUsage.Requests != before.LLMUsage.Requests+2 {
		t.Fatalf("requests after the retry = %+v, want %d", retried.LLMUsage, before.LLMUsage.Requests+2)
	}
}

// TestSessionBudgetUsageAndCLI checks the session object and the CLI tokens
// line for a budgeted session with a token cap.
func TestSessionBudgetUsageAndCLI(t *testing.T) {
	s := env.createSession(t, map[string]any{
		"prompt": "budget usage " + randomHex(4),
		"agent":  "budget-tokens",
	})
	if s.LLMBudget == nil || s.LLMBudget.MaxTokens != 100000 || s.LLMBudget.MaxRequests != 0 {
		t.Fatalf("session budget = %+v, want max_tokens 100000", s.LLMBudget)
	}
	// A budgeted session publishes usage from creation, so the field exists
	// even before its first model call.
	if s.LLMUsage == nil {
		t.Fatal("budgeted session has no llm_usage at creation")
	}
	env.waitStatus(t, s.ID, "running", 2*time.Minute)
	waitFirstReply(t, s.ID)

	cur, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if cur.LLMUsage == nil || cur.LLMUsage.Total() == 0 {
		t.Fatalf("usage after a model call = %+v", cur.LLMUsage)
	}
	if cur.LLMBudget == nil {
		t.Fatalf("session lost its budget")
	}

	out := mayflyGet(t, s.ID)
	match := tokensLineRE.FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("mayfly get has no tokens line:\n%s", out)
	}
	prompt, _ := strconv.ParseInt(match[1], 10, 64)
	completion, _ := strconv.ParseInt(match[2], 10, 64)
	budget, _ := strconv.ParseInt(match[3], 10, 64)
	if prompt != cur.LLMUsage.PromptTokens || completion != cur.LLMUsage.CompletionTokens {
		t.Fatalf("CLI tokens %d/%d, session %d/%d", prompt, completion, cur.LLMUsage.PromptTokens, cur.LLMUsage.CompletionTokens)
	}
	if budget != cur.LLMBudget.MaxTokens {
		t.Fatalf("CLI budget = %d, session = %d", budget, cur.LLMBudget.MaxTokens)
	}
}

// TestSessionBudgetMetrics checks the token and estimate counters move for a
// budgeted session that a model call charged.
func TestSessionBudgetMetrics(t *testing.T) {
	const tokens = `{kind="prompt",model="stub"}`
	const estimated = `{model="stub"}`
	promptBefore := metricValue(t, "mayfly_llm_tokens_total", tokens)
	estimatedBefore := metricValue(t, "mayfly_llm_usage_estimated_total", estimated)

	s := env.createSession(t, map[string]any{
		// The second line is the stub's no-usage directive. It is inert on a
		// stub that sends no usage, and keeps this case on the estimate path
		// once the stub can report usage.
		"prompt": "budget metrics " + randomHex(4) + "\nno-usage",
		"agent":  "budget-tokens",
	})
	env.waitStatus(t, s.ID, "running", 2*time.Minute)
	waitFirstReply(t, s.ID)

	// This branch's llmstub sends no usage, so every charge is a size
	// estimate. A provider that reports usage moves only the token counter.
	eventually(t, 30*time.Second, "budget metrics move", func() (bool, string) {
		prompt := metricValue(t, "mayfly_llm_tokens_total", tokens)
		est := metricValue(t, "mayfly_llm_usage_estimated_total", estimated)
		return prompt > promptBefore && est > estimatedBefore,
			fmt.Sprintf("prompt %v -> %v, estimated %v -> %v", promptBefore, prompt, estimatedBefore, est)
	})
}

// TestSessionBudgetSurvivesRestart checks that the bbolt record keeps a
// budgeted session's usage and budget across a control-plane replacement.
func TestSessionBudgetSurvivesRestart(t *testing.T) {
	s := env.createSession(t, map[string]any{
		"prompt": "budget restart " + randomHex(4),
		"agent":  "budget-tokens",
	})
	env.waitStatus(t, s.ID, "running", 2*time.Minute)
	waitFirstReply(t, s.ID)

	before, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if before.LLMUsage == nil || before.LLMUsage.Total() == 0 || before.LLMBudget == nil {
		t.Fatalf("session before the restart = usage %+v budget %+v", before.LLMUsage, before.LLMBudget)
	}

	env.restartControlPlane(t)

	eventually(t, 90*time.Second, "adopted session keeps its budget", func() (bool, string) {
		cur, r := env.getSession(t, s.ID)
		if r.Status != http.StatusOK {
			return false, r.String()
		}
		if cur.LLMUsage == nil || cur.LLMBudget == nil {
			return false, fmt.Sprintf("usage %+v budget %+v", cur.LLMUsage, cur.LLMBudget)
		}
		return *cur.LLMUsage == *before.LLMUsage && *cur.LLMBudget == *before.LLMBudget,
			fmt.Sprintf("usage %+v budget %+v", *cur.LLMUsage, *cur.LLMBudget)
	})
}

// TestSessionBudgetAgentOverride checks that each agent's caps become the
// session's resolved budget.
func TestSessionBudgetAgentOverride(t *testing.T) {
	large := env.createSession(t, map[string]any{
		"prompt": "budget override large " + randomHex(4),
		"agent":  "budget-large",
	})
	if large.LLMBudget == nil || *large.LLMBudget != (llmBudget{MaxTokens: 200000, MaxRequests: 20}) {
		t.Fatalf("large budget = %+v", large.LLMBudget)
	}
	small := env.createSession(t, map[string]any{
		"prompt": "budget override small " + randomHex(4),
		"agent":  "budget-small",
	})
	if small.LLMBudget == nil || *small.LLMBudget != (llmBudget{MaxTokens: 50000, MaxRequests: 4}) {
		t.Fatalf("small budget = %+v", small.LLMBudget)
	}
}

// TestSessionBudgetDefaultsOff checks that the e2e agent, with both global caps
// at zero, has no budget and never sees a budget refusal during its turn.
func TestSessionBudgetDefaultsOff(t *testing.T) {
	const busy = `{model="stub",result="busy"}`
	const overBudget = `{model="stub",result="over_budget"}`
	busyBefore := metricValue(t, "mayfly_llm_requests_total", busy)
	overBefore := metricValue(t, "mayfly_llm_requests_total", overBudget)

	s := env.createSession(t, map[string]any{
		"prompt": "budget defaults off " + randomHex(4),
		"agent":  "e2e",
	})
	if s.LLMBudget != nil {
		t.Fatalf("unbudgeted session reports a budget: %+v", s.LLMBudget)
	}
	env.waitStatus(t, s.ID, "running", 2*time.Minute)
	waitFirstReply(t, s.ID)

	cur, r := env.getSession(t, s.ID)
	expectStatus(t, r, http.StatusOK, "get session")
	if cur.LLMBudget != nil {
		t.Fatalf("unbudgeted session reports a budget: %+v", cur.LLMBudget)
	}
	// The ledger still counts usage for an unbudgeted session.
	if cur.LLMUsage == nil || cur.LLMUsage.Requests == 0 {
		t.Fatalf("unbudgeted session usage = %+v", cur.LLMUsage)
	}
	for _, ev := range env.events(t, s.ID) {
		if ev.Type == "error" && strings.Contains(ev.Text, "budget") {
			t.Fatalf("unbudgeted session got a budget error: %+v", ev)
		}
	}
	if got := metricValue(t, "mayfly_llm_requests_total", busy); got != busyBefore {
		t.Fatalf("busy refusals moved for an unbudgeted session: %v -> %v", busyBefore, got)
	}
	if got := metricValue(t, "mayfly_llm_requests_total", overBudget); got != overBefore {
		t.Fatalf("over-budget refusals moved for an unbudgeted session: %v -> %v", overBefore, got)
	}
}

// stopSessionAgent suspends the session pod's agent process, so the test can
// spend the model budget through the internal proxy and observe a refusal
// before the runtime finalizes. The returned function resumes the process; a
// cleanup resumes it too, so a failed assertion cannot leave it stopped.
func stopSessionAgent(t *testing.T, id string) func() {
	t.Helper()
	pods := env.podNames(t, id)
	if len(pods) != 1 {
		t.Fatalf("session %s pods = %v, want one", id, pods)
	}
	if code, out := env.execIn(t, pods[0], "sh", "-c", "kill -STOP 1"); code != 0 {
		t.Fatalf("stop the agent in %s: exit %d: %s", pods[0], code, out)
	}
	var once sync.Once
	resume := func() {
		once.Do(func() {
			if code, out := env.execIn(t, pods[0], "sh", "-c", "kill -CONT 1"); code != 0 {
				t.Errorf("resume the agent in %s: exit %d: %s", pods[0], code, out)
			}
		})
	}
	t.Cleanup(resume)
	return resume
}

// sessionToken reads the scoped model token from the session Secret.
func sessionToken(t *testing.T, id string) string {
	t.Helper()
	data, err := env.secretData("mayfly-" + id)
	if err != nil {
		t.Fatalf("session secret: %v", err)
	}
	token := data["token"]
	if token == "" {
		t.Fatal("session secret has no token")
	}
	return token
}

// proxy calls one completion through the internal LLM proxy. It takes no
// testing.T, so a goroutine can use it, and the cluster's HTTP client bounds
// the call.
func (c *cluster) proxy(token, body string) (response, error) {
	req, err := http.NewRequest(http.MethodPost, c.url("/internal/v1/llm/chat/completions"), strings.NewReader(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

func (c *cluster) proxyCall(t *testing.T, token, body string) response {
	t.Helper()
	rec, err := c.proxy(token, body)
	if err != nil {
		t.Fatalf("internal LLM call: %v", err)
	}
	return rec
}

// waitFirstReply waits for the agent to finish its first turn.
func waitFirstReply(t *testing.T, id string) {
	t.Helper()
	waitEventWithin(t, id, "first assistant reply", time.Minute, func(ev event) bool {
		return ev.Type == "assistant_message" && ev.Text != ""
	})
}

// waitEventWithin is waitEvent with the caller's timeout.
func waitEventWithin(t *testing.T, id, what string, timeout time.Duration, match func(event) bool) event {
	t.Helper()
	var found event
	eventually(t, timeout, "session "+id+": "+what, func() (bool, string) {
		for _, ev := range env.events(t, id) {
			if match(ev) {
				found = ev
				return true, ""
			}
		}
		return false, "no matching event"
	})
	return found
}

// metricValue reads one metric series from GET /metrics and returns zero when
// the series is absent.
func metricValue(t *testing.T, name, labels string) float64 {
	t.Helper()
	r := env.admin(t, http.MethodGet, "/metrics", nil)
	expectStatus(t, r, http.StatusOK, "metrics")
	prefix := name + labels + " "
	for _, line := range strings.Split(string(r.Body), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
		if err != nil {
			t.Fatalf("metric %s%s: %v", name, labels, err)
		}
		return value
	}
	return 0
}

// tokensLineRE matches the CLI tokens line, with the optional budget suffix.
var tokensLineRE = regexp.MustCompile(`tokens:\s+(\d+) prompt / (\d+) completion(?: \(budget (\d+)\))?`)

// mayflyGet builds the CLI and returns `mayfly get <id>` output, run against
// the forwarded control plane.
func mayflyGet(t *testing.T, id string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	root := filepath.Dir(filepath.Dir(file))
	bin := filepath.Join(t.TempDir(), "mayfly")
	build := exec.Command("go", "build", "-o", bin, "./cmd/mayfly")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mayfly: %v: %s", err, out)
	}
	cmd := exec.Command(bin, "get", id)
	// Build the environment from scratch, so a MAYFLY_URL or MAYFLY_API_TOKEN
	// in the test's own environment cannot win over the release under test.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"MAYFLY_URL=" + env.url(""),
		"MAYFLY_API_TOKEN=" + env.token,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("mayfly get %s: %v: %s", id, err, stderr.String())
	}
	return stdout.String()
}
