package journal

import (
	"fmt"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestJournalEvictsOldestAndKeepsMonotonicSequence(t *testing.T) {
	j := New(3)
	for i := 1; i <= 5; i++ {
		j.Record(&Call{Method: fmt.Sprintf("/service/Method%d", i)})
	}

	got := j.List()
	if len(got) != 3 {
		t.Fatalf("List length = %d, want 3", len(got))
	}
	for i, want := range []uint64{3, 4, 5} {
		if got[i].Seq != want {
			t.Errorf("List()[%d].Seq = %d, want %d", i, got[i].Seq, want)
		}
	}
	if got := j.Total(); got != 5 {
		t.Fatalf("Total = %d, want 5", got)
	}
}

func TestJournalMinimumCapacityIsOne(t *testing.T) {
	j := New(0)
	j.Record(&Call{Method: "/service/First"})
	j.Record(&Call{Method: "/service/Second"})

	got := j.List()
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("List = %+v, want only sequence 2", got)
	}
}

func TestJournalFilterNormalizesMethodAndLimitsToNewestMatches(t *testing.T) {
	j := New(10)
	for _, method := range []string{
		"/shop.Order/Get", "/shop.Order/Put", "/shop.Order/Get",
		"/shop.Order/Get", "/shop.Order/Put",
	} {
		j.Record(&Call{Method: method})
	}

	for _, method := range []string{"shop.Order/Get", "/shop.Order/Get"} {
		got := j.Filter(Filter{Method: method, Limit: 2})
		if len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 {
			t.Errorf("Filter(%q) sequences = %v, want [3 4]", method, sequences(got))
		}
	}
}

func TestJournalResetClearsEntriesButNotSequenceOrTotal(t *testing.T) {
	j := New(3)
	j.Record(&Call{Method: "/service/First"})
	j.Record(&Call{Method: "/service/Second"})
	j.Reset()

	if got := j.List(); len(got) != 0 {
		t.Fatalf("List after Reset = %+v, want empty", got)
	}
	if got := j.Total(); got != 2 {
		t.Fatalf("Total after Reset = %d, want 2", got)
	}
	j.Record(&Call{Method: "/service/Third"})
	if got := j.List(); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("List after new record = %+v, want sequence 3", got)
	}
	if got := j.Total(); got != 3 {
		t.Fatalf("Total after new record = %d, want 3", got)
	}
}

func TestJournalWrapsMultipleTimesAndResetsWrappedRing(t *testing.T) {
	j := New(3)
	for i := 0; i < 11; i++ {
		j.Record(&Call{Method: fmt.Sprintf("service/Method%d", i)})
	}
	if got := sequences(j.List()); fmt.Sprint(got) != "[9 10 11]" {
		t.Fatalf("sequences after wraps = %v, want [9 10 11]", got)
	}
	j.Reset()
	for i := 0; i < 4; i++ {
		j.Record(&Call{Method: "/service/AfterReset"})
	}
	if got := sequences(j.List()); fmt.Sprint(got) != "[13 14 15]" {
		t.Fatalf("sequences after reset and wrap = %v, want [13 14 15]", got)
	}
}

func TestJournalSnapshotsDoNotAliasInputOrOutput(t *testing.T) {
	req := healthMessage("original-request")
	resp := healthMessage("original-response")
	original := &Call{
		Method:     "grpc.health.v1.Health/Check",
		Metadata:   metadata.Pairs("x-test", "original"),
		Requests:   []*dynamicpb.Message{req},
		Responses:  []*dynamicpb.Message{resp},
		Err:        status.New(codes.Aborted, "original-error"),
		StubSource: "original-source",
	}
	j := New(2)
	j.Record(original)

	original.Method = "/mutated/input"
	original.Metadata.Set("x-test", "mutated-input")
	setHealthService(req, "mutated-input-request")
	setHealthService(resp, "mutated-input-response")
	original.Requests[0] = healthMessage("replaced-input-request")
	original.Responses[0] = healthMessage("replaced-input-response")

	first := j.List()[0]
	if first.Method != "/grpc.health.v1.Health/Check" || first.Metadata.Get("x-test")[0] != "original" {
		t.Fatalf("retained call changed with input: %+v", first)
	}
	if healthService(first.Requests[0]) != "original-request" || healthService(first.Responses[0]) != "original-response" {
		t.Fatalf("retained messages changed with input: %q/%q", healthService(first.Requests[0]), healthService(first.Responses[0]))
	}
	if first.Err == original.Err || first.Err.Code() != codes.Aborted || first.Err.Message() != "original-error" {
		t.Fatalf("retained status = %v, want independent Aborted status", first.Err)
	}

	first.Method = "/mutated/output"
	first.Metadata.Set("x-test", "mutated-output")
	setHealthService(first.Requests[0], "mutated-output-request")
	setHealthService(first.Responses[0], "mutated-output-response")
	first.Requests[0] = healthMessage("replaced-output-request")
	first.Responses[0] = healthMessage("replaced-output-response")
	filtered := j.Filter(Filter{Method: "grpc.health.v1.Health/Check"})[0]
	filtered.Metadata.Set("x-test", "mutated-filter")
	setHealthService(filtered.Requests[0], "mutated-filter-request")

	again := j.List()[0]
	if again.Method != "/grpc.health.v1.Health/Check" || again.Metadata.Get("x-test")[0] != "original" {
		t.Fatalf("retained call changed with output: %+v", again)
	}
	if healthService(again.Requests[0]) != "original-request" || healthService(again.Responses[0]) != "original-response" {
		t.Fatalf("retained messages changed with output: %q/%q", healthService(again.Requests[0]), healthService(again.Responses[0]))
	}
}

func TestJournalConcurrentSnapshotsAreIndependent(t *testing.T) {
	j := New(8)
	for i := 0; i < 8; i++ {
		j.Record(&Call{Metadata: metadata.Pairs("x-test", "original"), Requests: []*dynamicpb.Message{healthMessage("original")}})
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(value string) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				for _, call := range j.List() {
					call.Metadata.Set("x-test", value)
					setHealthService(call.Requests[0], value)
				}
			}
		}(fmt.Sprintf("reader-%d", i))
	}
	wg.Wait()
	for _, call := range j.List() {
		if call.Metadata.Get("x-test")[0] != "original" || healthService(call.Requests[0]) != "original" {
			t.Fatalf("retained concurrent snapshot was mutated: %+v", call)
		}
	}
}

func healthMessage(service string) *dynamicpb.Message {
	desc := healthpb.File_grpc_health_v1_health_proto.Messages().ByName("HealthCheckRequest")
	message := dynamicpb.NewMessage(desc)
	setHealthService(message, service)
	return message
}

func setHealthService(message *dynamicpb.Message, service string) {
	field := message.Descriptor().Fields().ByName(protoreflect.Name("service"))
	message.Set(field, protoreflect.ValueOfString(service))
}

func healthService(message *dynamicpb.Message) string {
	field := message.Descriptor().Fields().ByName(protoreflect.Name("service"))
	return message.Get(field).String()
}

func sequences(calls []*Call) []uint64 {
	seqs := make([]uint64, len(calls))
	for i, call := range calls {
		seqs[i] = call.Seq
	}
	return seqs
}
