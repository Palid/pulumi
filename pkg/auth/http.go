// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const expiryMargin = 30 * time.Second

// ErrCannotRefresh leaves a rejected request's response for the caller to handle.
var ErrCannotRefresh = errors.New("credentials cannot be refreshed")

type refreshReason string

const (
	credentialsExpired  refreshReason = "expired"
	credentialsRejected refreshReason = "rejected"
)

type httpCredentials struct {
	accessToken string
	headers     http.Header
	expiresAt   *time.Time
}

type credentialSource struct {
	refresh func(context.Context, refreshReason) (httpCredentials, error)

	mu         sync.Mutex
	current    httpCredentials
	generation int
	refreshErr error
	refreshing *pendingRefresh
}

type pendingRefresh struct {
	done chan struct{}
	err  error
}

// HTTPAuth holds the credentials for one backend. Clients that share it also share refreshed credentials.
type HTTPAuth struct {
	origin  *url.URL
	token   *credentialSource
	sources []*credentialSource
}

// NewHTTPAuth creates backend authentication with an access token and an optional refresh operation.
func NewHTTPAuth(backendURL, accessToken string, refresh func(context.Context) (string, error)) *HTTPAuth {
	origin, _ := url.Parse(backendURL)
	source := &credentialSource{current: httpCredentials{accessToken: accessToken}}
	if refresh != nil {
		source.refresh = func(ctx context.Context, _ refreshReason) (httpCredentials, error) {
			token, err := refresh(ctx)
			return httpCredentials{accessToken: token}, err
		}
	}
	a := &HTTPAuth{origin: origin, sources: []*credentialSource{source}}
	if accessToken != "" || refresh != nil {
		a.token = source
	}
	return a
}

// WithFallback uses fallback's token if a supplies none, and includes both sets of headers.
// Both inputs must belong to the same backend. A nil input contributes no credentials.
func (a *HTTPAuth) WithFallback(fallback *HTTPAuth) *HTTPAuth {
	if a == nil {
		return fallback
	}
	if fallback == nil || fallback == a {
		return a
	}
	if !sameOrigin(a.origin, fallback.origin) {
		panic("cannot combine credentials for different backend origins")
	}
	result := &HTTPAuth{origin: a.origin, token: a.token}
	if result.token == nil {
		result.token = fallback.token
	}
	result.sources = slices.Clone(a.sources)
	for _, source := range fallback.sources {
		if !slices.Contains(result.sources, source) {
			result.sources = append(result.sources, source)
		}
	}
	return result
}

// AccessToken returns the current backend token, or an empty string if none is available.
func (a *HTTPAuth) AccessToken() string {
	if a == nil || a.token == nil {
		return ""
	}
	credentials, _ := a.token.credentials()
	return credentials.accessToken
}

func (s *credentialSource) credentials() (httpCredentials, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.generation
}

func (s *credentialSource) renew(ctx context.Context, generation int, reason refreshReason) error {
	s.mu.Lock()
	if s.generation != generation {
		err := s.refreshErr
		s.mu.Unlock()
		return err
	}
	if pending := s.refreshing; pending != nil {
		s.mu.Unlock()
		select {
		case <-pending.done:
			return pending.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	pending := &pendingRefresh{done: make(chan struct{})}
	s.refreshing = pending
	s.mu.Unlock()

	credentials, err := s.refresh(ctx, reason)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.current = credentials
	}
	s.generation++
	s.refreshErr = err
	s.refreshing = nil
	pending.err = err
	close(pending.done)
	return err
}

type operation struct {
	auth      *HTTPAuth
	refreshed map[*credentialSource]bool
	err       error
}

type (
	operationKey          struct{}
	withoutAccessTokenKey struct{}
)

// WithHTTPAuth starts a backend API operation. Its requests and retries share one refresh per source.
func WithHTTPAuth(ctx context.Context, a *HTTPAuth) context.Context {
	return context.WithValue(ctx, operationKey{}, &operation{auth: a, refreshed: map[*credentialSource]bool{}})
}

// WithoutAccessToken retains an operation's headers and refresh limits for a token-exchange request.
func WithoutAccessToken(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutAccessTokenKey{}, true)
}

// Transport authenticates backend API operations marked by WithHTTPAuth. Other requests pass through unchanged.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &httpTransport{base: base}
}

type httpTransport struct {
	base http.RoundTripper
}

func (t *httpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	op, _ := req.Context().Value(operationKey{}).(*operation)
	if op == nil || op.auth == nil {
		return t.base.RoundTrip(req)
	}
	if op.err != nil {
		closeRequestBody(req)
		return nil, op.err
	}
	if !sameOrigin(op.auth.origin, req.URL) {
		req = req.Clone(req.Context())
		req.Header.Del("Authorization")
		return t.base.RoundTrip(req)
	}

	token := op.auth.token
	if req.Context().Value(withoutAccessTokenKey{}) == true ||
		strings.HasPrefix(req.Header.Get("Authorization"), "update-token ") {
		token = nil
	}
	type snapshot struct {
		source *credentialSource
		httpCredentials
		generation int
	}
	var snapshots []snapshot
	for _, source := range op.auth.sources {
		credentials, generation := source.credentials()
		if source == token || len(credentials.headers) != 0 {
			snapshots = append(snapshots, snapshot{source, credentials, generation})
		}
	}
	refresh := func(s *snapshot, reason refreshReason) error {
		op.refreshed[s.source] = true
		op.err = s.source.renew(req.Context(), s.generation, reason)
		s.httpCredentials, s.generation = s.source.credentials()
		return op.err
	}
	send := func() (*http.Response, error) {
		authorized := req.Clone(req.Context())
		for _, s := range snapshots {
			if s.source == token && s.accessToken != "" {
				authorized.Header.Set("Authorization", "token "+s.accessToken)
			}
			for name, values := range s.headers {
				for _, value := range values {
					authorized.Header.Add(name, value)
				}
			}
		}
		return t.base.RoundTrip(authorized)
	}

	for i := range snapshots {
		s := &snapshots[i]
		if s.source.refresh != nil && !op.refreshed[s.source] && s.expiresAt != nil &&
			!s.expiresAt.After(time.Now().Add(expiryMargin)) {
			if err := refresh(s, credentialsExpired); err != nil {
				closeRequestBody(req)
				return nil, err
			}
		}
	}
	resp, err := send()
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	for i := range snapshots {
		if err != nil || resp.StatusCode != http.StatusUnauthorized || !replayable {
			break
		}
		s := &snapshots[i]
		if s.source.refresh == nil || op.refreshed[s.source] {
			continue
		}
		if err := refresh(s, credentialsRejected); err != nil {
			if errors.Is(err, ErrCannotRefresh) {
				op.err = nil
				return resp, nil
			}
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		if req.GetBody != nil {
			req = req.Clone(req.Context())
			if req.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		resp, err = send()
	}
	return resp, err
}

func closeRequestBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
