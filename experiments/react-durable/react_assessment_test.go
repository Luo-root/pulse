package experiment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
	. "github.com/Luo-root/pulse-durable"
)

// These assessments use only public execution/message APIs and MemoryStore.
// Counters observe side effects; they never supply recovery state. Cancellation
// is cooperative, not a process kill or a cross-worker durability assessment.
func TestAssessmentOpaqueReActLoopReplaysUncommittedNode(t *testing.T) {
	store := NewMemoryStore()
	answer := pulse.NewKey[int]("answer")
	var rounds []int
	build := func(interrupt context.CancelFunc) *pulse.Graph {
		g := testGraph(t, "assessment-opaque-react")
		if err := g.DeclareOutput(pulse.NewPort(answer)); err != nil {
			t.Fatal(err)
		}
		testAdd(t, g, pulse.NewNode("react", "opaque model/tool loop", nil, pulse.Provides(answer), func(rc *pulse.RunCtx) error {
			value := 0
			for value < 2 { // Fake model chooses increment until observation == 2.
				value++ // Fake external tool side effect, not a durable node commit.
				rounds = append(rounds, value)
				if value == 2 && interrupt != nil {
					interrupt()
					return rc.Context().Err()
				}
			}
			return pulse.Set(rc, answer, value)
		}))
		return g
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt := testRuntime(t, store, NewCodecs())
	if _, err := rt.Run(ctx, build(cancel), pulse.Inputs{}, testOptions("assessment-opaque")); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected controlled cancellation, got %v", err)
	}
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resumeCancel()
	// A fresh Runtime and graph cannot consult the canceled node's local value.
	e, err := testRuntime(t, store, NewCodecs()).Resume(resumeCtx, build(nil), pulse.Inputs{}, testOptions("assessment-opaque"))
	if err != nil {
		t.Fatal(err)
	}
	value, state, err := pulse.ReadOutput(e, answer)
	if err != nil || state != pulse.SlotStateReady || value != 2 || !reflect.DeepEqual(rounds, []int{1, 2, 1, 2}) {
		t.Fatalf("answer=%d state=%v rounds=%v err=%v", value, state, rounds, err)
	}
	t.Logf("opaque loop: answer=%d tool rounds=%v; no per-round checkpoint", value, rounds)
}

// The source supplies a bounded wake-up budget, NOT preselected model actions.
// The model reads the previous committed tool observation to decide tool/finish.
// This proves a bounded message-driven ReAct encoding, not an autonomous cyclic
// graph or unlimited agent loop. Each model+tool round is one message transaction.
func TestAssessmentMessageReActResumesCommittedRounds(t *testing.T) {
	store := NewMemoryStore()
	wake := pulse.NewKey[pulse.Stream[int]]("wake")
	answer := pulse.NewKey[int]("answer")
	var decisions []string
	var toolRounds, previousValues []int
	build := func(interrupt context.CancelFunc) *pulse.Graph {
		g := testGraph(t, "assessment-message-react")
		if err := g.DeclareOutput(pulse.NewPort(answer)); err != nil {
			t.Fatal(err)
		}
		if err := pulse.ProduceStream(g, "budget", "bounded wake-up messages", wake,
			func(_ *pulse.RunCtx, writer *pulse.StreamWriter[int]) error {
				for next := int(writer.Position().Sequence) + 1; next <= 3; next++ {
					if err := writer.SendAt(next, next); err != nil {
						return err
					}
				}
				return nil
			}, pulse.WithBuffer(3), pulse.WithStreamSource("assessment/wake", true)); err != nil {
			t.Fatal(err)
		}
		testAdd(t, g, pulse.NewNode("react", "message-scoped model and tool", pulse.Requires(wake), pulse.Provides(answer), func(rc *pulse.RunCtx) error {
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
				if err := pulse.HandleMessage(rc, delivery, func(rc *pulse.RunCtx, round int) error {
					previous, present, err := pulse.GetMessageState[int](rc, "observation")
					if err != nil {
						return err
					}
					if present != (round > 1) || previous != round-1 {
						return fmt.Errorf("round %d recovered observation=%d present=%v", round, previous, present)
					}
					previousValues = append(previousValues, previous)
					if previous >= 2 { // Deterministic fake model decides from tool result.
						decisions = append(decisions, "finish")
						return pulse.Set(rc, answer, previous)
					}
					decisions = append(decisions, "increment")
					toolRounds = append(toolRounds, round) // Observable external side effect.
					observation := previous + 1            // Deterministic fake tool result.
					if err := pulse.SetMessageState(rc, "observation", observation); err != nil {
						return err
					}
					if round == 2 && interrupt != nil {
						// Round 1 has committed; round 2's side effect happened but its
						// staged observation and input Ack have NOT committed.
						interrupt()
						return rc.Context().Err()
					}
					return nil
				}); err != nil {
					return err
				}
			}
		}))
		return g
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := testRuntime(t, store, NewCodecs()).Run(ctx, build(cancel), pulse.Inputs{}, testOptions("assessment-message")); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected controlled cancellation, got %v", err)
	}
	if !reflect.DeepEqual(toolRounds, []int{1, 2}) {
		t.Fatalf("interruption did not occur after second side effect: %v", toolRounds)
	}
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resumeCancel()
	e, err := testRuntime(t, store, NewCodecs()).Resume(resumeCtx, build(nil), pulse.Inputs{}, testOptions("assessment-message"))
	if err != nil {
		t.Fatal(err)
	}
	value, state, err := pulse.ReadOutput(e, answer)
	if err != nil || state != pulse.SlotStateReady || value != 2 {
		t.Fatalf("answer=%d state=%v err=%v", value, state, err)
	}
	if !reflect.DeepEqual(toolRounds, []int{1, 2, 2}) ||
		!reflect.DeepEqual(previousValues, []int{0, 1, 1, 2}) ||
		!reflect.DeepEqual(decisions, []string{"increment", "increment", "increment", "finish"}) {
		t.Fatalf("tool rounds=%v observations=%v decisions=%v", toolRounds, previousValues, decisions)
	}
	t.Logf("message loop: answer=%d tool rounds=%v observations=%v decisions=%v; committed round skipped, uncommitted side effect repeated", value, toolRounds, previousValues, decisions)
}
