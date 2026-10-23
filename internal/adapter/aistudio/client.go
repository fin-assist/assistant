package aistudio

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const officialEndpoint = "https://ai.api.cloud.yandex.net/v1/responses"

// ErrRequestTooLarge is a preflight rejection: no key, HTTP request or text
// callback has been used. It is safe for the caller to shorten unsent history.
var ErrRequestTooLarge = errors.New("AI Studio request limit exceeded")

// Options supplies explicit policy from the owning generation assembly. No real
// requests are issued by construction, and no key value is retained by the client.
type Options struct {
	Environment string
	Endpoint    string
	FolderID    string
	Model       string
	Key         KeySource
	Limits      Limits
}

// Client owns a reusable HTTP transport. Call Close after all admitted streams stop.
type Client struct {
	options   Options
	http      *http.Client
	transport *http.Transport
}

// New checks the destination before any credentials can be obtained. Production
// and stage accept only AI Studio; dev additionally accepts loopback fake servers.
func New(options Options) (*Client, error) {
	if options.Endpoint == "" {
		options.Endpoint = officialEndpoint
	}
	if !validEndpoint(options.Environment, options.Endpoint) || !validIdentity(options.FolderID, options.Model) || options.Key == nil || !validLimits(options.Limits) {
		return nil, errors.New("invalid AI Studio configuration")
	}
	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DialContext:        (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:  true,
		MaxIdleConns:       32, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &Client{options: options, transport: transport, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Close releases idle connections; the owner cancels and drains active requests first.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

func validLimits(l Limits) bool {
	return l.RequestBytes > 0 && l.ResponseBytes > 0 && l.ResponseBytes < math.MaxInt64 && l.EventBytes > 0 && int64(l.EventBytes) <= l.ResponseBytes && l.ArgumentBytes > 0 && int64(l.ArgumentBytes) <= l.ResponseBytes && l.OutputItems > 0
}
func validIdentity(folder, model string) bool {
	if folder == "" || strings.TrimSpace(folder) != folder {
		return false
	}
	for _, r := range folder {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return model == "gpt://"+folder+"/aliceai-llm" || model == "gpt://"+folder+"/aliceai-llm-flash"
}
func validEndpoint(env, endpoint string) bool {
	if env != "dev" && env != "stage" && env != "prod" {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Path != "/v1/responses" || u.RawPath != "" {
		return false
	}
	if u.Scheme == "https" && u.Host == "ai.api.cloud.yandex.net" {
		return true
	}
	if env != "dev" || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

type wireRequest struct {
	Request
	Model             string `json:"model"`
	Stream            bool   `json:"stream"`
	Store             bool   `json:"store"`
	ParallelToolCalls bool   `json:"parallel_tool_calls"`
	Truncation        string `json:"truncation"`
}

// Stream performs exactly one POST. Network failures, 429/5xx, malformed streams
// and partial output are never retried here: another paid generation may duplicate work.
// The caller's deadline must bound the entire generation, including later steps.
func (c *Client) Stream(ctx context.Context, request Request, emit func(Delta) error) (Result, error) {
	return c.StreamGuarded(ctx, request, emit, func([]byte) error { return nil })
}

// StreamGuarded reserves the exact serialized wire request before reading a key
// or sending HTTP. The guard must not modify or retain the borrowed body slice.
// A guard error is returned unchanged; an accepted reservation is never released
// here, including after an uncertain network outcome. The body is marshaled once.
func (c *Client) StreamGuarded(ctx context.Context, request Request, emit func(Delta) error, reserve func([]byte) error) (Result, error) {
	if _, ok := ctx.Deadline(); !ok {
		return Result{}, errors.New("AI Studio request requires a deadline")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if emit == nil || reserve == nil || request.MaxOutputTokens < 16 || request.MaxToolCalls < 0 || len(request.Input) == 0 {
		return Result{}, errors.New("invalid AI Studio request")
	}
	remaining := c.options.Limits.RequestBytes
	if int64(len(request.Instructions)) > remaining {
		return Result{}, ErrRequestTooLarge
	}
	remaining -= int64(len(request.Instructions))
	for _, raw := range append(append([]json.RawMessage{}, request.Input...), request.Tools...) {
		if int64(len(raw)) > remaining {
			return Result{}, ErrRequestTooLarge
		}
		remaining -= int64(len(raw))
		var object map[string]json.RawMessage
		if decodeJSON(raw, &object) != nil || object == nil {
			return Result{}, errors.New("invalid AI Studio request item")
		}
	}
	body, err := json.Marshal(wireRequest{Request: request, Model: c.options.Model, Stream: true, Store: false, ParallelToolCalls: true, Truncation: "disabled"})
	if err != nil {
		return Result{}, errors.New("invalid AI Studio request")
	}
	if int64(len(body)) > c.options.Limits.RequestBytes {
		return Result{}, ErrRequestTooLarge
	}
	if err := reserve(body); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	key, err := c.options.Key.Key(ctx)
	if err != nil {
		return Result{}, contextOr(ctx, "AI Studio credential unavailable")
	}
	if !validKey(key) {
		return Result{}, errors.New("invalid AI Studio credential")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.options.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, errors.New("invalid AI Studio request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("OpenAI-Project", c.options.FolderID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-data-logging-enabled", "false")
	response, err := c.http.Do(req)
	if err != nil {
		return Result{}, contextOr(ctx, "AI Studio transport unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Result{}, errors.New("AI Studio request rejected")
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" || response.Header.Get("Content-Encoding") != "" {
		return Result{}, errors.New("invalid AI Studio response format")
	}
	// A limit+1 reader exposes over-limit streams without unbounded allocation.
	limited := &io.LimitedReader{R: response.Body, N: c.options.Limits.ResponseBytes + 1}
	result, err := parseStream(ctx, limited, c.options.Limits, emit)
	if limited.N <= 0 {
		return Result{}, errors.New("AI Studio response limit exceeded")
	}
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
func validKey(key string) bool {
	if len(key) == 0 || len(key) > 8192 {
		return false
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
func contextOr(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New(message)
}
