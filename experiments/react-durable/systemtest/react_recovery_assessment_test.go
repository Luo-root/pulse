package systemtest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
	durable "github.com/Luo-root/pulse-durable"
	d "github.com/Luo-root/pulse-durable/distributed"
	"github.com/Luo-root/pulse-durable/distributed/postgres"
	"github.com/Luo-root/pulse-durable/distributed/worker"
	"github.com/Luo-root/pulse-durable/recordio"
)

// This is a bounded message-driven encoding, NOT an autonomous cyclic graph or
// an AI integration. Five wake-ups allow model/tool/model/tool/model(finish).
// A fake model chooses from the committed observation, not a supplied action
// script. Each model reply and each individual tool result has its OWN message
// transaction (state + input Ack). Opaque loops inside one callback do not gain
// this granularity. External tool effects are NOT atomic with those transactions.
// The controller observes records/events; it never supplies recovered state.
func reactAssessmentAssembly(visit func(*pulse.RunCtx, string) error) (d.Assembly, error) {
	g, err := pulse.New("assessment-react-process", "bounded separate model/tool transactions")
	if err != nil {
		return d.Assembly{}, err
	}
	wake := pulse.NewKey[pulse.Stream[int]]("wake")
	answer := pulse.NewKey[int]("answer")
	if err = g.DeclareOutput(pulse.NewPort(answer)); err != nil {
		return d.Assembly{}, err
	}
	if err = pulse.ProduceStream(g, "budget", "five wake-ups, not model actions", wake,
		func(_ *pulse.RunCtx, w *pulse.StreamWriter[int]) error {
			for n := int(w.Position().Sequence) + 1; n <= 5; n++ {
				if err := w.SendAt(n, n); err != nil {
					return err
				}
			}
			return nil
		}, pulse.WithBuffer(5), pulse.WithStreamSource("assessment/react-wake", true)); err != nil {
		return d.Assembly{}, err
	}
	if err = g.Add(pulse.NewNode("react", "separate model and single tool messages", pulse.Requires(wake), pulse.Provides(answer), func(rc *pulse.RunCtx) error {
		stream, err := pulse.Get(rc, wake)
		if err != nil {
			return err
		}
		for {
			delivery, err := stream.Receive(rc)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if err = pulse.HandleMessage(rc, delivery, func(rc *pulse.RunCtx, n int) error {
				obs, present, err := pulse.GetMessageState[int](rc, "observation")
				if err != nil {
					return err
				}
				if present != (n >= 3) || obs != (n-1)/2 {
					return fmt.Errorf("unexpected observation at wake %d", n)
				}
				call := func(kind string) error {
					if visit != nil {
						return visit(rc, kind)
					}
					return nil
				}
				if n%2 == 1 {
					if n == 3 {
						if err := call("after_tool"); err != nil {
							return err
						}
					}
					decision := "increment"
					if obs >= 2 {
						decision = "finish"
					}
					if err := call(fmt.Sprintf("model-%d-%s", obs+1, decision)); err != nil {
						return err
					}
					if err := pulse.SetMessageState(rc, "decision", decision); err != nil {
						return err
					}
					if decision == "finish" {
						return pulse.Set(rc, answer, obs)
					}
					return nil
				}
				decision, ok, err := pulse.GetMessageState[string](rc, "decision")
				if err != nil || !ok || decision != "increment" {
					return fmt.Errorf("missing persisted model reply at wake %d", n)
				}
				if n == 2 {
					if err := call("after_model"); err != nil {
						return err
					}
				}
				// Event represents an external effect BEFORE its result is saved.
				if err := call(fmt.Sprintf("tool-%d", obs+1)); err != nil {
					return err
				}
				if err := pulse.SetMessageState(rc, "observation", obs+1); err != nil {
					return err
				}
				if n == 4 {
					return call("unsaved_tool") // staged result, no message commit yet
				}
				return nil
			}); err != nil {
				return err
			}
		}
	})); err != nil {
		return d.Assembly{}, err
	}
	return d.Assembly{Graph: g, Codecs: durable.NewCodecs(), Keys: pulse.NewRegistry()}, nil
}

// No DB: validates pure Registry assembly and actual public message execution.
func TestAssessmentReActAssembly(t *testing.T) {
	var events []string
	a, err := reactAssessmentAssembly(func(_ *pulse.RunCtx, kind string) error {
		events = append(events, kind)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task, err := d.NewTask(ctx, d.Identity{Namespace: "unit-react", RunID: "bounded"}, "assessment-react", "v1", a.Graph, pulse.Inputs{}, a.Codecs)
	if err != nil {
		t.Fatal(err)
	}
	r := &d.Registry{}
	if _, err = r.Register("assessment-react", "v1", func() (d.Assembly, error) { return reactAssessmentAssembly(nil) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.Resolve(ctx, task); err != nil || len(events) != 0 {
		t.Fatalf("assembly invoked business or failed resolution: %v", err)
	}
	store := durable.NewMemoryStore()
	rt, err := durable.New(store, a.Codecs)
	if err != nil {
		t.Fatal(err)
	}
	e, err := rt.Run(ctx, a.Graph, pulse.Inputs{}, durable.RunOptions{RunID: "bounded", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	value, state, err := pulse.ReadOutput(e, pulse.NewKey[int]("answer"))
	want := "model-1-increment,after_model,tool-1,after_tool,model-2-increment,tool-2,unsaved_tool,model-3-finish"
	if err != nil || state != pulse.SlotStateReady || value != 2 || strings.Join(events, ",") != want {
		t.Fatalf("answer=%d state=%v events=%v err=%v", value, state, events, err)
	}
	// Exercise the same record audit decoding without connecting to PostgreSQL.
	session, err := store.Acquire(ctx, "bounded")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	record, err := session.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var observation int
	var decision string
	if !reactAssessmentState(t, record, "observation", &observation) || observation != 2 || !reactAssessmentState(t, record, "decision", &decision) || decision != "finish" {
		t.Fatal("record audit did not recover final model/tool state")
	}
}

type reactAssessmentConfig struct {
	Namespace string
	ID        string
	Window    string // fault location only; never recovery business state
}

// Separate OS worker entry, selected only by an explicit child environment key.
func TestAssessmentReActWorkerProcess(t *testing.T) {
	if os.Getenv("PULSE_REACT_CHILD") != "yes" {
		t.Skip("subprocess entry only")
	}
	var c reactAssessmentConfig
	if json.Unmarshal([]byte(os.Getenv("PULSE_REACT_CONFIG")), &c) != nil || c.Namespace == "" || c.ID == "" || os.Getenv("PULSE_DISTRIBUTED_TEST_ALLOW") != "yes" {
		t.Fatal("invalid ReAct child configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	backend, err := postgres.New(ctx, os.Getenv("PULSE_DISTRIBUTED_TEST_DSN"))
	if err != nil {
		t.Fatal("child coordinator unavailable")
	}
	defer backend.Close()
	renew, err := postgres.New(ctx, os.Getenv("PULSE_DISTRIBUTED_TEST_DSN"))
	if err != nil {
		t.Fatal("child renewer unavailable")
	}
	defer renew.Close()
	emit := &emitter{out: os.Stdout}
	o := &observed{Coordinator: backend, emit: emit, tokens: map[string]d.Token{}}
	reg := &d.Registry{}
	_, err = reg.Register("assessment-react", "v1", func() (d.Assembly, error) {
		return reactAssessmentAssembly(func(rc *pulse.RunCtx, kind string) error {
			tok := o.token("react")
			emit.emit(Event{Kind: kind, RunID: "react", Epoch: tok.Epoch})
			if kind == c.Window {
				emit.emit(Event{Kind: "kill_window", RunID: "react", Epoch: tok.Epoch, Reason: kind})
				<-rc.Context().Done() // renewal continues; only controller kill interrupts
				return rc.Context().Err()
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal("child registration failed")
	}
	cfg := Config{Namespace: c.Namespace, ID: c.ID, Capacity: 1}
	w, err := worker.New(cfg.workerConfig(o, renew, reg, func(e worker.Event) {
		emit.emit(Event{Kind: e.Kind, RunID: e.Token.Identity.RunID, Epoch: e.Token.Epoch, Reason: e.Reason})
	}))
	if err != nil {
		t.Fatal("child worker initialization failed")
	}
	emit.identity = w.Identity()
	emit.emit(Event{Kind: "ready"})
	if err = w.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatal("child worker stopped")
	}
}

func startReactAssessment(t *testing.T, f fixture, id, window string) *process {
	t.Helper()
	config, _ := json.Marshal(reactAssessmentConfig{f.ns, id, window})
	cmd := exec.Command(os.Args[0], "-test.run=^TestAssessmentReActWorkerProcess$", "-test.timeout=70s")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "PULSE_REACT_") && !strings.HasPrefix(v, "PULSE_SYSTEMTEST_") && !strings.HasPrefix(v, "PULSE_DISTRIBUTED_TEST_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "PULSE_REACT_CHILD=yes", "PULSE_REACT_CONFIG="+string(config), "PULSE_DISTRIBUTED_TEST_DSN="+f.dsn, "PULSE_DISTRIBUTED_TEST_ALLOW=yes")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &process{cmd: cmd, done: make(chan error, 1)}
	if err = cmd.Start(); err != nil {
		t.Fatal("cannot start ReAct worker")
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
			t.Error("ReAct child did not exit")
		}
	})
	await(t, func() bool { return p.has("ready") }, "ReAct child ready")
	return p
}

// Administrative observation only, restricted by provision's existing allowlist
// and random namespace. Records are never sent to B or used to select its actions.
func reactAssessmentRecord(t *testing.T, f fixture) durable.RunRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var data []byte
	if err := f.admin.QueryRow(ctx, `SELECT record FROM pulse_durable_distributed.runs WHERE namespace=$1 AND run_id='react'`, f.ns).Scan(&data); err != nil {
		t.Fatal("ReAct record audit query failed")
	}
	r, err := recordio.Decode(data)
	if err != nil {
		t.Fatal("ReAct record audit decode failed")
	}
	return r
}

func reactAssessmentState(t *testing.T, r durable.RunRecord, name string, target any) bool {
	t.Helper()
	for _, m := range r.Managed {
		if m.NodeID == "react" {
			for _, v := range m.State {
				if v.Name == name {
					if err := json.Unmarshal(v.Data, target); err != nil {
						t.Fatalf("cannot decode committed %s", name)
					}
					return true
				}
			}
		}
	}
	return false
}

func TestAssessmentReActProcessRecovery(t *testing.T) {
	for _, window := range []string{"after_model", "after_tool", "unsaved_tool"} {
		t.Run(window, func(t *testing.T) {
			f := provision(t)
			a, err := reactAssessmentAssembly(nil)
			if err != nil {
				t.Fatal(err)
			}
			task, err := d.NewTask(context.Background(), d.Identity{Namespace: f.ns, RunID: "react"}, "assessment-react", "v1", a.Graph, pulse.Inputs{}, a.Codecs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.c.Submit(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			p := startReactAssessment(t, f, "react-a", window)
			await(t, func() bool { return p.has("kill_window") }, "ReAct deterministic kill window")
			before := f.status(t, "react")
			r := reactAssessmentRecord(t, f)
			if before.Status != d.Running || before.Revision == 0 || r.Revision < before.Revision {
				t.Fatal("missing authoritative checkpoint under active lease")
			}
			confirmed := 1
			if window == "after_tool" {
				confirmed = 2
			} else if window == "unsaved_tool" {
				confirmed = 3
			}
			for n := 1; n <= 5; n++ {
				found := false
				for _, m := range r.Messages {
					if m.NodeID == "react" && m.Sequence == uint64(n) && m.State == durable.AttemptConfirmed {
						found = true
					}
				}
				if found != (n <= confirmed) {
					t.Fatalf("message %d confirmed=%v; expected frontier=%d", n, found, confirmed)
				}
			}
			var decision string
			if !reactAssessmentState(t, r, "decision", &decision) || decision != "increment" {
				t.Fatal("model reply not independently persisted")
			}
			var obs int
			present := reactAssessmentState(t, r, "observation", &obs)
			if present != (confirmed >= 2) || obs != confirmed/2 {
				t.Fatalf("committed observation=%d present=%v frontier=%d", obs, present, confirmed)
			}
			t.Logf("checkpoint namespace=%s window=%s epoch=%d revision=%d confirmed=%d observation=%d", f.ns, window, before.Epoch, r.Revision, confirmed, obs)
			if err = p.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			b := startReactAssessment(t, f, "react-b", "")
			f.success(t, "react")
			await(t, func() bool { return b.has("model-3-finish") }, "recovered finish decision event")
			after := f.status(t, "react")
			if after.Epoch <= before.Epoch {
				t.Fatal("replacement worker did not acquire a higher fencing epoch")
			}
			counts := map[string]int{}
			pids, boots := map[int]bool{}, map[string]bool{}
			for _, child := range []*process{p, b} {
				for _, e := range child.snapshot() {
					if e.Kind == "ready" {
						if e.PID == os.Getpid() || pids[e.PID] || e.Worker.BootID == "" || boots[e.Worker.BootID] {
							t.Fatal("workers are not independent OS processes/boots")
						}
						pids[e.PID], boots[e.Worker.BootID] = true, true
					}
					counts[e.Kind]++
					if strings.HasPrefix(e.Kind, "model-") || strings.HasPrefix(e.Kind, "tool-") {
						t.Logf("business pid=%d worker=%s boot=%s epoch=%d kind=%s", e.PID, e.Worker.ID, e.Worker.BootID, e.Epoch, e.Kind)
						if child == b && e.Epoch != after.Epoch {
							t.Fatal("replacement business event has incorrect epoch")
						}
					}
				}
			}
			for _, kind := range []string{"model-1-increment", "model-2-increment", "model-3-finish", "tool-1", "tool-2"} {
				want := 1
				if window == "unsaved_tool" && kind == "tool-2" {
					want = 2
				}
				if counts[kind] != want {
					t.Fatalf("%s attempts=%d want=%d", kind, counts[kind], want)
				}
			}
			final := reactAssessmentRecord(t, f)
			var finalObs int
			var finalDecision string
			if !reactAssessmentState(t, final, "observation", &finalObs) || finalObs != 2 || !reactAssessmentState(t, final, "decision", &finalDecision) || finalDecision != "finish" {
				t.Fatal("replacement did not persist final observation/decision")
			}
			answerFound := false
			for _, scope := range final.Scopes {
				for _, node := range scope.Nodes {
					for _, v := range node.Outputs {
						if v.Name == "answer" && v.State == "ready" {
							var value int
							answerFound = json.Unmarshal(v.Data, &value) == nil && value == 2
						}
					}
				}
			}
			if !answerFound {
				t.Fatal("missing authoritative final answer=2")
			}
			t.Logf("recovered namespace=%s epoch=%d revision=%d; saved model/tool not repeated, unsaved tool attempts=%d", f.ns, after.Epoch, final.Revision, counts["tool-2"])
		})
	}
}
