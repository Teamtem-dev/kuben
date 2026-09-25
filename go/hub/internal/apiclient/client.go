// Package apiclient is a client of the Kuben API for the `kuben` CLI
// (crates/kuben-api/src/client.rs): plain net/http over the frozen JSON
// contract, authenticated with an API token.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/version"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

const (
	// timeout bounds a request until its response starts.
	timeout = 30 * time.Second
	// maxBody is the largest JSON body read (logs of ten pods are the
	// biggest).
	maxBody = 32 << 20
)

// Error is why a call failed: [APIError] or [TransportError].
//
//sumtype:decl
type Error interface {
	error
	clientError()
}

// APIError says that the server answered with a problem.
type APIError struct {
	// Status is the HTTP status.
	Status int
	// Code is the problem's code, or the status when the answer was no
	// problem document.
	Code string
	// Detail is the problem's detail, when it had one.
	Detail opt.Val[string]
}

// TransportError says that the server could not be reached, or its answer
// not read.
type TransportError struct {
	Reason string
}

// Error is `detail (code)`, the status standing in for a missing detail.
func (e APIError) Error() string {
	return e.Detail.Or(strconv.Itoa(e.Status)) + " (" + e.Code + ")"
}

func (e TransportError) Error() string { return e.Reason }

func (APIError) clientError()       {}
func (TransportError) clientError() {}

// errTimedOut is the cause of a request that got no response in time.
var errTimedOut = errors.New("timed out") //nolint:gochecknoglobals // sentinel

// Client is a client of one Kuben server. Printing it never shows the
// token.
type Client struct {
	base  string
	token string
	// transport carries the requests; tests set it through export_test.go.
	transport http.RoundTripper
}

// New is a client of the server at url (`https://kuben.example.com`),
// authenticated with token. Requests go through the proxy the environment
// names (HTTPS_PROXY, NO_PROXY) and trust the system's root certificates.
func New(url, token string) Client {
	return Client{
		base:      strings.TrimRight(url, "/") + "/api/v1",
		token:     token,
		transport: http.DefaultTransport,
	}
}

// String names the server and leaves the token out.
func (c Client) String() string { return fmt.Sprintf("Client { base: %q, .. }", c.base) }

// GoString is String: %#v must not print the token either.
func (c Client) GoString() string { return c.String() }

// Format prints String for every verb, so no verb prints the token.
func (c Client) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String()) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// cancelOnClose ends the request's context once its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel(nil)
	return err //nolint:wrapcheck // the body's own error
}

// send makes one request; a success is returned with its body unread, any
// other status becomes an [APIError].
func (c Client) send(ctx context.Context, method, path string, body []byte, headers map[string]string) (*http.Response, error) {
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(timeout, func() { cancel(errTimedOut) })
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, TransportError{Reason: err.Error()}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", "kuben-cli/"+version.Version)
	request.Header.Set("Accept", "application/json, text/event-stream")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	// Like the Rust client, redirects are not followed: they are answers.
	httpClient := http.Client{
		Transport:     c.transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := httpClient.Do(request)
	timer.Stop()
	if err != nil {
		cancel(nil)
		if errors.Is(context.Cause(ctx), errTimedOut) {
			return nil, TransportError{Reason: errTimedOut.Error()}
		}
		return nil, TransportError{Reason: err.Error()}
	}
	response.Body = cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	answer, err := read(response)
	if err != nil {
		answer = nil
	}
	code, detail, ok := problemOf(answer)
	if !ok {
		code = strconv.Itoa(response.StatusCode)
	}
	return nil, APIError{Status: response.StatusCode, Code: code, Detail: detail}
}

// problemOf reads a problem document as serde read Problem: code, title and
// status are required, detail is optional.
func problemOf(body []byte) (string, opt.Val[string], bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return "", opt.None[string](), false
	}
	for _, required := range []string{"code", "title", "status"} {
		if _, ok := fields[required]; !ok {
			return "", opt.None[string](), false
		}
	}
	var p struct {
		Code   string          `json:"code"`
		Title  string          `json:"title"`
		Status uint16          `json:"status"`
		Detail opt.Val[string] `json:"detail"`
	}
	if json.Unmarshal(body, &p) != nil {
		return "", opt.None[string](), false
	}
	return p.Code, p.Detail, true
}

// read is the whole body, at most maxBody bytes, closed afterwards.
func read(response *http.Response) ([]byte, error) {
	defer response.Body.Close() //nolint:errcheck // fully read or given up
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, TransportError{Reason: err.Error()}
	}
	if len(body) > maxBody {
		return nil, TransportError{Reason: "length limit exceeded"}
	}
	return body, nil
}

// call sends body as JSON (none when nil) and reads the answer as T.
func call[T any](ctx context.Context, c Client, method, path string, body any, headers map[string]string) (T, error) {
	var zero T
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return zero, TransportError{Reason: err.Error()}
		}
		payload = encoded
	}
	response, err := c.send(ctx, method, path, payload, headers)
	if err != nil {
		return zero, err
	}
	answer, err := read(response)
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(answer, &out); err != nil {
		return zero, TransportError{Reason: fmt.Sprintf("unexpected answer from %s: %v", path, err)}
	}
	return out, nil
}

func get[T any](ctx context.Context, c Client, path string) (T, error) {
	return call[T](ctx, c, http.MethodGet, path, nil, nil)
}

// Me is who the token belongs to: the /me document as it came, read with
// wire.DecodeAny (objects are map[string]any).
func (c Client) Me(ctx context.Context) (any, error) {
	const path = "/me"
	response, err := c.send(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	answer, err := read(response)
	if err != nil {
		return nil, err
	}
	me, err := wire.DecodeAny(answer)
	if err != nil {
		return nil, TransportError{Reason: fmt.Sprintf("unexpected answer from %s: %v", path, err)}
	}
	return me, nil
}

// Projects is the projects the token can see.
func (c Client) Projects(ctx context.Context) ([]ProjectDto, error) {
	return get[[]ProjectDto](ctx, c, "/projects")
}

// Environments is the environments of project.
func (c Client) Environments(ctx context.Context, project string) ([]EnvironmentDto, error) {
	return get[[]EnvironmentDto](ctx, c, "/projects/"+project+"/environments")
}

// Apps is the apps of an environment.
func (c Client) Apps(ctx context.Context, project, environment string) ([]AppDto, error) {
	return get[[]AppDto](ctx, c, "/projects/"+project+"/environments/"+environment+"/apps")
}

// App is an app with its pods.
func (c Client) App(ctx context.Context, app AppPath) (AppDetail, error) {
	return get[AppDetail](ctx, c, app.api())
}

// Releases is the app's history, newest first.
func (c Client) Releases(ctx context.Context, app AppPath) ([]ReleaseDto, error) {
	return get[[]ReleaseDto](ctx, c, app.api()+"/releases")
}

// Doctor is the app's Doctor.
func (c Client) Doctor(ctx context.Context, app AppPath) (DoctorReport, error) {
	return get[DoctorReport](ctx, c, app.api()+"/doctor")
}

// Deploy starts a deployment; the same idempotencyKey returns the same run.
func (c Client) Deploy(ctx context.Context, app AppPath, request StartDeploymentRequest, idempotencyKey string) (DeploymentDto, error) {
	return call[DeploymentDto](ctx, c, http.MethodPost, app.api()+"/deployments", request,
		map[string]string{"idempotency-key": idempotencyKey})
}

// Deployment is where a run stands.
func (c Client) Deployment(ctx context.Context, app AppPath, run uuid.UUID) (DeploymentDto, error) {
	return get[DeploymentDto](ctx, c, app.api()+"/deployments/"+run.String())
}

// Rollback returns the app to revision.
func (c Client) Rollback(ctx context.Context, app AppPath, revision int64) (AppDto, error) {
	return call[AppDto](ctx, c, http.MethodPost, app.api()+"/rollback", rollbackRequest{Revision: revision}, nil)
}

// Logs is the log lines of the app's pods.
func (c Client) Logs(ctx context.Context, app AppPath, options LogOptions) ([]PodLogs, error) {
	return get[[]PodLogs](ctx, c, app.api()+"/logs"+options.query(false))
}

// FollowEvent is what a followed log sends: [LineEvent] or [EndEvent].
//
//sumtype:decl
type FollowEvent interface {
	followEvent()
}

// LineEvent is a new log line.
type LineEvent struct{ LogLine }

// EndEvent says that a log (or the whole stream) ended.
type EndEvent struct{ LogEnd }

func (LineEvent) followEvent() {}
func (EndEvent) followEvent()  {}

// FollowLogs is new log lines as they are written, until the server ends
// the stream. The request is made before FollowLogs returns; the events
// are read while the sequence is ranged over, and the stream is closed when
// the loop ends. An event the client cannot read is an error in the
// sequence; so is a broken connection, after which the sequence ends.
func (c Client) FollowLogs(ctx context.Context, app AppPath, options LogOptions) (iter.Seq2[FollowEvent, error], error) {
	path := app.api() + "/logs" + options.query(true)
	response, err := c.send(ctx, http.MethodGet, path, nil, nil) //nolint:bodyclose // closed by the returned iterator
	if err != nil {
		return nil, err
	}
	return func(yield func(FollowEvent, error) bool) {
		defer response.Body.Close() //nolint:errcheck // the stream is done with
		var parser SSEParser
		var pending []byte
		chunk := make([]byte, 32<<10)
		for {
			n, readErr := response.Body.Read(chunk)
			if n > 0 {
				pending = append(pending, chunk[:n]...)
				valid := validUTF8Prefix(pending)
				text := string(pending[:valid])
				pending = append([]byte(nil), pending[valid:]...)
				for _, event := range parser.Push(text) {
					followed, known, err := decodeEvent(event)
					if !known {
						continue
					}
					if !yield(followed, err) {
						return
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				return
			}
			if readErr != nil {
				yield(nil, TransportError{Reason: readErr.Error()})
				return
			}
		}
	}, nil
}

// decodeEvent reads a `line` or `end` event; other events are not known.
func decodeEvent(event SSEEvent) (FollowEvent, bool, error) {
	var err error
	var followed FollowEvent
	switch event.Event {
	case "line":
		var line LogLine
		err = json.Unmarshal([]byte(event.Data), &line)
		followed = LineEvent{line}
	case "end":
		var end LogEnd
		err = json.Unmarshal([]byte(event.Data), &end)
		followed = EndEvent{end}
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, true, TransportError{Reason: "unexpected event: " + err.Error()}
	}
	return followed, true, nil
}
