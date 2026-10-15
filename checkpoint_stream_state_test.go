package pulse

import (
	"reflect"
	"testing"
)

func TestCheckpointStoppedTerminalRequiresStoppedGroup(t *testing.T) {
	original := NewKey[Stream[int]]("original")
	invalid := CheckpointStreamDescriptor{
		ID:    CheckpointStreamID{Scope: "root", NodeID: "source", Key: "original", Group: "default", Generation: 1},
		Graph: "invalid-stop-root", Attempt: "source-attempt", Element: reflect.TypeFor[int](),
		Source: CheckpointStreamSource{ID: "finite", Resumable: true}, Capacity: 1, Terminal: StreamStopped,
	}
	request := CheckpointStreamOpen{Site: Site{Graph: invalid.Graph}, Scope: invalid.ID.Scope, Attempt: invalid.Attempt,
		NodeID: invalid.ID.NodeID, Key: checkpointKey(original.asRef()), Group: invalid.ID.Group,
		Source: invalid.Source, Capacity: invalid.Capacity, Restore: &invalid}
	if err := checkCheckpointStreamRecord(request, CheckpointStreamRecord{Stream: invalid, Resumed: true}); err == nil {
		t.Error("Open accepted Terminal=stopped while Stopped=false")
	}
	valid := invalid
	valid.Stopped = true
	for _, terminal := range []StreamTerminal{StreamCompleted, StreamStopped} {
		valid.Terminal = terminal
		request.Restore = &valid
		if err := checkCheckpointStreamRecord(request, CheckpointStreamRecord{Stream: valid, Resumed: true}); err != nil {
			t.Fatalf("legitimate completed/stopped writer rejected: %v", err)
		}
		if err := checkCheckpointStreamDescriptor(checkpointKey(original.asRef()), valid); err != nil {
			t.Fatalf("legitimate terminal/reader-stop pair rejected: %v", err)
		}
	}
}
