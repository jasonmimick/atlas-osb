// Copyright 2020 MongoDB Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mongodbrealm // import "go.mongodb.org/atlas/mongodbrealm"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"

	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/go-querystring/query"
	"github.com/mongodb/go-client-mongodb-atlas/mongodbatlas"
	"github.com/pkg/errors"
)

const (
	userAgent           = "go-mongodbrealm"
	jsonMediaType       = "application/json"
	gzipMediaType       = "application/gzip"
	realmAppsPath       = "groups/%s/apps"
	realmDefaultBaseURL = "https://realm.mongodb.com/api/admin/v3.0/"
	realmLoginPath      = "auth/providers/mongodb-cloud/login"
	realmSessionPath    = "auth/session"
)

type Doer interface {
	Do(context.Context, *http.Request, interface{}) (*Response, error)
}

type Completer interface {
	OnRequestCompleted(RequestCompletionCallback)
}

type RequestDoer interface {
	Doer
	Completer
	NewRequest(context.Context, string, string, interface{}) (*http.Request, error)
}

type GZipRequestDoer interface {
	Doer
	Completer
	NewGZipRequest(context.Context, string, string) (*http.Request, error)
}

// Client manages communication with mongodbrealm v1.0 API
type Client struct {
	client    *http.Client
	BaseURL   *url.URL
	UserAgent string

	// Services used for communicating with the API
	RealmApps   RealmAppsService
	RealmValues RealmValuesService

	auth *RealmAuth

	onRequestCompleted RequestCompletionCallback
}

// RequestCompletionCallback defines the type of the request callback function
type RequestCompletionCallback func(*http.Request, *http.Response)

type service struct {
	Client RequestDoer
}

// Response is a mongodbrealm response. This wraps the standard http.Response returned from mongodbrealm API.
type Response struct {
	*http.Response

	// Links that were returned with the response.
	Links []*mongodbatlas.Link `json:"links"`
}

// ListOptions specifies the optional parameters to List methods that
// support pagination.
type ListOptions struct {
	// For paginated result sets, page of results to retrieve.
	PageNum int `url:"pageNum,omitempty"`

	// For paginated result sets, the number of results to include per page.
	ItemsPerPage int `url:"itemsPerPage,omitempty"`
}

// ErrorResponse reports the error caused by an API request.
type ErrorResponse struct {
	// HTTP response that caused this error
	Response *http.Response
	// The error code, which is simply the HTTP status code.
	ErrorCode int `json:"Error"`

	// A short description of the error, which is simply the HTTP status phrase.
	Reason string `json:"reason"`

	// A more detailed description of the error.
	Detail string `json:"detail,omitempty"`
}

func (resp *Response) getLinkByRef(ref string) *mongodbatlas.Link {
	for i := range resp.Links {
		if resp.Links[i].Rel == ref {
			return resp.Links[i]
		}
	}
	return nil
}

// IsLastPage returns true if the current page is the last page
func (resp *Response) IsLastPage() bool {
	return resp.getLinkByRef("next") == nil
}

// CurrentPage gets the current page for list pagination request.
func (resp *Response) CurrentPage() (int, error) {
	//link, err := resp.getCurrentPageLink()
	//if err != nil {
	//	return 0, err
	//}

	//pageNumStr, err := link.getHrefQueryParam("pageNum")
	//if err != nil {
	//	return 0, err
	//}
	pageNumStr := "0"
	pageNum, err := strconv.Atoi(pageNumStr)
	if err != nil {
		return 0, fmt.Errorf("error getting current page: %s", err)
	}

	return pageNum, nil
}

// NewClient returns a new mongodbrealm API Client
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	baseURL, _ := url.Parse(realmDefaultBaseURL)

	c := &Client{client: httpClient,
		BaseURL:   baseURL,
		UserAgent: userAgent,
		auth:      &RealmAuth{},
	}

	c.RealmApps = &RealmAppsServiceOp{Client: c}
	c.RealmValues = &RealmValuesServiceOp{Client: c}

	return c
}

// ClientOpt are options for New.
type ClientOpt func(*Client) error

// New returns a new mongodbrealm API client instance.
func New(ctx context.Context, httpClient *http.Client, opts ...ClientOpt) (*Client, error) {
	c := NewClient(httpClient)

	for _, opt := range opts {
		if opt == nil {
			panic("nil option passed, check your arguments")
		}

		if err := opt(c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// SetBaseURL is a client option for setting the base URL.
func SetBaseURL(bu string) ClientOpt {
	return func(c *Client) error {
		u, err := url.Parse(bu)
		if err != nil {
			return err
		}

		c.BaseURL = u
		return nil
	}
}

// SetUserAgent is a client option for setting the user agent.
func SetUserAgent(ua string) ClientOpt {
	return func(c *Client) error {
		c.UserAgent = fmt.Sprintf("%s %s", ua, c.UserAgent)
		return nil
	}
}

func SetAPIAuth(ctx context.Context, pub string, priv string) ClientOpt {
	return func(c *Client) error {
		return c.obtainToken(ctx, pub, priv)
	}
}

func (c *Client) refreshToken(ctx context.Context) error {
	req, err := c.NewRequest(ctx, http.MethodPost, realmSessionPath, nil)
	if err != nil {
		return errors.Wrap(err, "cannot create login request")
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.auth.RefreshToken))

	resp, err := c.do(ctx, req, c.auth)
	if err != nil {
		return errors.Wrap(err, "cannot do refresh request")
	}

	return errors.Wrap(CheckResponse(resp.Response), "unexpected response")
}

func (c *Client) obtainToken(ctx context.Context, publicKey string, privateKey string) error {
	data := map[string]interface{}{
		"username": publicKey,
		"apiKey":   privateKey,
	}

	loginReq, err := c.NewRequest(ctx, http.MethodPost, realmLoginPath, data)
	if err != nil {
		return errors.Wrapf(err, "cannot create login request (public key %q)", publicKey)
	}

	_, err = c.do(ctx, loginReq, c.auth)
	if err != nil {
		return errors.Wrapf(err, "cannot do login request (public key %q)", publicKey)
	}

	return nil
}

// NewRequest creates an API request. A relative URL can be provided in urlStr, which will be resolved to the
// BaseURL of the Client. Relative URLS should always be specified without a preceding slash. If specified, the
// value pointed to by body is JSON encoded and included in as the request body.
func (c *Client) NewRequest(ctx context.Context, method, urlStr string, body interface{}) (*http.Request, error) {
	if !strings.HasSuffix(c.BaseURL.Path, "/") {
		return nil, fmt.Errorf("base URL must have a trailing slash, but %q does not", c.BaseURL)
	}
	u, err := c.BaseURL.Parse(urlStr)
	if err != nil {
		return nil, err
	}
	var buf io.Reader
	if body != nil {
		if buf, err = c.newEncodedBody(body); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest(method, u.String(), buf)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", jsonMediaType)
	}
	req.Header.Add("Accept", jsonMediaType)

	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	return req, nil
}

// newEncodedBody returns an ReadWriter object containing the body of the http request
func (c *Client) newEncodedBody(body interface{}) (io.Reader, error) {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(body)
	return buf, err
}

// NewGZipRequest creates an API request that accepts gzip. A relative URL can be provided in urlStr, which will be resolved to the
// BaseURL of the Client. Relative URLS should always be specified without a preceding slash.
func (c *Client) NewGZipRequest(ctx context.Context, method, urlStr string) (*http.Request, error) {
	rel, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	u := c.BaseURL.ResolveReference(rel)

	req, err := http.NewRequest(method, u.String(), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Accept", gzipMediaType)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	return req, nil
}

// OnRequestCompleted sets the DO API request completion callback
func (c *Client) OnRequestCompleted(rc RequestCompletionCallback) {
	c.onRequestCompleted = rc
}

func (c *Client) Do(ctx context.Context, req *http.Request, v interface{}) (*Response, error) {
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.auth.AccessToken))
	resp, err := c.do(ctx, req, v)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			_ = resp.Body.Close()

			err = c.refreshToken(ctx)
			if err != nil {
				return nil, errors.Wrap(err, "cannot refresh auth token")
			}

			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.auth.AccessToken))
			return c.do(ctx, req, v)
		}

		return nil, err
	}

	return resp, nil
}

// Do sends an API request and returns the API response. The API response is JSON decoded and stored in the value
// pointed to by v, or returned as an error if an API error has occurred. If v implements the io.Writer interface,
// the raw response will be written to v, without attempting to decode it.
func (c *Client) do(ctx context.Context, req *http.Request, v interface{}) (*Response, error) {
	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		// If we got an error, and the context has been canceled,
		// the context's error is probably more useful.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		return nil, err
	}

	if c.onRequestCompleted != nil {
		c.onRequestCompleted(req, resp)
	}

	defer func() {
		if rerr := resp.Body.Close(); err == nil {
			err = rerr
		}
	}()

	response := &Response{Response: resp}

	err = CheckResponse(resp)
	if err != nil {
		return response, err
	}

	if v != nil {
		if w, ok := v.(io.Writer); ok {
			_, err = io.Copy(w, resp.Body)
			if err != nil {
				return nil, err
			}
		} else {
			decErr := json.NewDecoder(resp.Body).Decode(v)
			if decErr == io.EOF {
				decErr = nil // ignore EOF errors caused by empty response body
			}
			if decErr != nil {
				err = decErr
			}
		}
	}

	return response, err
}

func (r *ErrorResponse) Error() string {
	return fmt.Sprintf("%v %v: %d (request %q) %v",
		r.Response.Request.Method, r.Response.Request.URL, r.Response.StatusCode, r.Reason, r.Detail)
}

// CheckResponse checks the API response for errors, and returns them if present. A response is considered an
// error if it has a status code outside the 200 range. API error responses are expected to have either no response
// body, or a JSON response body that maps to ErrorResponse. Any other response body will be silently ignored.
func CheckResponse(r *http.Response) error {
	if c := r.StatusCode; c >= 200 && c <= 299 {
		return nil
	}

	data, err := ioutil.ReadAll(r.Body)
	if err == nil && len(data) > 0 {
		return errors.New(string(data))
	}

	return nil
}

func setListOptions(s string, opt interface{}) (string, error) {
	v := reflect.ValueOf(opt)

	if v.Kind() == reflect.Ptr && v.IsNil() {
		return s, nil
	}

	origURL, err := url.Parse(s)
	if err != nil {
		return s, err
	}

	origValues := origURL.Query()

	newValues, err := query.Values(opt)
	if err != nil {
		return s, err
	}

	for k, v := range newValues {
		origValues[k] = v
	}

	origURL.RawQuery = origValues.Encode()
	return origURL.String(), nil
}
