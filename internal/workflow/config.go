package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Typed node configurations. Executors decode into these with DecodeConfig.

type WebhookConfig struct {
	HMACSecret string `json:"hmac_secret,omitempty"` // name of a workspace secret used to verify signatures
}

type ScheduleConfig struct {
	Cron     string         `json:"cron"`
	Timezone string         `json:"timezone,omitempty"`
	Payload  map[string]any `json:"payload,omitempty"`
}

type ManualConfig struct {
	Sample any `json:"sample,omitempty"`
}

type ConditionConfig struct {
	Expression string `json:"expression"`
}

type DelayConfig struct {
	DurationMS *int64 `json:"duration_ms,omitempty"`
	Duration   string `json:"duration,omitempty"` // Go-style, e.g. "5m"
	Until      string `json:"until,omitempty"`    // expression yielding an RFC3339 time
}

// MaxDelay bounds durable delays.
const MaxDelay = 30 * 24 * time.Hour

// Wait resolves a static duration; ok is false for `until` delays.
func (c DelayConfig) Wait() (d time.Duration, ok bool, err error) {
	switch {
	case c.DurationMS != nil:
		return time.Duration(*c.DurationMS) * time.Millisecond, true, nil
	case c.Duration != "":
		d, err := time.ParseDuration(c.Duration)
		return d, true, err
	}
	return 0, false, nil
}

type ForEachConfig struct {
	Items       string `json:"items"`
	Concurrency int    `json:"concurrency,omitempty"`
	MaxItems    int    `json:"max_items,omitempty"`
	OnItemError string `json:"on_item_error,omitempty"` // fail (default) | continue
}

const (
	DefaultForEachMaxItems = 1000
	HardForEachMaxItems    = 10000
	DefaultForEachConc     = 10
)

type TransformConfig struct {
	Expression string `json:"expression,omitempty"`
	Output     any    `json:"output,omitempty"`
}

type MergeConfig struct{}

type StopConfig struct {
	Status  string `json:"status,omitempty"` // succeeded (default) | failed
	Message string `json:"message,omitempty"`
}

type HTTPConfig struct {
	Method          string            `json:"method,omitempty"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers,omitempty"`
	Query           map[string]string `json:"query,omitempty"`
	Body            any               `json:"body,omitempty"`
	FollowRedirects bool              `json:"follow_redirects,omitempty"`
	ExpectStatus    []int             `json:"expect_status,omitempty"` // default: any 2xx
	// IdempotencyHeader, when set, sends the task's stable idempotency key in
	// that header so receivers can deduplicate redelivered requests.
	IdempotencyHeader string `json:"idempotency_header,omitempty"`
}

type LogConfig struct {
	Message string         `json:"message"`
	Level   string         `json:"level,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type EmailConfig struct {
	To      string `json:"to"`
	From    string `json:"from,omitempty"`
	Subject string `json:"subject"`
	Body    string `json:"body,omitempty"`
}

type SubWorkflowConfig struct {
	WorkflowID string `json:"workflow_id"`
	Input      any    `json:"input,omitempty"`
}

// DecodeConfig strictly decodes a node's config into the typed struct for its
// type. Unknown fields are rejected.
func DecodeConfig(n *Node) (any, error) {
	var dst any
	switch n.Type {
	case TypeWebhookTrigger:
		dst = &WebhookConfig{}
	case TypeScheduleTrigger:
		dst = &ScheduleConfig{}
	case TypeManualTrigger:
		dst = &ManualConfig{}
	case TypeCondition:
		dst = &ConditionConfig{}
	case TypeDelay:
		dst = &DelayConfig{}
	case TypeForEach:
		dst = &ForEachConfig{}
	case TypeTransform:
		dst = &TransformConfig{}
	case TypeMerge:
		dst = &MergeConfig{}
	case TypeStop:
		dst = &StopConfig{}
	case TypeHTTPRequest:
		dst = &HTTPConfig{}
	case TypeLog:
		dst = &LogConfig{}
	case TypeEmail:
		dst = &EmailConfig{}
	case TypeSubWorkflow:
		dst = &SubWorkflowConfig{}
	case TypeItemInput:
		dst = &struct{}{}
	default:
		return nil, fmt.Errorf("unknown node type %q", n.Type)
	}
	raw := n.Config
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return nil, fmt.Errorf("invalid %s config: %w", n.Type, err)
	}
	return dst, nil
}

// ExprChecker lets validation verify expressions without importing the
// expression package. Refs are the node ids referenced by the source.
type ExprChecker interface {
	CheckExpression(src string) (refs []string, err error)
	CheckTemplate(src string) (refs []string, err error)
	CheckCron(spec, tz string) error
}

// checkConfig validates a node's config, returning problems and the node ids
// referenced by its expressions/templates.
func checkConfig(n *Node, ck ExprChecker) (problems []string, refs []string) {
	cfg, err := DecodeConfig(n)
	if err != nil {
		return []string{err.Error()}, nil
	}
	addRefs := func(r []string) { refs = append(refs, r...) }
	expr := func(field, src string) {
		if strings.TrimSpace(src) == "" {
			problems = append(problems, field+" is required")
			return
		}
		if ck != nil {
			r, err := ck.CheckExpression(src)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", field, err))
			}
			addRefs(r)
		}
	}
	tmpl := func(field, src string) {
		if ck == nil || !strings.Contains(src, "{{") {
			return
		}
		r, err := ck.CheckTemplate(src)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", field, err))
		}
		addRefs(r)
	}
	walk := func(field string, v any) { walkStrings(field, v, tmpl) }

	switch c := cfg.(type) {
	case *ScheduleConfig:
		if strings.TrimSpace(c.Cron) == "" {
			problems = append(problems, "cron is required")
		} else if ck != nil {
			if err := ck.CheckCron(c.Cron, c.Timezone); err != nil {
				problems = append(problems, "cron: "+err.Error())
			}
		}
	case *ConditionConfig:
		expr("expression", c.Expression)
	case *DelayConfig:
		set := 0
		if c.DurationMS != nil {
			set++
		}
		if c.Duration != "" {
			set++
		}
		if c.Until != "" {
			set++
		}
		if set != 1 {
			problems = append(problems, "exactly one of duration_ms, duration or until is required")
			break
		}
		if d, ok, err := c.Wait(); err != nil {
			problems = append(problems, "duration: "+err.Error())
		} else if ok && (d < 0 || d > MaxDelay) {
			problems = append(problems, "delay must be between 0 and 30 days")
		}
		if c.Until != "" {
			expr("until", c.Until)
		}
	case *ForEachConfig:
		expr("items", c.Items)
		if c.Concurrency < 0 || c.Concurrency > 100 {
			problems = append(problems, "concurrency must be between 0 and 100")
		}
		if c.MaxItems < 0 || c.MaxItems > HardForEachMaxItems {
			problems = append(problems, fmt.Sprintf("max_items must be between 0 and %d", HardForEachMaxItems))
		}
		if c.OnItemError != "" && c.OnItemError != OnErrorFail && c.OnItemError != OnErrorContinue {
			problems = append(problems, "on_item_error must be fail or continue")
		}
	case *TransformConfig:
		switch {
		case c.Expression != "" && c.Output != nil:
			problems = append(problems, "set only one of expression or output")
		case c.Expression != "":
			expr("expression", c.Expression)
		case c.Output != nil:
			walk("output", c.Output)
		default:
			problems = append(problems, "one of expression or output is required")
		}
	case *StopConfig:
		if c.Status != "" && c.Status != "succeeded" && c.Status != "failed" {
			problems = append(problems, "status must be succeeded or failed")
		}
		tmpl("message", c.Message)
	case *HTTPConfig:
		switch c.Method {
		case "", "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
		default:
			problems = append(problems, "unsupported method "+c.Method)
		}
		if strings.TrimSpace(c.URL) == "" {
			problems = append(problems, "url is required")
		} else if !strings.Contains(c.URL, "{{") {
			if u, err := url.Parse(c.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				problems = append(problems, "url must be an absolute http(s) URL")
			}
		}
		tmpl("url", c.URL)
		for k, v := range c.Headers {
			tmpl("headers."+k, v)
		}
		for k, v := range c.Query {
			tmpl("query."+k, v)
		}
		walk("body", c.Body)
		if h := c.IdempotencyHeader; h != "" && !validHeaderName(h) {
			problems = append(problems, "idempotency_header is not a valid header name")
		}
		for _, s := range c.ExpectStatus {
			if s < 100 || s > 599 {
				problems = append(problems, "expect_status contains an invalid status code")
			}
		}
	case *LogConfig:
		switch c.Level {
		case "", "debug", "info", "warn", "error":
		default:
			problems = append(problems, "level must be debug, info, warn or error")
		}
		if c.Message == "" {
			problems = append(problems, "message is required")
		}
		tmpl("message", c.Message)
		walk("fields", c.Fields)
	case *EmailConfig:
		if c.To == "" || c.Subject == "" {
			problems = append(problems, "to and subject are required")
		}
		tmpl("to", c.To)
		tmpl("subject", c.Subject)
		tmpl("body", c.Body)
	case *SubWorkflowConfig:
		if c.WorkflowID == "" {
			problems = append(problems, "workflow_id is required")
		}
		walk("input", c.Input)
	}
	return problems, refs
}

func walkStrings(path string, v any, fn func(field, s string)) {
	switch t := v.(type) {
	case string:
		fn(path, t)
	case []any:
		for i, e := range t {
			walkStrings(fmt.Sprintf("%s[%d]", path, i), e, fn)
		}
	case map[string]any:
		for k, e := range t {
			walkStrings(path+"."+k, e, fn)
		}
	}
}

// Sources returns the expression and template strings a node's config contains.
func Sources(n *Node) (exprs, tmpls []string) {
	cfg, err := DecodeConfig(n)
	if err != nil {
		return nil, nil
	}
	tm := func(v any) { walkStrings("", v, func(_, s string) { tmpls = append(tmpls, s) }) }
	switch c := cfg.(type) {
	case *ConditionConfig:
		exprs = append(exprs, c.Expression)
	case *DelayConfig:
		if c.Until != "" {
			exprs = append(exprs, c.Until)
		}
	case *ForEachConfig:
		exprs = append(exprs, c.Items)
	case *TransformConfig:
		if c.Expression != "" {
			exprs = append(exprs, c.Expression)
		}
		tm(c.Output)
	case *StopConfig:
		tm(c.Message)
	case *HTTPConfig:
		tm(c.URL)
		for _, v := range c.Headers {
			tm(v)
		}
		for _, v := range c.Query {
			tm(v)
		}
		tm(c.Body)
	case *LogConfig:
		tm(c.Message)
		tm(c.Fields)
	case *EmailConfig:
		tm(c.To)
		tm(c.From)
		tm(c.Subject)
		tm(c.Body)
	case *SubWorkflowConfig:
		tm(c.Input)
	case *ScheduleConfig:
		tm(c.Payload)
	}
	return exprs, tmpls
}

func validHeaderName(h string) bool {
	if h == "" || len(h) > 100 {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
