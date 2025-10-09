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
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/icholy/replace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/text/transform"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler manipulates response bodies by performing
// substring or regex replacements.
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
		ID:  "http.handlers.replace_response",
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
			return fmt.Errorf("replacement %d: no search or search_regexp configured", i)
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
		if repl.re != nil {
			tr := replace.RegexpIndexFunc(repl.re, func(src []byte, index []int) []byte {
				template := reqReplacer.ReplaceKnown(repl.Replace, "")
				return repl.re.Expand(nil, []byte(template), src, index)
			})
			// See: https://github.com/icholy/replace/issues/5#issuecomment-949757616
			tr.MaxMatchSize = 2048
			transforms[i] = tr
		} else {
			transforms[i] = newReplaceOnce(
				[]byte(reqReplacer.ReplaceKnown(repl.Search, "")),
				[]byte(reqReplacer.ReplaceKnown(repl.Replace, "")),
			)
		}
	}
	return transform.Chain(transforms...)
}

// Replacement is either a substring or regular expression replacement
// to perform; precisely one must be specified, not both.
type Replacement struct {
	// A substring to search for. Mutually exclusive with search_regexp.
	Search string `json:"search,omitempty"`

	// The replacement string/value. Required.
	Replace string `json:"replace"`

	re *regexp.Regexp
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

// replaceOnce is a transformer that replaces only the first occurrence of a substring.
type replaceOnce struct {
	search      []byte
	replace     []byte
	replaced    int32 // 1 if replaced, 0 otherwise. Use atomic for safe concurrent access.
	searchIndex int
}

func newReplaceOnce(search, replace []byte) *replaceOnce {
	return &replaceOnce{
		search:  search,
		replace: replace,
	}
}

func (r *replaceOnce) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	if atomic.LoadInt32(&r.replaced) == 1 || len(r.search) == 0 {
		// passthrough if already replaced or search is empty
		n := copy(dst, src)
		return n, n, nil
	}

	// Combine pending search buffer with new source
	var searchBuffer []byte
	if r.searchIndex > 0 {
		searchBuffer = make([]byte, r.searchIndex+len(src))
		copy(searchBuffer, r.search[:r.searchIndex])
		copy(searchBuffer[r.searchIndex:], src)
	} else {
		searchBuffer = src
	}

	// Look for the search string
	idx := bytes.Index(searchBuffer, r.search)

	if idx != -1 {
		// Found it.
		if len(dst) < idx+len(r.replace) {
			return 0, 0, transform.ErrShortDst
		}

		// Copy bytes before the match
		copy(dst, searchBuffer[:idx])
		nDst += idx

		// Copy replacement
		copy(dst[nDst:], r.replace)
		nDst += len(r.replace)

		// Calculate how much of original src was consumed
		// This is tricky because searchBuffer is a mix of old and new src
		consumedInTotal := idx + len(r.search)
		if consumedInTotal > r.searchIndex {
			nSrc = consumedInTotal - r.searchIndex
		} else {
			// The entire match was within the previous partial match buffer.
			// This case should be handled carefully, but for now we assume it doesn't happen
			// if search buffer logic is correct.
			nSrc = 0
		}

		atomic.StoreInt32(&r.replaced, 1)
		r.searchIndex = 0 // reset partial match tracking

		// Copy remaining part of src to dst
		remainingSrc := src[nSrc:]
		n := copy(dst[nDst:], remainingSrc)
		nDst += n
		nSrc += n

		return nDst, nSrc, nil
	}

	// Not found. We might have a partial match at the end of src.
	// We need to keep the part of src that could be a prefix of search.
	// And write out the rest.

	// The maximum overlap between the end of searchBuffer and the start of r.search
	maxOverlap := 0
	for i := 1; i < len(searchBuffer) && i < len(r.search); i++ {
		if bytes.HasSuffix(searchBuffer, r.search[:i]) {
			maxOverlap = i
		}
	}

	if !atEOF && maxOverlap > 0 {
		// We have a potential partial match at the end.
		// Write out bytes before the potential partial match.
		writeCount := len(searchBuffer) - maxOverlap
		if writeCount > 0 {
			if len(dst) < writeCount {
				return 0, 0, transform.ErrShortDst
			}
			copy(dst, searchBuffer[:writeCount])
			nDst = writeCount
		}

		// Update searchIndex to reflect the new partial match state
		r.searchIndex = maxOverlap

		// Calculate nSrc. All of src has been processed and either written or buffered.
		nSrc = len(src)

		return nDst, nSrc, nil
	}

	// No full match, and no partial match at the end (or atEOF).
	// Write everything out.
	if len(dst) < len(searchBuffer) {
		return 0, 0, transform.ErrShortDst
	}
	copy(dst, searchBuffer)
	nDst = len(searchBuffer)
	nSrc = len(src)
	r.searchIndex = 0

	if atEOF {
		return nDst, nSrc, nil
	}

	return nDst, nSrc, nil
}

func (r *replaceOnce) Reset() {
	atomic.StoreInt32(&r.replaced, 0)
	r.searchIndex = 0
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
