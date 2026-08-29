package builtins_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Luo-root/pulse/toolset/builtins"
)

type stubSearcher struct {
	hits []builtins.SearchHit
	err  error
}

func (s stubSearcher) Search(context.Context, string, int) ([]builtins.SearchHit, error) {
	return s.hits, s.err
}

type stubAsker struct{ ans string }

func (s stubAsker) Ask(context.Context, builtins.Question) (string, error) {
	return s.ans, nil
}

func TestWebFetchHTTPAndRejectSchemes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><p>Hello fetch</p><script>secret()</script></body></html>`))
	}))
	defer srv.Close()

	_, reg, cleanup := setup(t, builtins.Options{Root: t.TempDir(), HTTPClient: srv.Client()})
	defer cleanup()

	out := call(t, reg, "web_fetch", map[string]any{"url": srv.URL})
	if !strings.Contains(out, "Hello fetch") || strings.Contains(out, "secret()") {
		t.Fatalf("fetch=%q", out)
	}
	msg := callErr(t, reg, "web_fetch", map[string]any{"url": "file:///etc/passwd"})
	if !strings.Contains(msg, "scheme") {
		t.Fatalf("%s", msg)
	}
	msg = callErr(t, reg, "web_fetch", map[string]any{"url": "http://169.254.169.254/latest/meta-data"})
	if !strings.Contains(msg, "metadata") && !strings.Contains(msg, "link-local") {
		t.Fatalf("%s", msg)
	}
}

func TestWebFetchRedirectToMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()

	_, reg, cleanup := setup(t, builtins.Options{Root: t.TempDir(), HTTPClient: srv.Client()})
	defer cleanup()

	msg := callErr(t, reg, "web_fetch", map[string]any{"url": srv.URL})
	if !strings.Contains(msg, "metadata") && !strings.Contains(msg, "link-local") {
		t.Fatalf("redirect to metadata should be refused, got %s", msg)
	}
}

func TestWebFetchBlockPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should-not-see"))
	}))
	defer srv.Close()

	_, reg, cleanup := setup(t, builtins.Options{
		Root:         t.TempDir(),
		HTTPClient:   srv.Client(),
		BlockPrivate: true,
	})
	defer cleanup()

	msg := callErr(t, reg, "web_fetch", map[string]any{"url": srv.URL})
	if !strings.Contains(msg, "private") && !strings.Contains(msg, "loopback") {
		t.Fatalf("BlockPrivate should refuse httptest loopback, got %s", msg)
	}
}

func TestWebFetchClipLongLine(t *testing.T) {
	long := strings.Repeat("x", 80)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(long))
	}))
	defer srv.Close()

	_, reg, cleanup := setup(t, builtins.Options{
		Root:         t.TempDir(),
		HTTPClient:   srv.Client(),
		MaxLineRunes: 20,
	})
	defer cleanup()

	out := call(t, reg, "web_fetch", map[string]any{"url": srv.URL})
	if strings.Contains(out, strings.Repeat("x", 21)) {
		t.Fatalf("long line should be clipped, got %q", out)
	}
	if !strings.Contains(out, "xxxxxxxxxxxxxxxxxxxx…") {
		t.Fatalf("want clipped line with ellipsis, got %q", out)
	}
	if !strings.Contains(out, "lines=1") {
		t.Fatalf("header=%q", out)
	}
}

func TestWebFetchOffsetLimit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("L0\nL1\nL2\nL3\nL4\n"))
	}))
	defer srv.Close()

	_, reg, cleanup := setup(t, builtins.Options{
		Root:       t.TempDir(),
		HTTPClient: srv.Client(),
		ReadLimit:  2,
	})
	defer cleanup()

	page0 := call(t, reg, "web_fetch", map[string]any{"url": srv.URL})
	if !strings.Contains(page0, "lines=5") || !strings.Contains(page0, "offset=0") {
		t.Fatalf("header=%q", page0)
	}
	if !strings.Contains(page0, "L0") || !strings.Contains(page0, "L1") || strings.Contains(page0, "L2") {
		t.Fatalf("page0=%q", page0)
	}
	if !strings.Contains(page0, "pass offset=2 limit=2") {
		t.Fatalf("want continuation trailer, got %q", page0)
	}

	page1 := call(t, reg, "web_fetch", map[string]any{"url": srv.URL, "offset": 2, "limit": 2})
	if !strings.Contains(page1, "L2") || !strings.Contains(page1, "L3") || strings.Contains(page1, "L0") || strings.Contains(page1, "L4") {
		t.Fatalf("page1=%q", page1)
	}
	if !strings.Contains(page1, "pass offset=4 limit=2") {
		t.Fatalf("want next trailer, got %q", page1)
	}

	page2 := call(t, reg, "web_fetch", map[string]any{"url": srv.URL, "offset": 4, "limit": 2})
	if !strings.Contains(page2, "L4") || strings.Contains(page2, "L3") || strings.Contains(page2, "pass offset=") {
		t.Fatalf("page2=%q", page2)
	}

	past := call(t, reg, "web_fetch", map[string]any{"url": srv.URL, "offset": 10})
	if !strings.Contains(past, "past end") || !strings.Contains(past, "has 5 lines") {
		t.Fatalf("past=%q", past)
	}
	if n := hits.Load(); n != 4 {
		t.Fatalf("each continue should re-GET, hits=%d", n)
	}
}

func TestWebFetchEmptyAndRawTrunc(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/empty", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, reg, cleanup := setup(t, builtins.Options{
		Root:          t.TempDir(),
		HTTPClient:    srv.Client(),
		MaxFetchBytes: 16,
	})
	defer cleanup()

	empty := call(t, reg, "web_fetch", map[string]any{"url": srv.URL + "/empty"})
	if !strings.Contains(empty, "(empty body)") {
		t.Fatalf("empty=%q", empty)
	}

	big := call(t, reg, "web_fetch", map[string]any{"url": srv.URL + "/big"})
	if !strings.Contains(big, "raw body truncated at max_bytes=16") {
		t.Fatalf("big=%q", big)
	}
}

func TestWebSearchInjectedAndDDGParse(t *testing.T) {
	html := `<html><body>
<table><tr><td><a class="result-link" href="https://example.com/a">Alpha</a></td></tr>
<tr><td class="result-snippet">Snippet A</td></tr>
<tr><td><a class="result-link" href="https://example.com/b">Beta</a></td></tr>
<tr><td class="result-snippet">Snippet B</td></tr>
</table></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(html))
	}))
	defer srv.Close()

	_, reg, cleanup := setup(t, builtins.Options{
		Root:           t.TempDir(),
		HTTPClient:     srv.Client(),
		SearchEndpoint: srv.URL + "/?q=",
	})
	defer cleanup()
	out := call(t, reg, "web_search", map[string]any{"query": "pulse"})
	if !strings.Contains(out, "Alpha") || !strings.Contains(out, "https://example.com/a") {
		t.Fatalf("search=%q", out)
	}

	_, reg2, cleanup2 := setup(t, builtins.Options{
		Root: t.TempDir(),
		Searcher: stubSearcher{hits: []builtins.SearchHit{{Title: "X", URL: "https://x.test", Snippet: "y"}}},
	})
	defer cleanup2()
	out = call(t, reg2, "web_search", map[string]any{"query": "q"})
	if !strings.Contains(out, "https://x.test") {
		t.Fatalf("%q", out)
	}
}

func TestQuestionAskerAndPreview(t *testing.T) {
	_, reg, cleanup := setup(t, builtins.Options{Root: t.TempDir()})
	defer cleanup()
	msg := callErr(t, reg, "question", map[string]any{"text": "hi"})
	if !strings.Contains(msg, "no Asker") {
		t.Fatalf("%s", msg)
	}

	_, reg2, cleanup2 := setup(t, builtins.Options{Root: t.TempDir(), Asker: stubAsker{ans: "pick-a"}})
	defer cleanup2()
	out := call(t, reg2, "question", map[string]any{"text": "choose", "options": []string{"a", "b"}})
	if out != "pick-a" {
		t.Fatalf("%q", out)
	}
	p, ok, err := reg2.Preview(context.Background(), "question", json.RawMessage(`{"text":"choose"}`))
	if err != nil || !ok || p.Opaque == nil || !strings.Contains(p.Opaque.Summary, "choose") {
		t.Fatalf("%+v %v %v", p, ok, err)
	}
	_, ok = reg2.LookupPreview("web_fetch")
	if !ok {
		t.Fatal("web_fetch should have PreviewFn")
	}
}
