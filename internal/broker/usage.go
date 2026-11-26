package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"time"
)

// Usage is the token count of one model response, as the provider reported it.
type Usage struct{ Prompt, Completion int64 }

// CallResult describes one ServeLLM call. Later packages charge Usage when
// Reported is true. A TransportErr charges the full reservation ReqBytes/4 +
// MaxOut, because the provider may have billed a request whose response never
// arrived. Otherwise a 2xx or 5xx is estimated from ReqBytes/4 + RespBytes/4,
// and the completion part of that estimate stops at MaxOut, the cap the
// request set. A forwarded 4xx costs no tokens, and no request that was not
// forwarded costs anything.
type CallResult struct {
	Status    int
	Usage     Usage
	Reported  bool // upstream sent usage
	Streamed  bool
	ReqBytes  int64 // forwarded request body
	RespBytes int64 // bytes written to the session
	Forwarded bool  // an upstream request was made
	// TransportErr reports that the request was sent but no response arrived:
	// a dial or TLS failure, or the wait for response headers timed out. Status
	// is then 502 from Mayfly, not a provider status.
	TransportErr bool
	// MaxOut is the completion cap the admit hook reserved for this call.
	MaxOut int64
}

// Refusal is an admit-hook refusal. The broker writes Status and the JSON body
// {"error": Message, "code": Code}, then returns Forwarded=false with no
// upstream call. A hook error that is not a *Refusal becomes a 500 with no
// code. A positive RetryAfter also sets the Retry-After header in whole
// seconds, rounded up.
type Refusal struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

// Error implements error.
func (r *Refusal) Error() string { return r.Message }

const (
	// maxUsageBufferBytes bounds the non-streamed response copy kept for usage
	// parsing. It matches mcp.maxResponseBytes. A larger body drops the copy
	// instead of growing memory, and Reported stays false.
	maxUsageBufferBytes = 8 << 20
	// maxUsageLineBytes bounds one streamed SSE line. A usage chunk is a few
	// hundred bytes. A longer line is dropped up to the next newline.
	maxUsageLineBytes = 64 << 10
	// defaultCompletionCap is the completion cap that Admit reserves for a
	// request that sets neither max_tokens nor max_completion_tokens.
	defaultCompletionCap = 4096
)

// errNotJSONObject rejects a body that is valid JSON but carries no members.
var errNotJSONObject = errors.New("request body is not a JSON object")

// outputLimit returns the completion cap the request sets, or
// defaultCompletionCap. max_completion_tokens wins over the deprecated
// max_tokens when both are present.
func outputLimit(completion, legacy json.RawMessage) int64 {
	if n := rawTokenCount(completion); n > 0 {
		return n
	}
	if n := rawTokenCount(legacy); n > 0 {
		return n
	}
	return defaultCompletionCap
}

// rawTokenCount reads one JSON number field. A missing, null, string, or
// non-positive value yields zero.
func rawTokenCount(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	return tokenCount(json.Number(bytes.TrimSpace(raw)))
}

// tokenCount converts one JSON number to a token count. A negative value
// clamps to zero and a fractional value keeps its integer part. A value beyond
// int64 saturates, so a broken or hostile provider cannot wrap the counter.
func tokenCount(n json.Number) int64 {
	if n == "" {
		return 0
	}
	if v, err := n.Int64(); err == nil {
		if v < 0 {
			return 0
		}
		return v
	}
	f, err := n.Float64()
	if err != nil {
		// ParseFloat reports a range error with ±Inf or a flushed zero.
		if math.IsInf(f, 1) {
			return math.MaxInt64
		}
		return 0
	}
	switch {
	case math.IsNaN(f), f <= 0:
		return 0
	case f >= float64(math.MaxInt64):
		return math.MaxInt64
	}
	return int64(f)
}

// decodeUsage reads {"usage":{"prompt_tokens","completion_tokens"}} from one
// JSON object. ok is false for anything that is not a JSON object with a
// non-null usage object, which makes an absent usage indistinguishable from an
// unparseable one: both are unreported.
func decodeUsage(body []byte) (Usage, bool) {
	var doc struct {
		Usage *struct {
			Prompt     json.Number `json:"prompt_tokens"`
			Completion json.Number `json:"completion_tokens"`
		} `json:"usage"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return Usage{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		// Trailing data means the body is not one JSON value.
		return Usage{}, false
	}
	if doc.Usage == nil {
		return Usage{}, false
	}
	return Usage{
		Prompt:     tokenCount(doc.Usage.Prompt),
		Completion: tokenCount(doc.Usage.Completion),
	}, true
}

// usageScanner is the io.Writer tee that streamCopy uses to read usage from a
// streamed response. It splits on newlines as chunks arrive, so a usage chunk
// parses the same wherever the read boundary falls. It never blocks, never
// returns an error, and never holds more than maxUsageLineBytes of a line.
type usageScanner struct {
	line  []byte
	skip  bool
	total int64
	usage Usage
	found bool
}

// Write consumes one chunk of the stream. It always reports the full length
// and a nil error, so the tee stays transparent to streamCopy.
func (s *usageScanner) Write(p []byte) (int, error) {
	n := len(p)
	s.total += int64(n)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		var segment []byte
		if end < 0 {
			segment, p = p, nil
		} else {
			segment, p = p[:end], p[end+1:]
		}
		if s.skip {
			// Drop the rest of an over-long line, then resume.
			if end >= 0 {
				s.skip = false
			}
			continue
		}
		if len(s.line)+len(segment) > maxUsageLineBytes {
			s.line = s.line[:0]
			s.skip = end < 0
			continue
		}
		s.line = append(s.line, segment...)
		if end >= 0 {
			s.observe()
			s.line = s.line[:0]
		}
	}
	return n, nil
}

// Result returns the last usage chunk the stream carried.
func (s *usageScanner) Result() (Usage, bool) { return s.usage, s.found }

// observe parses one complete line. Only an SSE data line with a usage object
// counts, and the last non-null usage wins.
func (s *usageScanner) observe() {
	line := bytes.TrimSuffix(s.line, []byte("\r"))
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return
	}
	if usage, ok := decodeUsage(payload); ok {
		s.usage = usage
		s.found = true
	}
}

// bodyCapture keeps a bounded copy of a non-streamed response for usage
// parsing. It counts every byte and drops the copy once the limit is exceeded,
// so a huge response cannot grow memory.
type bodyCapture struct {
	buf      []byte
	limit    int
	total    int64
	overflow bool
}

// Write consumes one chunk of the response body and always reports the full
// length.
func (c *bodyCapture) Write(p []byte) (int, error) {
	c.total += int64(len(p))
	if c.overflow {
		return len(p), nil
	}
	if len(c.buf)+len(p) > c.limit {
		c.overflow = true
		c.buf = nil
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// injectStreamOptions returns body with stream_options.include_usage set to
// true, keeping every other key, including other stream_options keys. It
// returns the body unchanged when stream is false, because OpenAI rejects
// stream_options on a non-streamed call.
func injectStreamOptions(body []byte, stream bool) ([]byte, error) {
	if !stream {
		return body, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errNotJSONObject
	}
	var options map[string]json.RawMessage
	if raw, ok := doc["stream_options"]; ok {
		// A non-object stream_options is unusable as a merge target, so it is
		// replaced. The provider would reject it anyway.
		if err := json.Unmarshal(raw, &options); err != nil {
			options = nil
		}
	}
	if options == nil {
		options = map[string]json.RawMessage{}
	}
	options["include_usage"] = json.RawMessage("true")
	encoded, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	doc["stream_options"] = encoded
	return json.Marshal(doc)
}
