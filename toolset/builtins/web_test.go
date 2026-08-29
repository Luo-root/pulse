package builtins_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
