package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/namesarnav/synapse/internal/engine"
	"github.com/namesarnav/synapse/internal/expressions"
	"github.com/namesarnav/synapse/internal/workflow"
)

// HTTPOptions configure the HTTP request node.
type HTTPOptions struct {
	// AllowPrivate disables the SSRF guard. Only for tests and trusted
	// development setups.
	AllowPrivate   bool
	DefaultTimeout time.Duration
	MaxBodyBytes   int64
	MaxConcurrent  int
}

type httpExec struct {
	o      HTTPOptions
	client *http.Client
	follow *http.Client
	sem    chan struct{}
}

// ErrBlockedAddress is returned when a request targets a non-public address.
var ErrBlockedAddress = errors.New("destination address is not allowed")

// NewHTTPExecutor builds the HTTP node executor.
func NewHTTPExecutor(o HTTPOptions) Executor {
	if o.DefaultTimeout <= 0 {
		o.DefaultTimeout = 30 * time.Second
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 1 << 20
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 50
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !o.AllowPrivate {
		// Checking at connect time, after DNS resolution, defeats rebinding.
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || blockedIP(ip) {
				return ErrBlockedAddress
			}
			return nil
		}
	}
	tr := &http.Transport{DialContext: dialer.DialContext, Proxy: nil, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second, DisableCompression: false}
	e := &httpExec{o: o, sem: make(chan struct{}, o.MaxConcurrent)}
	e.client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	e.follow = &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("stopped after 5 redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("redirect to unsupported scheme")
		}
		return nil
	}}
	return e
}

// blockedPrefixes are special-purpose ranges not covered by the netip predicates.
var blockedPrefixes = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/23", "2001:db8::/32", "2002::/16",
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (e *httpExec) Execute(ctx context.Context, in Input) (any, error) {
	c := in.Config.(*workflow.HTTPConfig)
	render := func(s string) (string, error) {
		v, err := expressions.RenderTemplate(s, in.Env)
		if err != nil {
			return "", exprFailure(err)
		}
		return expressions.ToString(v), nil
	}
	rawURL, err := render(c.URL)
	if err != nil {
		return nil, err
	}
	u, perr := url.Parse(rawURL)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, &engine.NodeError{Code: engine.CodeConfig, Message: "url must be an absolute http(s) URL"}
	}
	if len(c.Query) > 0 {
		q := u.Query()
		for k, v := range c.Query {
			rv, err := render(v)
			if err != nil {
				return nil, err
			}
			q.Set(k, rv)
		}
		u.RawQuery = q.Encode()
	}
	method := c.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	hasBody := false
	if c.Body != nil && method != http.MethodGet && method != http.MethodHead {
		resolved, err := expressions.Resolve(expressions.Normalize(c.Body), in.Env)
		if err != nil {
			return nil, exprFailure(err)
		}
		var buf bytes.Buffer
		if s, ok := resolved.(string); ok {
			buf.WriteString(s)
		} else if err := json.NewEncoder(&buf).Encode(resolved); err != nil {
			return nil, &engine.NodeError{Code: engine.CodeConfig, Message: "body is not JSON encodable"}
		}
		body, hasBody = &buf, true
	}

	timeout := e.o.DefaultTimeout
	if in.Node.TimeoutMS > 0 {
		timeout = time.Duration(in.Node.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, &engine.NodeError{Code: engine.CodeConfig, Message: err.Error()}
	}
	if hasBody {
		if _, isStr := c.Body.(string); !isStr {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	for k, v := range c.Headers {
		rv, err := render(v)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(rv, "\r\n") {
			return nil, &engine.NodeError{Code: engine.CodeConfig, Message: "header contains a line break"}
		}
		req.Header.Set(k, rv)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Synapse/1.0")
	}
	if c.IdempotencyHeader != "" && in.IdempotencyKey != "" {
		req.Header.Set(c.IdempotencyHeader, in.IdempotencyKey)
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, ctxError(ctx)
	}

	client := e.client
	if c.FollowRedirects {
		client = e.follow
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, e.classify(ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, e.o.MaxBodyBytes+1))
	if err != nil {
		return nil, e.classify(ctx, err)
	}
	if int64(len(data)) > e.o.MaxBodyBytes {
		return nil, &engine.NodeError{Code: engine.CodeHTTP, Message: fmt.Sprintf("response body exceeds %d bytes", e.o.MaxBodyBytes)}
	}

	out := map[string]any{
		"status":      float64(resp.StatusCode),
		"headers":     flattenHeaders(resp.Header),
		"body":        decodeBody(resp.Header.Get("Content-Type"), data),
		"duration_ms": float64(time.Since(start).Milliseconds()),
	}
	if !statusOK(resp.StatusCode, c.ExpectStatus) {
		retry := resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 408
		return out, &engine.NodeError{Code: engine.CodeHTTP, Message: fmt.Sprintf("unexpected status %d", resp.StatusCode), Retryable: retry}
	}
	return out, nil
}

func statusOK(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code < 300
	}
	for _, e := range expect {
		if e == code {
			return true
		}
	}
	return false
}

func (e *httpExec) classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctxError(ctx)
	}
	if errors.Is(err, ErrBlockedAddress) {
		return &engine.NodeError{Code: engine.CodeConfig, Message: "request blocked: destination address is not allowed"}
	}
	return &engine.NodeError{Code: engine.CodeNetwork, Message: sanitizeNetErr(err), Retryable: true}
}

func ctxError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &engine.NodeError{Code: engine.CodeTimeout, Message: "request timed out", Retryable: true}
	}
	return ctx.Err()
}

// sanitizeNetErr strips the URL (which may embed secrets) from transport errors.
func sanitizeNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func flattenHeaders(h http.Header) map[string]any {
	out := make(map[string]any, len(h))
	for k, v := range h {
		if strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		out[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return out
}

func decodeBody(contentType string, data []byte) any {
	if len(data) == 0 {
		return nil
	}
	if strings.Contains(contentType, "json") {
		var v any
		if json.Unmarshal(data, &v) == nil {
			return expressions.Normalize(v)
		}
	}
	return string(data)
}
