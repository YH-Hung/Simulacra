package journal

import (
	"fmt"
	"testing"
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

func sequences(calls []*Call) []uint64 {
	seqs := make([]uint64, len(calls))
	for i, call := range calls {
		seqs[i] = call.Seq
	}
	return seqs
}
