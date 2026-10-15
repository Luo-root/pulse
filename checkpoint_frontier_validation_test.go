package pulse

import (
	"strings"
	"testing"
)

func TestCheckpointStreamConfirmationFrontierValidation(t *testing.T) {
	request := CheckpointStreamOpen{Site: Site{Graph: "frontier"}, Scope: "root", Attempt: "attempt", NodeID: "source",
		Key: checkpointKey(NewKey[Stream[int]]("input").asRef()), Group: "readers", Capacity: 3,
		Source: CheckpointStreamSource{ID: "numbers", Resumable: true}}
	for _, test := range []struct {
		name    string
		last    uint64
		acked   uint64
		pending []uint64
		stopped bool
		want    string
	}{
		{name: "empty"},
		{name: "all-confirmed", last: 5, acked: 5},
		{name: "sparse-confirmations", last: 5, acked: 1, pending: []uint64{2, 5}},
		{name: "missing-pending", last: 5, acked: 1, want: "non-contiguous confirmation frontier"},
		{name: "wrong-first-pending", last: 5, acked: 1, pending: []uint64{3, 5}, want: "non-contiguous confirmation frontier"},
		{name: "stop-is-not-ack", last: 5, acked: 1, stopped: true},
		{name: "stopped-with-pending", last: 5, acked: 1, pending: []uint64{2}, stopped: true, want: "invalid confirmation progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := checkpointStreamTestHooks(&checkpointProbe{}).StreamHooks.Open(t.Context(), request)
			managedTestOK(t, err)
			record.LastSequence, record.AckedThrough = test.last, test.acked
			record.Stream.Stopped = test.stopped
			if test.stopped {
				record.Stream.Terminal = StreamStopped
			}
			for _, sequence := range test.pending {
				record.Pending = append(record.Pending, CheckpointStreamMessage{Sequence: sequence, Value: 1})
			}
			err = checkCheckpointStreamRecord(request, record)
			if test.want == "" {
				managedTestOK(t, err)
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation=%v, want %q", err, test.want)
			}
		})
	}
}
