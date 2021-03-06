// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package transport

import (
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPConstructor(t *testing.T) {
	tests := []struct {
		opts   HTTPOptions
		errMsg string
	}{
		{
			opts:   HTTPOptions{TargetService: "svc"},
			errMsg: errNoURLs.Error(),
		},
		{
			opts:   HTTPOptions{URLs: []string{"http://localhost"}},
			errMsg: errMissingTarget.Error(),
		},
		{
			opts: HTTPOptions{TargetService: "svc", URLs: []string{"http://localhost"}},
		},
	}

	for _, tt := range tests {
		got, err := HTTP(tt.opts)
		if tt.errMsg != "" {
			if assert.Error(t, err, "HTTP(%v) should fail", tt.opts) {
				assert.Contains(t, err.Error(), tt.errMsg, "Unexpected error for HTTP(%v)", tt.opts)
			}
			continue
		}

		if assert.NoError(t, err, "HTTP(%v) should not fail", tt.opts) {
			assert.NotNil(t, got, "HTTP(%v) returned nil Transport", tt.opts)
		}
	}
}

func TestHTTPCall(t *testing.T) {
	timeoutCtx, _ := context.WithTimeout(context.Background(), 3*time.Second)

	tests := []struct {
		ctx    context.Context
		r      *Request
		errMsg string
		ttlMin time.Duration
		ttlMax time.Duration
	}{
		{
			ctx:    context.Background(),
			r:      &Request{Method: "method", Body: []byte{1, 2, 3}},
			ttlMin: time.Second,
			ttlMax: time.Second,
		},
		{
			ctx:    timeoutCtx,
			r:      &Request{Method: "method", Body: []byte{1, 2, 3}},
			ttlMin: 3*time.Second - 100*time.Millisecond,
			ttlMax: 3 * time.Second,
		},
		{
			ctx: context.Background(),
			r: &Request{
				Method:  "method",
				Body:    []byte{1, 2, 3},
				Headers: map[string]string{"fail": "kill_conn"},
			},
			errMsg: "EOF",
		},
		{
			ctx: context.Background(),
			r: &Request{
				Method:  "method",
				Body:    []byte{1, 2, 3},
				Headers: map[string]string{"fail": "bad_req"},
			},
			errMsg: "non-success response code: 400",
		},
		{
			ctx: context.Background(),
			r: &Request{
				Method:  "method",
				Body:    []byte{1, 2, 3},
				Headers: map[string]string{"fail": "flush_and_kill"},
			},
			errMsg: "unexpected EOF",
		},
	}

	lastReq := struct {
		url     *url.URL
		headers http.Header
		body    []byte
	}{}

	svr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error

		// Test hooks to make the request fail.
		switch f := r.Header.Get("fail"); f {
		case "bad_req":
			w.WriteHeader(http.StatusBadRequest)
			return
		case "server_err":
			w.WriteHeader(http.StatusInternalServerError)
			return
		case "flush_and_kill":
			io.WriteString(w, "some data")
			flusher := w.(http.Flusher)
			flusher.Flush()
			fallthrough
		case "kill_conn":
			hijacker := w.(http.Hijacker)
			conn, _, _ := hijacker.Hijack()
			conn.Close()
			return
		}

		lastReq.url = r.URL
		lastReq.headers = r.Header
		lastReq.body, err = ioutil.ReadAll(r.Body)
		if err != nil {
			io.WriteString(w, "failed")
			return
		}

		w.Header().Set("Custom-Header", "ok")
		io.WriteString(w, "ok")
	}))
	defer svr.Close()

	transport, err := HTTP(HTTPOptions{
		URLs:          []string{svr.URL + "/rpc"},
		SourceService: "source",
		TargetService: "target",
	})
	require.NoError(t, err, "Failed to create HTTP transport")

	for _, tt := range tests {
		got, err := transport.Call(tt.ctx, tt.r)
		if tt.errMsg != "" {
			if assert.Error(t, err, "Call(%v, %v) should fail", tt.ctx, tt.r) {
				assert.Contains(t, err.Error(), tt.errMsg, "Unexpected error for Call(%v, %v)", tt.ctx, tt.r)
			}
			continue
		}

		if !assert.NoError(t, err, "Call(%v, %v) shouldn't fail", tt.ctx, tt.r) {
			continue
		}

		if !assert.Equal(t, []byte("ok"), got.Body) {
			continue
		}

		assert.Equal(t, lastReq.url.Path, "/rpc", "Path mismatch")
		assert.Equal(t, lastReq.headers.Get("RPC-Service"), "target", "Service header mismatch")
		assert.Equal(t, lastReq.headers.Get("RPC-Caller"), "source", "Caller header mismatch")
		assert.Equal(t, lastReq.headers.Get("RPC-Procedure"), tt.r.Method, "Method header mismatch")

		ttlMS, err := strconv.Atoi(lastReq.headers.Get("Context-TTL-MS"))
		if assert.NoError(t, err, "Failed to parse TTLms header: %v", lastReq.headers.Get("YARPC-TTLms")) {
			gotTTL := time.Duration(ttlMS) * time.Millisecond
			assert.True(t, gotTTL >= tt.ttlMin && gotTTL <= tt.ttlMax,
				"Got TTL %v out of range [%v,%v]", gotTTL, tt.ttlMin, tt.ttlMax)
		}

		assert.Equal(t, "ok", got.Headers["Custom-Header"], "Header mismatch")
		assert.Equal(t, lastReq.body, tt.r.Body, "Body mismatch")
	}
}
