// Copyright 2020 Matthew Holt
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

// Package replaceresponseonce registers a Caddy HTTP handler module that
// performs replacements on response bodies.
package replaceresponseonce

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/text/transform"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler manipulates response bodies by replacing the first
// occurrence of each search string.
type Handler struct {
	// The list of replacements to make on the response body.
	Replacements []*Replacement `json:"replacements,omitempty"`

	// If true, perform replacements in a streaming fashion.
	// This is more memory-efficient but can remove the
	// Content-Length header since knowing the correct length
	// is impossible without buffering, and getting it wrong
	// can break HTTP/2 streams.
	Stream bool `json:"stream,omitempty"`

	// Only run replacements on responses that match against this ResponseMmatcher.
	Matcher *caddyhttp.ResponseMatcher `json:"match,omitempty"`

	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.replace_response_once",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision implements caddy.Provisioner.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()

	if len(h.Replacements) == 0 {
		return fmt.Errorf("no replacements configured")
	}

	// prepare each replacement
	for i, repl := range h.Replacements {
		if repl.Search == "" {
			return fmt.Errorf("replacement %d: no search configured", i)
		}
		if len(repl.Search) > maxSearchSize {
			return fmt.Errorf("replacement %d: search is longer than %d bytes", i, maxSearchSize)
		}
	}

	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.Stream {
		if c := h.logger.Check(zapcore.DebugLevel, "streaming body replacement"); c != nil {
			c.Write(
				zap.Any("replacements", h.Replacements),
				zap.Object("request", caddyhttp.LoggableHTTPRequest{Request: r}),
			)
		}

		tr := h.makeTransformer(r)

		// don't buffer response body, perform streaming replacement
		fw := &replaceWriter{
			ResponseWriterWrapper: &caddyhttp.ResponseWriterWrapper{ResponseWriter: w},
			tr:                    tr,
			handler:               h,
		}
		err := next.ServeHTTP(fw, r)
		if err != nil {
			return err
		}
		// only close if there is no error; see PR #21
		// as of May 2023, Close() only flushes remaining bytes, but
		// this ends up calling WriteHeader() even if we don't want that
		fw.Close()

		return nil
	}

	// get a buffer to hold the response body
	respBuf := bufPool.Get().(*bytes.Buffer)
	respBuf.Reset()
	defer bufPool.Put(respBuf)

	// set up the response recorder
	shouldBuf := func(status int, headers http.Header) bool {
		if h.Matcher != nil {
			return h.Matcher.Match(status, headers)
		} else {
			// Always replace if no matcher is specified
			return true
		}
	}
	rec := caddyhttp.NewResponseRecorder(w, respBuf, shouldBuf)

	// collect the response from upstream
	err := next.ServeHTTP(rec, r)
	if err != nil {
		return err
	}
	if !rec.Buffered() {
		// Skipped, no need to replace
		if c := h.logger.Check(zapcore.DebugLevel, "not buffering body; skipping replacement"); c != nil {
			c.Write(
				zap.Int("response_status", rec.Status()),
				zap.Object("request", caddyhttp.LoggableHTTPRequest{Request: r}),
			)
		}
		return nil
	}

	if c := h.logger.Check(zapcore.DebugLevel, "buffered body replacement"); c != nil {
		c.Write(
			zap.Any("replacements", h.Replacements),
			zap.Object("request", caddyhttp.LoggableHTTPRequest{Request: r}),
		)
	}

	tr := h.makeTransformer(r)

	// TODO: could potentially use transform.Append here with a pooled byte slice as buffer?
	result, _, err := transform.Bytes(tr, rec.Buffer().Bytes())
	if err != nil {
		return err
	}

	// make sure length is correct, otherwise bad things can happen
	if w.Header().Get("Content-Length") != "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(result)))
	}

	if status := rec.Status(); status > 0 {
		w.WriteHeader(status)
	}
	w.Write(result)

	return nil
}

func (h *Handler) makeTransformer(req *http.Request) transform.Transformer {
	reqReplacer := req.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	transforms := make([]transform.Transformer, len(h.Replacements))
	for i, repl := range h.Replacements {
		transforms[i] = newReplaceOnce(
			[]byte(reqReplacer.ReplaceKnown(repl.Search, "")),
			[]byte(reqReplacer.ReplaceKnown(repl.Replace, "")),
		)
	}
	return transform.Chain(transforms...)
}

// Replacement replaces the first occurrence of a substring.
type Replacement struct {
	// The substring to search for. Required. Placeholders are expanded per request.
	Search string `json:"search,omitempty"`

	// The replacement string/value. Placeholders are expanded per request.
	Replace string `json:"replace"`
}

// replaceWriter is used for streaming response body replacement. It
// ensures the Content-Length header is removed and writes to tw,
// which should be a transform writer that performs replacements.
type replaceWriter struct {
	*caddyhttp.ResponseWriterWrapper
	wroteHeader bool
	tw          io.WriteCloser
	tr          transform.Transformer
	handler     *Handler
}

func (fw *replaceWriter) WriteHeader(status int) {
	if fw.wroteHeader {
		return
	}
	fw.wroteHeader = true

	if fw.handler.Matcher == nil || fw.handler.Matcher.Match(status, fw.ResponseWriterWrapper.Header()) {
		// we don't know the length after replacements since
		// we're not buffering it all to find out
		fw.Header().Del("Content-Length")
		fw.tw = transform.NewWriter(fw.ResponseWriterWrapper, fw.tr)
	}

	fw.ResponseWriterWrapper.WriteHeader(status)
}

func (fw *replaceWriter) Write(d []byte) (int, error) {
	if !fw.wroteHeader {
		fw.WriteHeader(http.StatusOK)
	}

	if fw.tw != nil {
		return fw.tw.Write(d)
	} else {
		return fw.ResponseWriterWrapper.Write(d)
	}
}

func (fw *replaceWriter) Close() error {
	if fw.tw != nil {
		// Close if we have a transform writer, the underlying one does not need to be closed.
		return fw.tw.Close()
	}
	return nil
}

// maxSearchSize bounds a search string. Until the first match, the transformer
// holds back up to len(search)-1 bytes between writes, and transform.Writer can
// carry at most a few KB of unconsumed input.
const maxSearchSize = 2048

// replaceOnce is a transformer that replaces only the first occurrence of
// search with replace. Each response gets its own instance.
//
// Until the match, it writes everything except the last len(search)-1 bytes and
// returns ErrShortSrc, so the transform package hands those bytes back with the
// next chunk and a match split across writes is still found. After the match, it
// copies input through. It honors the size of dst everywhere, returning
// ErrShortDst with the progress made, so the caller can continue.
type replaceOnce struct {
	search  []byte
	replace []byte
	done    bool
	// The part of replace not yet written because dst was full.
	pending []byte
}

func newReplaceOnce(search, replace []byte) *replaceOnce {
	// A search that a placeholder expanded to nothing matches nothing.
	return &replaceOnce{search: search, replace: replace, done: len(search) == 0}
}

func (r *replaceOnce) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for {
		if len(r.pending) > 0 {
			n := copy(dst[nDst:], r.pending)
			nDst += n
			r.pending = r.pending[n:]
			if len(r.pending) > 0 {
				return nDst, nSrc, transform.ErrShortDst
			}
		}

		rest := src[nSrc:]
		if r.done {
			n := copy(dst[nDst:], rest)
			nDst += n
			nSrc += n
			if n < len(rest) {
				return nDst, nSrc, transform.ErrShortDst
			}
			return nDst, nSrc, nil
		}

		if i := bytes.Index(rest, r.search); i >= 0 {
			n := copy(dst[nDst:], rest[:i])
			nDst += n
			nSrc += n
			if n < i {
				return nDst, nSrc, transform.ErrShortDst
			}
			nSrc += len(r.search)
			r.done = true
			r.pending = r.replace
			continue
		}

		// No match yet. Keep back a tail that may be the start of a match.
		keep := 0
		if !atEOF {
			keep = min(len(r.search)-1, len(rest))
		}
		safe := len(rest) - keep
		n := copy(dst[nDst:], rest[:safe])
		nDst += n
		nSrc += n
		if n < safe {
			return nDst, nSrc, transform.ErrShortDst
		}
		if keep > 0 {
			return nDst, nSrc, transform.ErrShortSrc
		}
		return nDst, nSrc, nil
	}
}

func (r *replaceOnce) Reset() {
	r.done = len(r.search) == 0
	r.pending = nil
}

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)

	_ http.ResponseWriter   = (*replaceWriter)(nil)
	_ transform.Transformer = (*replaceOnce)(nil)
)
