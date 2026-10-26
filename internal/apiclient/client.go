// Package apiclient is a small Go client for BeeVM's HTTP API. The beevm CLI uses it, and so
// will the benchmark tool.
package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/om8P/BeeVM/internal/apitypes"
)

// Client talks to one BeeVM server with one API key.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// New creates a client. baseURL looks like "http://127.0.0.1:8080".
func New(baseURL, key string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), key: key, http: &http.Client{}}
}

// APIError is an error answer from the server.
type APIError struct {
	Status   int
	Message  string
	ExitCode *int   // set when a function failed (502)
	Stderr   string // the failed function's log (502)
}

func (e *APIError) Error() string { return fmt.Sprintf("server answered %d: %s", e.Status, e.Message) }

// IsNotFound reports whether err is a 404 from the server.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// do sends a request and returns the response if its status is 2xx; other statuses become *APIError.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ae := &APIError{Status: resp.StatusCode}
	var eb apitypes.Error
	if json.Unmarshal(data, &eb) == nil && eb.Error != "" {
		ae.Message, ae.ExitCode, ae.Stderr = eb.Error, eb.ExitCode, eb.Stderr
	} else {
		ae.Message = strings.TrimSpace(string(data))
		if ae.Message == "" {
			ae.Message = http.StatusText(resp.StatusCode)
		}
	}
	return nil, ae
}

func decodeBody[T any](resp *http.Response) (T, error) {
	defer resp.Body.Close()
	var v T
	err := json.NewDecoder(resp.Body).Decode(&v)
	return v, err
}

func fnPath(name string) string { return "/api/v1/functions/" + url.PathEscape(name) }

// Deploy uploads a function. The code is streamed, not loaded into memory. It reports whether
// the function was newly created (false means an existing one was replaced).
func (c *Client) Deploy(ctx context.Context, name string, code io.Reader, cfg apitypes.FunctionConfig) (apitypes.Function, bool, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if cfg != (apitypes.FunctionConfig{}) {
				fw, err := mw.CreateFormField("config")
				if err != nil {
					return err
				}
				if err := json.NewEncoder(fw).Encode(cfg); err != nil {
					return err
				}
			}
			fw, err := mw.CreateFormFile("code", name)
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, code); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err) // nil closes normally
	}()

	resp, err := c.do(ctx, http.MethodPut, fnPath(name), pr, mw.FormDataContentType())
	if err != nil {
		pr.CloseWithError(err) // stop the writer goroutine if the request ended early
		return apitypes.Function{}, false, err
	}
	created := resp.StatusCode == http.StatusCreated
	f, err := decodeBody[apitypes.Function](resp)
	return f, created, err
}

// List returns the caller's functions.
func (c *Client) List(ctx context.Context) ([]apitypes.Function, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/functions", nil, "")
	if err != nil {
		return nil, err
	}
	l, err := decodeBody[apitypes.FunctionList](resp)
	return l.Functions, err
}

// Get returns one function.
func (c *Client) Get(ctx context.Context, name string) (apitypes.Function, error) {
	resp, err := c.do(ctx, http.MethodGet, fnPath(name), nil, "")
	if err != nil {
		return apitypes.Function{}, err
	}
	return decodeBody[apitypes.Function](resp)
}

// Delete removes a function.
func (c *Client) Delete(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, fnPath(name), nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// InvokeResult is a successful call.
type InvokeResult struct {
	Output     json.RawMessage
	VMID       string
	BootMS     int64
	FunctionMS int64
	TotalMS    int64 // inside the server; the caller measures its own round trip
}

// Invoke runs a function. payload must be valid JSON (nil or empty means no input). A function
// that fails comes back as *APIError with Status 502, ExitCode and Stderr set.
func (c *Client) Invoke(ctx context.Context, name string, payload []byte) (InvokeResult, error) {
	resp, err := c.do(ctx, http.MethodPost, "/invoke/"+url.PathEscape(name), strings.NewReader(string(payload)), "application/json")
	if err != nil {
		return InvokeResult{}, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return InvokeResult{}, err
	}
	ms := func(h string) int64 { n, _ := strconv.ParseInt(resp.Header.Get(h), 10, 64); return n }
	return InvokeResult{
		Output: json.RawMessage(out), VMID: resp.Header.Get(apitypes.HeaderVMID),
		BootMS: ms(apitypes.HeaderBootMS), FunctionMS: ms(apitypes.HeaderFunctionMS), TotalMS: ms(apitypes.HeaderTotalMS),
	}, nil
}
