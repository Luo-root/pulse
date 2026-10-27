package systemtest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
	durable "github.com/Luo-root/pulse-durable"
	d "github.com/Luo-root/pulse-durable/distributed"
	"github.com/Luo-root/pulse-durable/distributed/postgres"
	"github.com/Luo-root/pulse-durable/distributed/worker"
)

// A user turn is one workflow. Wake-ups are a finite scheduling budget, NOT a
// model/action script. Every assistant reply and individual tool result commits
// through HandleMessage. External HTTP/tool effects and that commit are not
// atomic: only already-committed effects are asserted not to replay.
const recoveryLiveBudget = 6

type recoveryLiveMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type recoveryLiveBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}
type recoveryLiveFixture struct {
	Codes  [2]string
	Values map[string]int
}
type recoveryLiveState struct {
	History    []recoveryLiveMessage
	Assistant  json.RawMessage // complete content, including opaque thinking/signatures
	Pending    []recoveryLiveBlock
	Next       int
	Results    []recoveryLiveBlock
	Requests   int
	Used       map[string]bool
	Seen       map[string]bool
	CorrectAdd bool
	Tools      int
	Final      string
}
type recoveryLiveClient struct {
	endpoint, key, model string
	http                 *http.Client
}
type recoveryLiveHook func(*pulse.RunCtx, string, recoveryLiveState) error

func recoveryLiveHash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func recoveryLiveHTTP() *http.Client {
	return &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
}
func recoveryLiveEnv() (*recoveryLiveClient, error) {
	if os.Getenv("PULSE_LIVE_MINIMAX_ALLOW") != "yes" || os.Getenv("PULSE_LIVE_MINIMAX_KEY") == "" {
		return nil, errors.New("live model opt-in/key required")
	}
	base := os.Getenv("PULSE_LIVE_MINIMAX_BASE")
	if base == "" {
		base = "https://api.minimax.cn/anthropic"
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid HTTPS model base")
	}
	model := os.Getenv("PULSE_LIVE_MINIMAX_MODEL")
	if model == "" {
		model = "MiniMax-M3"
	}
	return &recoveryLiveClient{strings.TrimRight(u.String(), "/") + "/v1/messages", os.Getenv("PULSE_LIVE_MINIMAX_KEY"), model, recoveryLiveHTTP()}, nil
}
func (c *recoveryLiveClient) message(ctx context.Context, s *recoveryLiveState) (json.RawMessage, string, error) {
	if s.Requests >= recoveryLiveBudget {
		return nil, "", errors.New("model request budget exhausted")
	}
	body, err := json.Marshal(map[string]any{"model": c.model, "max_tokens": 2048, "messages": s.History, "tools": json.RawMessage(`[{"name":"lookup_value","description":"Read the integer assigned to a code.","input_schema":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}},{"name":"add","description":"Add two integers.","input_schema":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"],"additionalProperties":false}}]`)})
	if err != nil {
		return nil, "", errors.New("encode request failed")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", errors.New("construct request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", c.key)
	s.Requests++ // no retries; persisted with the reply, retained across takeover
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", errors.New("model HTTP failed (details withheld)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("model HTTP status %d (body withheld)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, "", errors.New("response unreadable or too large")
	}
	var v struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Stop    string          `json:"stop_reason"`
	}
	if json.Unmarshal(data, &v) != nil || v.Type != "message" || v.Role != "assistant" {
		return nil, "", errors.New("invalid response envelope")
	}
	return v.Content, v.Stop, nil
}
func recoveryLiveInitial(f recoveryLiveFixture) recoveryLiveState {
	content, _ := json.Marshal([]recoveryLiveBlock{{Type: "text", Text: fmt.Sprintf("Use lookup_value to find the values of codes %s and %s, then use add on those two values. You must use both tools, never guess. Finish with only the exact integer sum.", f.Codes[0], f.Codes[1])}})
	return recoveryLiveState{History: []recoveryLiveMessage{{Role: "user", Content: content}}, Used: map[string]bool{}, Seen: map[string]bool{}}
}
func recoveryLiveModel(ctx context.Context, c *recoveryLiveClient, s *recoveryLiveState, f recoveryLiveFixture) error {
	content, stop, err := c.message(ctx, s)
	if err != nil {
		return err
	}
	var blocks []recoveryLiveBlock
	if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
		return errors.New("invalid assistant content")
	}
	s.Assistant = append(json.RawMessage(nil), content...)
	s.History = append(s.History, recoveryLiveMessage{Role: "assistant", Content: content})
	s.Pending = nil
	s.Next = 0
	s.Results = nil
	var text strings.Builder
	for _, b := range blocks {
		if b.Type == "tool_use" {
			if b.ID == "" || s.Used[b.ID] {
				return errors.New("missing or repeated tool ID")
			}
			s.Used[b.ID] = true
			s.Pending = append(s.Pending, b)
		}
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	switch stop {
	case "tool_use":
		if len(s.Pending) == 0 || len(s.Pending) > 16 {
			return errors.New("invalid tool count")
		}
	case "end_turn":
		if len(s.Pending) != 0 || !s.CorrectAdd || strings.TrimSpace(text.String()) != strconv.Itoa(f.Values[f.Codes[0]]+f.Values[f.Codes[1]]) {
			return errors.New("final lacks verified lookup/add result")
		}
		s.Final = strings.TrimSpace(text.String())
	default:
		return errors.New("unsupported or truncated stop reason")
	}
	return nil
}
func recoveryLiveTool(s *recoveryLiveState, f recoveryLiveFixture) (recoveryLiveBlock, error) {
	b := s.Pending[s.Next]
	var args map[string]json.RawMessage
	if json.Unmarshal(b.Input, &args) != nil || args == nil {
		return b, errors.New("invalid tool input")
	}
	var result int
	switch b.Name {
	case "lookup_value":
		var code string
		if len(args) != 1 || json.Unmarshal(args["code"], &code) != nil {
			return b, errors.New("invalid lookup input")
		}
		value, ok := f.Values[code]
		if !ok {
			return b, errors.New("unknown synthetic code")
		}
		result = value
		s.Seen[code] = true
	case "add":
		var a, z int
		if len(args) != 2 || string(args["a"]) == "null" || string(args["b"]) == "null" || json.Unmarshal(args["a"], &a) != nil || json.Unmarshal(args["b"], &z) != nil || a < -1000000 || a > 1000000 || z < -1000000 || z > 1000000 {
			return b, errors.New("invalid add input")
		}
		x, y := f.Values[f.Codes[0]], f.Values[f.Codes[1]]
		if s.Seen[f.Codes[0]] && s.Seen[f.Codes[1]] && ((a == x && z == y) || (a == y && z == x)) {
			s.CorrectAdd = true
		}
		result = a + z
	default:
		return b, errors.New("unknown tool")
	}
	s.Results = append(s.Results, recoveryLiveBlock{Type: "tool_result", ToolUseID: b.ID, Content: strconv.Itoa(result)})
	s.Next++
	s.Tools++
	// Only send the model a user message after ALL calls in its reply have results.
	// Partial results and their index are nevertheless independently checkpointed.
	if s.Next == len(s.Pending) {
		encoded, _ := json.Marshal(s.Results)
		s.History = append(s.History, recoveryLiveMessage{Role: "user", Content: encoded})
	}
	return b, nil
}
func recoveryLiveAssembly(c *recoveryLiveClient, hook recoveryLiveHook) (d.Assembly, error) {
	g, err := pulse.New("live-react-recovery", "dynamic model/tool message transactions")
	if err != nil {
		return d.Assembly{}, err
	}
	fixtureKey := pulse.NewKey[string]("fixture")
	wake := pulse.NewKey[pulse.Stream[int]]("wake")
	answer := pulse.NewKey[string]("answer")
	if err = g.DeclareInput(pulse.NewPort(fixtureKey)); err != nil {
		return d.Assembly{}, err
	}
	if err = g.DeclareOutput(pulse.NewPort(answer)); err != nil {
		return d.Assembly{}, err
	}
	// 6 model replies, at most 16 calls/reply. Final stops unused wake-ups.
	if err = pulse.ProduceStream(g, "budget", "step ceiling only", wake, func(_ *pulse.RunCtx, w *pulse.StreamWriter[int]) error {
		for n := int(w.Position().Sequence) + 1; n <= 102; n++ {
			if err := w.SendAt(n, n); err != nil {
				if errors.Is(err, pulse.ErrStreamStopped) {
					return nil
				}
				return err
			}
		}
		return nil
	}, pulse.WithBuffer(102), pulse.WithStreamSource("live-react/wake", true)); err != nil {
		return d.Assembly{}, err
	}
	if err = g.Add(pulse.NewNode("react", "one reply or one tool per message", append(pulse.Requires(wake), pulse.Requires(fixtureKey)...), pulse.Provides(answer), func(rc *pulse.RunCtx) error {
		raw, err := pulse.Get(rc, fixtureKey)
		if err != nil {
			return err
		}
		var f recoveryLiveFixture
		if json.Unmarshal([]byte(raw), &f) != nil || len(f.Values) != 2 {
			return errors.New("invalid synthetic fixture")
		}
		stream, err := pulse.Get(rc, wake)
		if err != nil {
			return err
		}
		for {
			delivery, err := stream.Receive(rc)
			if err == io.EOF {
				return errors.New("step ceiling reached without final")
			}
			if err != nil {
				return err
			}
			var saved recoveryLiveState
			var kind string
			err = pulse.HandleMessage(rc, delivery, func(mc *pulse.RunCtx, _ int) error {
				s, ok, err := pulse.GetMessageState[recoveryLiveState](mc, "live-react")
				if err != nil {
					return err
				}
				if !ok {
					s = recoveryLiveInitial(f)
				}
				if s.Final != "" {
					saved = s
					return nil
				}
				if s.Next < len(s.Pending) {
					b, err := recoveryLiveTool(&s, f)
					if err != nil {
						return err
					}
					kind = "tool-" + b.Name + "-" + recoveryLiveHash([]byte(b.ID))
				} else {
					if err := recoveryLiveModel(mc.Context(), c, &s, f); err != nil {
						return err
					}
					kind = "request-" + strconv.Itoa(s.Requests)
				}
				if err := pulse.SetMessageState(mc, "live-react", s); err != nil {
					return err
				}
				if s.Final != "" {
					if err := pulse.Set(mc, answer, s.Final); err != nil {
						return err
					}
				}
				saved = s
				return nil
			})
			if err != nil {
				return err
			}
			// HandleMessage has returned: state + delivery Ack are authoritative now.
			if hook != nil {
				if err := hook(rc, kind, saved); err != nil {
					return err
				}
			}
			if saved.Final != "" {
				return stream.Stop(rc)
			}
		}
	})); err != nil {
		return d.Assembly{}, err
	}
	keys := pulse.NewRegistry()
	if err = pulse.RegisterKey(keys, fixtureKey); err != nil {
		return d.Assembly{}, err
	}
	codecs := durable.NewCodecs()
	if err = durable.RegisterJSON[recoveryLiveState](codecs, "systemtest/live-react-state", "v1"); err != nil {
		return d.Assembly{}, err
	}
	return d.Assembly{Graph: g, Codecs: codecs, Keys: keys}, nil
}

func recoveryLiveRead(t *testing.T, r durable.RunRecord) recoveryLiveState {
	t.Helper()
	var s recoveryLiveState
	if !reactAssessmentState(t, r, "live-react", &s) {
		t.Fatal("missing committed live state")
	}
	return s
}

// Offline dynamic server: replies depend on history, not wake-up parity. A final
// after 2 requests stops the remaining 99 wake-ups; full opaque content survives.
func TestLiveReActRecoveryOffline(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body struct {
			Messages []recoveryLiveMessage `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad request")
			w.WriteHeader(400)
			return
		}
		content := `[{"type":"thinking","thinking":"opaque","signature":"keep-me"},{"type":"tool_use","id":"a","name":"lookup_value","input":{"code":"x"}},{"type":"tool_use","id":"b","name":"lookup_value","input":{"code":"y"}},{"type":"tool_use","id":"c","name":"add","input":{"a":17,"b":29}}]`
		stop := "tool_use"
		if len(body.Messages) > 1 {
			if !bytes.Contains(body.Messages[1].Content, []byte(`"signature":"keep-me"`)) {
				t.Error("lost opaque assistant block")
			}
			var results []recoveryLiveBlock
			if json.Unmarshal(body.Messages[len(body.Messages)-1].Content, &results) != nil || len(results) != 3 {
				t.Error("partial tool results sent to model")
			}
			content = `[{"type":"text","text":"46"}]`
			stop = "end_turn"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "content": json.RawMessage(content), "stop_reason": stop})
	}))
	defer srv.Close()
	c := &recoveryLiveClient{srv.URL, "not-a-real-key", "offline", recoveryLiveHTTP()}
	f := recoveryLiveFixture{[2]string{"x", "y"}, map[string]int{"x": 17, "y": 29}}
	raw, _ := json.Marshal(f)
	var in pulse.Inputs
	if err := pulse.Seed(&in, pulse.NewKey[string]("fixture"), string(raw)); err != nil {
		t.Fatal(err)
	}
	store := durable.NewMemoryStore()
	firstCtx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	var committed []recoveryLiveState
	a, err := recoveryLiveAssembly(c, func(rc *pulse.RunCtx, _ string, s recoveryLiveState) error {
		committed = append(committed, s)
		if len(committed) == 2 { // assistant and first tool have committed
			interrupt()
			return rc.Context().Err()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := durable.New(store, a.Codecs)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opts := durable.RunOptions{RunID: "live-offline", Version: "v1"}
	if _, err = rt.Run(firstCtx, a.Graph, in, opts); err == nil {
		t.Fatal("expected interrupted first execution")
	}
	if requests.Load() != 1 || len(committed) != 2 {
		t.Fatal("incorrect interruption frontier")
	}
	// Fresh runtime and graph: no parent state is seeded into the replacement.
	a, err = recoveryLiveAssembly(c, func(_ *pulse.RunCtx, _ string, s recoveryLiveState) error {
		committed = append(committed, s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, err = durable.New(store, a.Codecs)
	if err != nil {
		t.Fatal(err)
	}
	e, err := rt.Resume(ctx, a.Graph, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	value, state, err := pulse.ReadOutput(e, pulse.NewKey[string]("answer"))
	if err != nil || state != pulse.SlotStateReady || value != "46" {
		t.Fatal("missing final output")
	}
	if requests.Load() != 2 || len(committed) != 5 {
		t.Fatalf("requests=%d transactions=%d; final did not stop early", requests.Load(), len(committed))
	}
	session, err := store.Acquire(ctx, "live-offline")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	record, err := session.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := recoveryLiveRead(t, record)
	if s.Final != "46" || s.Requests != 2 || s.Next != 0 || s.Tools != 3 {
		t.Fatal("committed state decode mismatch")
	}
	// Intermediate state round-trip is exactly the public message-state JSON wire
	// representation. Resume must retain pending order, partial results and budget.
	for i, original := range committed {
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var restored recoveryLiveState
		if json.Unmarshal(encoded, &restored) != nil || recoveryLiveHash(restored.Assistant) != recoveryLiveHash(original.Assistant) || restored.Next != original.Next || restored.Requests != original.Requests || len(restored.History) != len(original.History) {
			t.Fatalf("state round trip %d", i)
		}
	}
	if len(committed[0].Pending) != 3 || committed[1].Next != 1 || len(committed[1].Results) != 1 {
		t.Fatal("missing independent tool checkpoint")
	}
}

// Only the controller provisions PG. Worker children receive its isolated DSN
// via environment; they neither provision nor discover credentials.
func TestLiveReActRecoveryWorkerProcess(t *testing.T) {
	if os.Getenv("PULSE_LIVE_RECOVERY_CHILD") != "yes" {
		t.Skip("subprocess entry only")
	}
	var cfg reactAssessmentConfig
	if json.Unmarshal([]byte(os.Getenv("PULSE_LIVE_RECOVERY_CONFIG")), &cfg) != nil || cfg.Namespace == "" || cfg.ID == "" || os.Getenv("PULSE_DISTRIBUTED_TEST_ALLOW") != "yes" || os.Getenv("PULSE_DISTRIBUTED_TEST_DSN") == "" {
		t.Fatal("invalid child configuration")
	}
	c, err := recoveryLiveEnv()
	if err != nil {
		t.Fatal("live model configuration rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	backend, err := postgres.New(ctx, os.Getenv("PULSE_DISTRIBUTED_TEST_DSN"))
	if err != nil {
		t.Fatal("worker backend unavailable")
	}
	defer backend.Close()
	renew, err := postgres.New(ctx, os.Getenv("PULSE_DISTRIBUTED_TEST_DSN"))
	if err != nil {
		t.Fatal("renewer unavailable")
	}
	defer renew.Close()
	emit := &emitter{out: os.Stdout}
	o := &observed{Coordinator: backend, emit: emit, tokens: map[string]d.Token{}}
	reg := &d.Registry{}
	_, err = reg.Register("live-react", "v1", func() (d.Assembly, error) {
		return recoveryLiveAssembly(c, func(rc *pulse.RunCtx, kind string, s recoveryLiveState) error {
			tok := o.token("react")
			emit.emit(Event{Kind: kind, RunID: "react", Epoch: tok.Epoch})
			if s.Final != "" {
				emit.emit(Event{Kind: "final", Epoch: tok.Epoch, Reason: s.Final})
			}
			gate := (cfg.Window == "after_model" && s.Requests == 1 && s.Tools == 0) || (cfg.Window == "after_tool" && s.Tools == 1)
			if gate {
				emit.emit(Event{Kind: "kill_window", Epoch: tok.Epoch})
				<-rc.Context().Done()
				return rc.Context().Err()
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal("registration failed")
	}
	wc := Config{Namespace: cfg.Namespace, ID: cfg.ID, Capacity: 1}
	w, err := worker.New(wc.workerConfig(o, renew, reg, nil))
	if err != nil {
		t.Fatal("worker initialization failed")
	}
	emit.identity = w.Identity()
	emit.emit(Event{Kind: "ready"})
	if err = w.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatal("worker failed (details withheld)")
	}
}
func recoveryLiveStart(t *testing.T, ctx context.Context, f fixture, id, window string) *process {
	t.Helper()
	cfg, _ := json.Marshal(reactAssessmentConfig{f.ns, id, window})
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLiveReActRecoveryWorkerProcess$", "-test.timeout=5m")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "PULSE_LIVE_RECOVERY_") && !strings.HasPrefix(v, "PULSE_SYSTEMTEST_") && !strings.HasPrefix(v, "PULSE_REACT_") && !strings.HasPrefix(v, "PULSE_DISTRIBUTED_TEST_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "PULSE_LIVE_RECOVERY_CHILD=yes", "PULSE_LIVE_RECOVERY_CONFIG="+string(cfg), "PULSE_DISTRIBUTED_TEST_DSN="+f.dsn, "PULSE_DISTRIBUTED_TEST_ALLOW=yes")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("child pipe failed")
	}
	p := &process{cmd: cmd, done: make(chan error, 1)}
	if err = cmd.Start(); err != nil {
		t.Fatal("child start failed")
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		s := bufio.NewScanner(out)
		for s.Scan() {
			var e Event
			if json.Unmarshal(s.Bytes(), &e) == nil && e.Kind != "" {
				p.mu.Lock()
				p.events = append(p.events, e)
				p.mu.Unlock()
			}
		}
	}()
	go func() { err := cmd.Wait(); <-readDone; p.done <- err }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("child did not exit")
		}
	})
	recoveryLiveAwait(t, ctx, func() bool { return p.has("ready") }, "worker ready")
	return p
}
func recoveryLiveAwait(t *testing.T, ctx context.Context, condition func() bool, label string) {
	t.Helper()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("live recovery deadline: %s", label)
		case <-time.After(30 * time.Millisecond):
		}
	}
}
func recoveryLiveRandom(t *testing.T) recoveryLiveFixture {
	t.Helper()
	var codes [2]string
	values := map[string]int{}
	for i := range codes {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			t.Fatal("random code failed")
		}
		codes[i] = hex.EncodeToString(b)
		n, err := rand.Int(rand.Reader, big.NewInt(90000))
		if err != nil {
			t.Fatal("random value failed")
		}
		values[codes[i]] = 10000 + int(n.Int64())
	}
	return recoveryLiveFixture{codes, values}
}
func TestLiveReActRecoveryPostgres(t *testing.T) {
	if os.Getenv("PULSE_LIVE_MINIMAX_ALLOW") != "yes" {
		t.Skip("PULSE_LIVE_MINIMAX_ALLOW=yes required")
	}
	if _, err := recoveryLiveEnv(); err != nil {
		t.Fatal("live model configuration rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, window := range []string{"after_model", "after_tool"} {
		t.Run(window, func(t *testing.T) {
			f := provision(t)
			a, err := recoveryLiveAssembly(nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			fixture := recoveryLiveRandom(t)
			raw, _ := json.Marshal(fixture)
			var in pulse.Inputs
			if err = pulse.Seed(&in, pulse.NewKey[string]("fixture"), string(raw)); err != nil {
				t.Fatal(err)
			}
			task, err := d.NewTask(ctx, d.Identity{Namespace: f.ns, RunID: "react"}, "live-react", "v1", a.Graph, in, a.Codecs)
			if err != nil {
				t.Fatal("task construction failed")
			}
			if _, err = f.c.Submit(ctx, task); err != nil {
				t.Fatal("submit failed")
			}
			p := recoveryLiveStart(t, ctx, f, "live-a", window)
			recoveryLiveAwait(t, ctx, func() bool { return p.has("kill_window") }, "committed fault window")
			before := f.status(t, "react")
			record := reactAssessmentRecord(t, f)
			saved := recoveryLiveRead(t, record)
			if before.Status != d.Running || before.Epoch != 1 || before.Revision == 0 || record.Revision < before.Revision || saved.Requests != 1 || len(saved.Assistant) == 0 || len(saved.Pending) == 0 {
				t.Fatal("missing authoritative assistant checkpoint")
			}
			if window == "after_model" && (saved.Tools != 0 || saved.Next != 0) {
				t.Fatal("incorrect model frontier")
			}
			if window == "after_tool" && (saved.Tools != 1 || saved.Next != 1 || len(saved.Results) != 1) {
				t.Fatal("missing authoritative tool result checkpoint")
			}
			confirmed := 0
			for _, m := range record.Messages {
				if m.NodeID == "react" && m.State == durable.AttemptConfirmed {
					confirmed++
				}
			}
			if confirmed != 1+saved.Tools {
				t.Fatal("message Ack frontier differs from committed state")
			}
			if err = p.cmd.Process.Kill(); err != nil {
				t.Fatal("kill A failed")
			}
			b := recoveryLiveStart(t, ctx, f, "live-b", "")
			recoveryLiveAwait(t, ctx, func() bool { return f.status(t, "react").Status == d.Succeeded }, "recovered final")
			recoveryLiveAwait(t, ctx, func() bool { return b.has("final") }, "final event")
			after := f.status(t, "react")
			final := recoveryLiveRead(t, reactAssessmentRecord(t, f))
			if after.Epoch != 2 || final.Requests > 6 || final.Final != strconv.Itoa(fixture.Values[fixture.Codes[0]]+fixture.Values[fixture.Codes[1]]) || !final.CorrectAdd {
				t.Fatal("invalid recovered final/budget/epoch")
			}
			if len(final.History) < len(saved.History) {
				t.Fatal("lost history")
			}
			for i, h := range saved.History {
				if h.Role != final.History[i].Role || recoveryLiveHash(h.Content) != recoveryLiveHash(final.History[i].Content) {
					t.Fatal("committed full history/assistant hash changed across takeover")
				}
			}
			counts := map[string]int{}
			pids := map[int]bool{}
			boots := map[string]bool{}
			for _, child := range []*process{p, b} {
				for _, e := range child.snapshot() {
					if e.Kind == "ready" {
						if e.PID == os.Getpid() || pids[e.PID] || e.Worker.BootID == "" || boots[e.Worker.BootID] {
							t.Fatal("not fresh worker PID/BootID")
						}
						pids[e.PID] = true
						boots[e.Worker.BootID] = true
					}
					if strings.HasPrefix(e.Kind, "request-") || strings.HasPrefix(e.Kind, "tool-") {
						counts[e.Kind]++
						if child == b && e.Epoch != 2 {
							t.Fatal("incorrect replacement epoch")
						}
					}
				}
			}
			if len(pids) != 2 {
				t.Fatal("missing worker identities")
			}
			for i := 1; i <= final.Requests; i++ {
				if counts["request-"+strconv.Itoa(i)] != 1 {
					t.Fatal("committed model request replayed or missing")
				}
			}
			toolEvents := 0
			for k, n := range counts {
				if strings.HasPrefix(k, "tool-") {
					if n != 1 {
						t.Fatal("committed tool replayed")
					}
					toolEvents++
				}
			}
			if toolEvents != final.Tools {
				t.Fatal("tool attempt audit mismatch")
			}
			t.Logf("window=%s epoch=1->2 requests=%d tools=%d final=%s; full committed assistant/history hashes unchanged", window, final.Requests, final.Tools, final.Final)
		})
	}
}
