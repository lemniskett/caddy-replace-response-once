package replaceresponseonce

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"golang.org/x/text/transform"
)

func bufferedOnce(search, repl, body string) (string, error) {
	out, _, err := transform.Bytes(newReplaceOnce([]byte(search), []byte(repl)), []byte(body))
	return string(out), err
}

func streamedOnce(search, repl string, chunks []string) (string, error) {
	var out bytes.Buffer
	w := transform.NewWriter(&out, newReplaceOnce([]byte(search), []byte(repl)))
	for _, c := range chunks {
		if n, err := w.Write([]byte(c)); err != nil || n != len(c) {
			return out.String(), err
		}
	}
	err := w.Close()
	return out.String(), err
}

// split cuts s into pieces of n bytes.
func split(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

func TestReplaceOnce(t *testing.T) {
	big := strings.Repeat("a", 10000)
	tail := strings.Repeat("b", 50000)
	for _, c := range []struct{ name, search, repl, body, want string }{
		{"first only", "Foo", "Bar", "a Foo b Foo", "a Bar b Foo"},
		{"longer replacement", "Foo", "Barbazqux", "a Foo hello world", "a Barbazqux hello world"},
		{"shorter replacement", "Foo", "X", "a Foo hello", "a X hello"},
		{"empty replacement", "Foo", "", "aFoob", "ab"},
		{"at start", "Foo", "Bar", "Foo!", "Bar!"},
		{"at end", "Foo", "Bar", "!Foo", "!Bar"},
		{"no match", "Foo", "Bar", "nothing here", "nothing here"},
		{"partial at end", "Foo", "Bar", "a Fo", "a Fo"},
		{"empty body", "Foo", "Bar", "", ""},
		{"overlapping prefix", "aab", "X", "aaab", "aX"},
		{"large body", "</head>", "<script>x</script></head>", "<head>" + big + "</head>" + tail, "<head>" + big + "<script>x</script></head>" + tail},
		{"large replacement", "X", strings.Repeat("r", 20000), "aXb", "a" + strings.Repeat("r", 20000) + "b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, err := bufferedOnce(c.search, c.repl, c.body); err != nil || got != c.want {
				t.Fatalf("buffered: got %d bytes, want %d, err %v", len(got), len(c.want), err)
			}
			// Every chunk size, including ones that end inside the search string.
			for _, n := range []int{1, 2, 3, 7, 4096, 32 * 1024, len(c.body) + 1} {
				if got, err := streamedOnce(c.search, c.repl, split(c.body, n)); err != nil || got != c.want {
					t.Fatalf("streamed in %d-byte writes: got %d bytes, want %d, err %v", n, len(got), len(c.want), err)
				}
			}
		})
	}
}

func TestEmptySearchPassesThrough(t *testing.T) {
	// A placeholder can expand to nothing at request time.
	if got, err := bufferedOnce("", "X", "abc"); err != nil || got != "abc" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestChainReplacesEachOnce(t *testing.T) {
	tr := transform.Chain(newReplaceOnce([]byte("a"), []byte("1")), newReplaceOnce([]byte("b"), []byte("2")))
	out, _, err := transform.Bytes(tr, []byte("abab"))
	if err != nil || string(out) != "12ab" {
		t.Fatalf("got %q, err %v", out, err)
	}
}

func serve(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	h.logger = zap.NewNop()
	req := httptest.NewRequest("GET", "/", nil)
	repl := caddy.NewReplacer()
	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl))
	rec := httptest.NewRecorder()
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(200)
		// Write the way a proxy does: in chunks.
		for _, c := range split(body, 32*1024) {
			if _, err := io.WriteString(w, c); err != nil {
				return err
			}
		}
		return nil
	})
	if err := h.ServeHTTP(rec, req, next); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestHandlerBufferedAndStreamed(t *testing.T) {
	body := "<html><head>" + strings.Repeat("x", 100000) + "</head><body></head></body></html>"
	want := strings.Replace(body, "</head>", "<meta></head>", 1)
	for _, stream := range []bool{false, true} {
		h := &Handler{Replacements: []*Replacement{{Search: "</head>", Replace: "<meta></head>"}}, Stream: stream}
		rec := serve(t, h, body)
		if rec.Body.String() != want {
			t.Fatalf("stream=%v: got %d bytes, want %d", stream, rec.Body.Len(), len(want))
		}
		if !stream && rec.Header().Get("Content-Length") != "" && rec.Header().Get("Content-Length") != strconv.Itoa(len(want)) {
			t.Fatalf("buffered Content-Length %s, want %d", rec.Header().Get("Content-Length"), len(want))
		}
		if stream && rec.Header().Get("Content-Length") != "" {
			t.Fatal("streamed response kept Content-Length")
		}
	}
}

func TestModuleAndDirective(t *testing.T) {
	if id := (Handler{}).CaddyModule().ID; id != "http.handlers.replace_response_once" {
		t.Fatalf("module ID %s", id)
	}
	d := caddyfile.NewTestDispenser(`replace_response_once {
		stream
		Foo Bar
		A B
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if !h.Stream || len(h.Replacements) != 2 || h.Replacements[0].Search != "Foo" || h.Replacements[1].Replace != "B" {
		t.Fatalf("parsed %+v", h)
	}
	if err := (&Handler{Replacements: []*Replacement{{Search: strings.Repeat("s", maxSearchSize+1)}}}).Provision(caddy.Context{}); err == nil {
		t.Fatal("an oversized search was accepted")
	}
}
