package experiment

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
	. "github.com/Luo-root/pulse-durable"
)

// This is an opaque node's ordinary, bounded ReAct loop, NOT a per-step
// checkpoint/recovery assessment. A successful API run cannot prove recovery.
const liveReActBudget = 6

var errLiveReActBudget = errors.New("model request budget exhausted")

type liveReActMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type liveReActBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type liveReActClient struct {
	endpoint, key, model string
	http                 *http.Client
	requests             int
}

type liveReActTask struct {
	codes         [2]string
	values        map[string]int
	lookups, adds int
	seen          map[string]bool
	correctAdd    bool
}

func liveReActHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 45 * time.Second,
		// Never forward credentials to a redirected origin (or downgrade HTTPS).
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("redirect refused")
		},
	}
}

func liveReActEndpoint(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid HTTPS API base")
	}
	// An explicitly configured base is the trusted destination; no redirects
	// are followed, including redirects to a different host on that destination.
	return strings.TrimRight(u.String(), "/") + "/v1/messages", nil
}

func liveReActTools() json.RawMessage {
	return json.RawMessage(`[
		{"name":"lookup_value","description":"Read the integer assigned to a code.","input_schema":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}},
		{"name":"add","description":"Add two integers.","input_schema":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"],"additionalProperties":false}}
	]`)
}

func (c *liveReActClient) message(ctx context.Context, history []liveReActMessage) (json.RawMessage, string, error) {
	if c.requests >= liveReActBudget {
		return nil, "", errLiveReActBudget
	}
	body, err := json.Marshal(struct {
		Model     string             `json:"model"`
		MaxTokens int                `json:"max_tokens"`
		Messages  []liveReActMessage `json:"messages"`
		Tools     json.RawMessage    `json:"tools"`
	}{c.model, 2048, history, liveReActTools()})
	if err != nil {
		return nil, "", errors.New("encode model request failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", errors.New("construct model request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", c.key)
	c.requests++ // No retries: this is also the total outbound attempt budget.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", errors.New("model HTTP request failed (details withheld)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("model HTTP status %d (body withheld)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, "", errors.New("model response unreadable or too large")
	}
	var response struct {
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	}
	if json.Unmarshal(data, &response) != nil || response.Type != "message" || response.Role != "assistant" {
		return nil, "", errors.New("invalid Messages response envelope")
	}
	return response.Content, response.StopReason, nil
}

func (task *liveReActTask) tool(block liveReActBlock) (string, error) {
	var args map[string]json.RawMessage
	if json.Unmarshal(block.Input, &args) != nil || args == nil {
		return "", errors.New("invalid tool argument object")
	}
	switch block.Name {
	case "lookup_value":
		var code string
		if len(args) != 1 || json.Unmarshal(args["code"], &code) != nil {
			return "", errors.New("lookup_value requires one string code")
		}
		value, ok := task.values[code]
		if !ok {
			return "", errors.New("lookup_value code not in synthetic dataset")
		}
		task.lookups++
		if task.seen == nil {
			task.seen = make(map[string]bool)
		}
		task.seen[code] = true
		return strconv.Itoa(value), nil
	case "add":
		var a, b int
		if len(args) != 2 || json.Unmarshal(args["a"], &a) != nil || json.Unmarshal(args["b"], &b) != nil || string(args["a"]) == "null" || string(args["b"]) == "null" || a < -1000000 || a > 1000000 || b < -1000000 || b > 1000000 {
			return "", errors.New("add requires two bounded integers")
		}
		task.adds++
		x, y := task.values[task.codes[0]], task.values[task.codes[1]]
		if task.seen[task.codes[0]] && task.seen[task.codes[1]] && ((a == x && b == y) || (a == y && b == x)) {
			task.correctAdd = true
		}
		return strconv.Itoa(a + b), nil
	default:
		return "", errors.New("unknown tool name")
	}
}

func liveReActLoop(ctx context.Context, c *liveReActClient, task *liveReActTask) (string, error) {
	prompt := fmt.Sprintf("Look up the values for codes %s and %s using lookup_value, then use add to sum those two values. You must actually use both tools. Finish with the exact integer sum as your final answer. Do not guess the values.", task.codes[0], task.codes[1])
	initial, _ := json.Marshal([]liveReActBlock{{Type: "text", Text: prompt}})
	history := []liveReActMessage{{Role: "user", Content: initial}}
	usedIDs := make(map[string]bool)
	for {
		if err := ctx.Err(); err != nil {
			return "", errors.New("ReAct context ended")
		}
		content, stop, err := c.message(ctx, history)
		if err != nil {
			return "", err
		}
		var blocks []liveReActBlock
		if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
			return "", errors.New("invalid assistant content")
		}
		var calls []liveReActBlock
		var text strings.Builder
		for _, block := range blocks {
			switch block.Type {
			case "tool_use":
				calls = append(calls, block)
			case "text":
				text.WriteString(block.Text)
				text.WriteByte('\n')
				// Thinking and other content remain opaque and are never logged.
			}
		}
		// Preserve the COMPLETE assistant content, including thinking/signatures.
		history = append(history, liveReActMessage{Role: "assistant", Content: content})
		switch stop {
		case "end_turn":
			if len(calls) != 0 {
				return "", errors.New("end_turn contained tool calls")
			}
			expected := strconv.Itoa(task.values[task.codes[0]] + task.values[task.codes[1]])
			exact := regexp.MustCompile(`(^|[^0-9+-])` + regexp.QuoteMeta(expected) + `([^0-9]|$)`)
			if task.lookups < 2 || task.adds < 1 || !task.correctAdd || !exact.MatchString(text.String()) {
				return "", errors.New("final answer missing verified lookup/add result")
			}
			return strings.TrimSpace(text.String()), nil // No request after final.
		case "tool_use":
			if len(calls) == 0 {
				return "", errors.New("tool_use stop without calls")
			}
			results := make([]liveReActBlock, 0, len(calls))
			for _, call := range calls {
				if call.ID == "" || usedIDs[call.ID] {
					return "", errors.New("missing or repeated tool_use ID")
				}
				usedIDs[call.ID] = true
				result, err := task.tool(call)
				if err != nil {
					return "", err
				}
				results = append(results, liveReActBlock{Type: "tool_result", ToolUseID: call.ID, Content: result})
			}
			encoded, _ := json.Marshal(results)
			history = append(history, liveReActMessage{Role: "user", Content: encoded})
		default:
			// In particular, max_tokens is a failure, never a final answer.
			return "", errors.New("unsupported or truncated model stop reason")
		}
	}
}

func liveReActGraph(t *testing.T, ctx context.Context, c *liveReActClient, task *liveReActTask) (string, error) {
	t.Helper()
	answer := pulse.NewKey[string]("answer")
	g := testGraph(t, "live-react-opaque")
	if err := g.DeclareOutput(pulse.NewPort(answer)); err != nil {
		t.Fatal(err)
	}
	testAdd(t, g, pulse.NewNode("react", "bounded HTTP model/tool loop; no per-step checkpoint", nil, pulse.Provides(answer), func(rc *pulse.RunCtx) error {
		text, err := liveReActLoop(rc.Context(), c, task)
		if err != nil {
			return err
		}
		return pulse.Set(rc, answer, text)
	}))
	e, err := testRuntime(t, NewMemoryStore(), NewCodecs()).Run(ctx, g, pulse.Inputs{}, testOptions("live-react"))
	if err != nil {
		return "", err
	}
	text, state, err := pulse.ReadOutput(e, answer)
	if err != nil || state != pulse.SlotStateReady {
		return "", errors.New("graph output not ready")
	}
	return text, nil
}

func TestLiveReActMiniMax(t *testing.T) {
	if os.Getenv("PULSE_LIVE_MINIMAX_ALLOW") != "yes" {
		t.Skip("live MiniMax API requires explicit opt-in")
	}
	key := os.Getenv("PULSE_LIVE_MINIMAX_KEY")
	if key == "" {
		t.Fatal("PULSE_LIVE_MINIMAX_KEY is required")
	}
	base := os.Getenv("PULSE_LIVE_MINIMAX_BASE")
	if base == "" {
		base = "https://api.minimax.cn/anthropic"
	}
	endpoint, err := liveReActEndpoint(base)
	if err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("PULSE_LIVE_MINIMAX_MODEL")
	if model == "" {
		model = "MiniMax-M3"
	}
	c := &liveReActClient{endpoint: endpoint, key: key, model: model, http: liveReActHTTPClient()}
	task := &liveReActTask{values: make(map[string]int)}
	for i := range task.codes {
		var code [12]byte
		if _, err := rand.Read(code[:]); err != nil {
			t.Fatal("random code generation failed")
		}
		task.codes[i] = hex.EncodeToString(code[:])
		value, err := rand.Int(rand.Reader, big.NewInt(9000))
		if err != nil {
			t.Fatal("random value generation failed")
		}
		task.values[task.codes[i]] = 1000 + int(value.Int64())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	text, err := liveReActGraph(t, ctx, c, task)
	if err != nil {
		t.Fatalf("live ReAct failed: %v", err)
	}
	// Defense in depth: even accidental model echoes never print the credential.
	redact := func(s string) string { return strings.ReplaceAll(s, key, "[REDACTED]") }
	t.Logf("model=%q requests=%d lookup_value=%d add=%d final=%q", redact(model), c.requests, task.lookups, task.adds, redact(text))
}

func TestLiveReActProtocol(t *testing.T) {
	var requests atomic.Int32
	first := json.RawMessage(`[{"type":"thinking","thinking":"opaque private thought","signature":"opaque-signature"},{"type":"tool_use","id":"left","name":"lookup_value","input":{"code":"A"}},{"type":"tool_use","id":"right","name":"lookup_value","input":{"code":"B"}}]`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-only" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Error("wrong Messages protocol request")
		}
		var request struct {
			Model     string             `json:"model"`
			MaxTokens int                `json:"max_tokens"`
			Messages  []liveReActMessage `json:"messages"`
			Tools     []json.RawMessage  `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "fixture" || request.MaxTokens != 2048 || len(request.Tools) != 2 {
			t.Error("wrong model request body")
		}
		stop, content := "tool_use", first
		switch n {
		case 1:
			if len(request.Messages) != 1 {
				t.Error("preseeded model history")
			}
		case 2:
			if len(request.Messages) != 3 {
				t.Error("wrong first tool history")
				break
			}
			var original, replay any
			_ = json.Unmarshal(first, &original)
			_ = json.Unmarshal(request.Messages[1].Content, &replay)
			a, _ := json.Marshal(original)
			b, _ := json.Marshal(replay)
			if !bytes.Equal(a, b) {
				t.Error("assistant content not preserved")
			}
			var results []liveReActBlock
			if json.Unmarshal(request.Messages[2].Content, &results) != nil || len(results) != 2 || results[0].ToolUseID != "left" || results[0].Content != "17" || results[1].ToolUseID != "right" || results[1].Content != "25" {
				t.Error("wrong multiple tool results")
			}
			content = json.RawMessage(`[{"type":"tool_use","id":"sum","name":"add","input":{"a":17,"b":25}}]`)
		case 3:
			if len(request.Messages) != 5 {
				t.Error("wrong add history")
				break
			}
			var results []liveReActBlock
			if json.Unmarshal(request.Messages[4].Content, &results) != nil || len(results) != 1 || results[0].ToolUseID != "sum" || results[0].Content != "42" {
				t.Error("wrong add result")
			}
			stop, content = "end_turn", json.RawMessage(`[{"type":"text","text":"42"}]`)
		default:
			t.Error("request after final answer")
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "stop_reason": stop, "content": content})
	}))
	defer server.Close()
	c := &liveReActClient{endpoint: server.URL + "/v1/messages", key: "test-only", model: "fixture", http: liveReActHTTPClient()}
	task := &liveReActTask{codes: [2]string{"A", "B"}, values: map[string]int{"A": 17, "B": 25}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, err := liveReActGraph(t, ctx, c, task)
	if err != nil || text != "42" || requests.Load() != 3 || c.requests != 3 || task.lookups != 2 || task.adds != 1 {
		t.Fatalf("protocol/graph failed: requests=%d lookup=%d add=%d err=%v", c.requests, task.lookups, task.adds, err)
	}
}

func TestLiveReActRejectAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name, stop, content string
		wantRequests        int
	}{
		{"budget", "tool_use", `[{"type":"tool_use","id":"call-%d","name":"lookup_value","input":{"code":"A"}}]`, 6},
		{"truncated", "max_tokens", `[{"type":"text","text":"42"}]`, 1},
		{"end_with_tool", "end_turn", `[{"type":"tool_use","id":"x","name":"add","input":{"a":17,"b":25}}]`, 1},
		{"no_tools", "end_turn", `[{"type":"text","text":"42"}]`, 1},
		{"bad_type", "tool_use", `[{"type":"tool_use","id":"x","name":"add","input":{"a":"17","b":25}}]`, 1},
		{"null_type", "tool_use", `[{"type":"tool_use","id":"x","name":"add","input":{"a":null,"b":25}}]`, 1},
		{"bad_name", "tool_use", `[{"type":"tool_use","id":"x","name":"write","input":{}}]`, 1},
		{"duplicate_id", "tool_use", `[{"type":"tool_use","id":"x","name":"lookup_value","input":{"code":"A"}},{"type":"tool_use","id":"x","name":"lookup_value","input":{"code":"B"}}]`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := requests.Add(1)
				content := tc.content
				if tc.name == "budget" {
					content = fmt.Sprintf(content, n)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "stop_reason": tc.stop, "content": json.RawMessage(content)})
			}))
			defer server.Close()
			c := &liveReActClient{endpoint: server.URL, http: liveReActHTTPClient()}
			task := &liveReActTask{codes: [2]string{"A", "B"}, values: map[string]int{"A": 17, "B": 25}}
			_, err := liveReActLoop(context.Background(), c, task)
			if err == nil || c.requests != tc.wantRequests || int(requests.Load()) != tc.wantRequests {
				t.Fatal("expected bounded rejection")
			}
			if tc.name == "budget" && !errors.Is(err, errLiveReActBudget) {
				t.Fatal("expected request budget exhaustion")
			}
		})
	}
}

func TestLiveReActRedirectAndBase(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c := &liveReActClient{endpoint: origin.URL, key: "test-only", http: liveReActHTTPClient()}
	if _, _, err := c.message(context.Background(), nil); err == nil || leaked.Load() != 0 || c.requests != 1 {
		t.Fatal("redirect was not refused")
	}
	if endpoint, err := liveReActEndpoint("https://api.minimax.cn/anthropic/"); err != nil || endpoint != "https://api.minimax.cn/anthropic/v1/messages" {
		t.Fatal("wrong default endpoint")
	}
	for _, base := range []string{"http://api.minimax.cn/anthropic", "https://user:secret@api.minimax.cn/anthropic", "https://api.minimax.cn/anthropic?key=secret", "https://api.minimax.cn/anthropic#fragment"} {
		if _, err := liveReActEndpoint(base); err == nil {
			t.Fatal("unsafe base accepted")
		}
	}
}
