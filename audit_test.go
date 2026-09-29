package main

import (
	"strings"
	"testing"
)

func auditJSON(t *testing.T, in string) *Report {
	t.Helper()
	l, err := ParseLog([]byte(in))
	if err != nil {
		t.Fatalf("ParseLog: %v", err)
	}
	return Audit(l)
}

func readSource(t *testing.T, rep *Report, seq int) Source {
	t.Helper()
	for _, r := range rep.Reads {
		if r.Seq == seq {
			return r.Source
		}
	}
	t.Fatalf("no READ at seq %d", seq)
	return Source{}
}

func violSeq(p PropResult) int {
	if p.Violation == nil {
		return 0
	}
	return p.Violation.Seq
}

// Dirty read, then both abort: the final state is empty and looks clean,
// yet the dirty read at seq 2 must still be reported.
func TestDirtyReadSurvivesAbort(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T1", "op": "ABORT"},
	    {"txn": "T2", "op": "ABORT"}
	  ]
	}`)

	src := readSource(t, rep, 2)
	if src.Kind != "write" || src.Txn != "T1" || src.Seq != 1 || *src.Value != 1 {
		t.Fatalf("read source = %+v, want write T1@1 value 1", src)
	}
	if len(rep.Edges) != 1 || rep.Edges[0].From != "T1" || rep.Edges[0].To != "T2" {
		t.Fatalf("edges = %+v, want single T1->T2", rep.Edges)
	}
	if c := rep.Edges[0].Conflicts[0]; c.Kind != "WR" || c.Key != "x" || c.FromSeq != 1 || c.ToSeq != 2 {
		t.Fatalf("conflict = %+v, want WR x 1->2", c)
	}
	if !rep.Serializability.Acyclic || strings.Join(rep.Serializability.Order, ",") != "T1,T2" {
		t.Fatalf("serializability = %+v", rep.Serializability)
	}
	if !rep.Recoverable.OK {
		t.Fatalf("recoverable should hold: reader aborted, %+v", rep.Recoverable.Violation)
	}
	if got := violSeq(rep.Cascadeless); got != 2 {
		t.Fatalf("cascadeless violation seq = %d, want 2", got)
	}
	if got := violSeq(rep.Strict); got != 2 {
		t.Fatalf("strict violation seq = %d, want 2", got)
	}
	if len(rep.FinalState) != 0 {
		t.Fatalf("finalState = %v, want empty (both aborted)", rep.FinalState)
	}
}

// Reader commits before its source: classic non-recoverable schedule.
func TestNonRecoverable(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T2", "op": "COMMIT"},
	    {"txn": "T1", "op": "COMMIT"}
	  ]
	}`)
	if got := violSeq(rep.Recoverable); got != 3 {
		t.Fatalf("recoverable violation seq = %d, want 3", got)
	}
	if rep.Recoverable.Violation.Op != OpCommit {
		t.Fatalf("recoverable violation op = %s, want COMMIT", rep.Recoverable.Violation.Op)
	}
	if got := violSeq(rep.Cascadeless); got != 2 {
		t.Fatalf("cascadeless violation seq = %d, want 2", got)
	}
	if got := violSeq(rep.Strict); got != 2 {
		t.Fatalf("strict violation seq = %d, want 2", got)
	}
	if rep.FinalState["x"] != 1 {
		t.Fatalf("finalState[x] = %v, want 1", rep.FinalState["x"])
	}
}

// Source commits between the read and the reader's commit: recoverable but
// not cascadeless and not strict.
func TestRecoverableButNotCascadeless(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T1", "op": "COMMIT"},
	    {"txn": "T2", "op": "COMMIT"}
	  ]
	}`)
	if !rep.Recoverable.OK {
		t.Fatalf("recoverable should hold, %+v", rep.Recoverable.Violation)
	}
	if got := violSeq(rep.Cascadeless); got != 2 {
		t.Fatalf("cascadeless violation seq = %d, want 2", got)
	}
	if got := violSeq(rep.Strict); got != 2 {
		t.Fatalf("strict violation seq = %d, want 2", got)
	}
}

// Blind overwrite of an uncommitted write: strict fails with no reads at
// all, so recoverable and cascadeless hold vacuously.
func TestStrictFailsOnBlindOverwrite(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T2", "op": "WRITE", "key": "x", "value": 2},
	    {"txn": "T1", "op": "COMMIT"},
	    {"txn": "T2", "op": "COMMIT"}
	  ]
	}`)
	if !rep.Recoverable.OK || !rep.Cascadeless.OK {
		t.Fatalf("recoverable/cascadeless should hold: %+v %+v", rep.Recoverable, rep.Cascadeless)
	}
	if got := violSeq(rep.Strict); got != 2 {
		t.Fatalf("strict violation seq = %d, want 2", got)
	}
	if rep.Strict.Violation.Op != OpWrite {
		t.Fatalf("strict violation op = %s, want WRITE", rep.Strict.Violation.Op)
	}
	if len(rep.Edges) != 1 || rep.Edges[0].Conflicts[0].Kind != "WW" {
		t.Fatalf("edges = %+v, want single WW edge", rep.Edges)
	}
	if rep.FinalState["x"] != 2 {
		t.Fatalf("finalState[x] = %v, want 2", rep.FinalState["x"])
	}
}

// Fully strict schedule: everything holds.
func TestStrictSchedule(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T1", "op": "COMMIT"},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T2", "op": "WRITE", "key": "x", "value": 2},
	    {"txn": "T2", "op": "COMMIT"}
	  ]
	}`)
	if !rep.Recoverable.OK || !rep.Cascadeless.OK || !rep.Strict.OK {
		t.Fatalf("all properties should hold: %+v %+v %+v",
			rep.Recoverable.Violation, rep.Cascadeless.Violation, rep.Strict.Violation)
	}
	if !rep.Serializability.Acyclic || strings.Join(rep.Serializability.Order, ",") != "T1,T2" {
		t.Fatalf("serializability = %+v", rep.Serializability)
	}
	if rep.FinalState["x"] != 2 {
		t.Fatalf("finalState[x] = %v, want 2", rep.FinalState["x"])
	}
}

// A real directed cycle T1 -> T2 -> T1 through two keys.
func TestConflictCycle(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 1},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T2", "op": "WRITE", "key": "y", "value": 2},
	    {"txn": "T1", "op": "READ",  "key": "y"},
	    {"txn": "T1", "op": "COMMIT"},
	    {"txn": "T2", "op": "COMMIT"}
	  ]
	}`)
	if rep.Serializability.Acyclic {
		t.Fatalf("expected a cycle, got order %v", rep.Serializability.Order)
	}
	want := []string{"T1", "T2", "T1"}
	if strings.Join(rep.Serializability.Cycle, ",") != strings.Join(want, ",") {
		t.Fatalf("cycle = %v, want %v", rep.Serializability.Cycle, want)
	}
	// T1 commits at 5 having read T2's uncommitted y at 4.
	if got := violSeq(rep.Recoverable); got != 5 {
		t.Fatalf("recoverable violation seq = %d, want 5", got)
	}
	if got := violSeq(rep.Cascadeless); got != 2 {
		t.Fatalf("cascadeless violation seq = %d, want 2", got)
	}
}

// An aborted write is still the read source and still counts as
// uncommitted: the reader must abort, so its COMMIT breaks recoverability.
func TestAbortedWriteRemainsSource(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 9},
	    {"txn": "T1", "op": "ABORT"},
	    {"txn": "T2", "op": "READ",  "key": "x"},
	    {"txn": "T2", "op": "COMMIT"}
	  ]
	}`)
	src := readSource(t, rep, 3)
	if src.Kind != "write" || src.Txn != "T1" || src.Seq != 1 {
		t.Fatalf("read source = %+v, want write T1@1 (aborted write is still the source)", src)
	}
	if got := violSeq(rep.Cascadeless); got != 3 {
		t.Fatalf("cascadeless violation seq = %d, want 3", got)
	}
	if got := violSeq(rep.Strict); got != 3 {
		t.Fatalf("strict violation seq = %d, want 3", got)
	}
	if got := violSeq(rep.Recoverable); got != 4 {
		t.Fatalf("recoverable violation seq = %d, want 4 (source aborted, reader must abort)", got)
	}
}

// Reading the initial version and reading one's own write are always clean
// and create no cross-transaction edges.
func TestInitialAndSelfReads(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T1", "T2"],
	  "ops": [
	    {"txn": "T1", "op": "WRITE", "key": "x", "value": 5},
	    {"txn": "T1", "op": "READ",  "key": "x"},
	    {"txn": "T2", "op": "READ",  "key": "y"},
	    {"txn": "T1", "op": "COMMIT"},
	    {"txn": "T2", "op": "COMMIT"}
	  ]
	}`)
	if src := readSource(t, rep, 2); src.Kind != "write" || src.Txn != "T1" {
		t.Fatalf("self read source = %+v, want own write", src)
	}
	if src := readSource(t, rep, 3); src.Kind != "initial" {
		t.Fatalf("initial read source = %+v, want initial", src)
	}
	if len(rep.Edges) != 0 {
		t.Fatalf("edges = %+v, want none", rep.Edges)
	}
	if !rep.Recoverable.OK || !rep.Cascadeless.OK || !rep.Strict.OK {
		t.Fatalf("all properties should hold")
	}
	if got := strings.Join(rep.Serializability.Order, ","); got != "T1,T2" {
		t.Fatalf("order = %v, want T1,T2", got)
	}
}

// With no constraints the serial order is the byte order of the ids, and
// "T10" sorts before "T2" bytewise.
func TestSerialOrderByteOrder(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["T2", "T10", "T1"],
	  "ops": [
	    {"txn": "T2",  "op": "READ", "key": "a"},
	    {"txn": "T2",  "op": "COMMIT"},
	    {"txn": "T10", "op": "READ", "key": "b"},
	    {"txn": "T10", "op": "COMMIT"},
	    {"txn": "T1",  "op": "READ", "key": "c"},
	    {"txn": "T1",  "op": "COMMIT"}
	  ]
	}`)
	if got := strings.Join(rep.Serializability.Order, ","); got != "T1,T10,T2" {
		t.Fatalf("order = %v, want T1,T10,T2 (byte order)", got)
	}
}

// The smallest available id is emitted first even when it is not
// constrained: edge C->B gives order A,C,B.
func TestSerialOrderLexicographicallySmallest(t *testing.T) {
	rep := auditJSON(t, `{
	  "transactions": ["C", "A", "B"],
	  "ops": [
	    {"txn": "C", "op": "WRITE", "key": "x", "value": 7},
	    {"txn": "C", "op": "COMMIT"},
	    {"txn": "A", "op": "READ",  "key": "z"},
	    {"txn": "A", "op": "COMMIT"},
	    {"txn": "B", "op": "READ",  "key": "x"},
	    {"txn": "B", "op": "COMMIT"}
	  ]
	}`)
	if got := strings.Join(rep.Serializability.Order, ","); got != "A,C,B" {
		t.Fatalf("order = %v, want A,C,B", got)
	}
}

func TestParseValidation(t *testing.T) {
	cases := []struct {
		name, in, wantErr string
	}{
		{"bad json", `{`, "invalid JSON"},
		{"one transaction", `{"transactions":["T1"],"ops":[{"txn":"T1","op":"COMMIT"}]}`, "need 2..8"},
		{"nine transactions", `{"transactions":["a","b","c","d","e","f","g","h","i"],"ops":[]}`, "need 2..8"},
		{"duplicate id", `{"transactions":["T1","T1"],"ops":[]}`, "duplicate"},
		{"empty id", `{"transactions":["","T2"],"ops":[]}`, "empty id"},
		{"empty ops", `{"transactions":["T1","T2"],"ops":[]}`, "log is empty"},
		{"undeclared txn", `{"transactions":["T1","T2"],"ops":[{"txn":"T9","op":"COMMIT"}]}`, "undeclared transaction"},
		{"read without key", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"READ"}]}`, "requires a key"},
		{"commit with key", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"COMMIT","key":"x"}]}`, "must not carry a key"},
		{"unknown op", `{"transactions":["T1","T2"],"ops":[{"txn":"T1","op":"DELETE","key":"x"}]}`, "unknown op"},
		{"op after commit", `{"transactions":["T1","T2"],"ops":[
			{"txn":"T1","op":"COMMIT"},
			{"txn":"T1","op":"READ","key":"x"},
			{"txn":"T2","op":"COMMIT"}]}`, "already terminated"},
		{"two terminators", `{"transactions":["T1","T2"],"ops":[
			{"txn":"T1","op":"COMMIT"},
			{"txn":"T1","op":"ABORT"},
			{"txn":"T2","op":"COMMIT"}]}`, "already terminated"},
		{"missing terminator", `{"transactions":["T1","T2"],"ops":[
			{"txn":"T1","op":"COMMIT"},
			{"txn":"T2","op":"READ","key":"x"}]}`, "never terminates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseLog([]byte(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseTooManyOps(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"transactions":["T1","T2"],"ops":[`)
	for i := 0; i < 501; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"txn":"T1","op":"READ","key":"x"}`)
	}
	b.WriteString(`]}`)
	if _, err := ParseLog([]byte(b.String())); err == nil || !strings.Contains(err.Error(), "at most 500") {
		t.Fatalf("err = %v, want 'at most 500'", err)
	}
}
