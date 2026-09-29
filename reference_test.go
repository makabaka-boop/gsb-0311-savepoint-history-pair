package main

// A reference interpreter for the same specification, written as an
// independent, deliberately brute-force implementation. The randomized
// test at the bottom cross-checks the audited report against it on
// thousands of small generated logs: read sources, conflict edges,
// serializability (order or cycle) and the first violation of each of the
// three properties.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// refCommitSeq maps each transaction to the seq of its COMMIT (absent when
// it aborts).
func refCommitSeq(l *Log) map[string]int {
	m := map[string]int{}
	for _, op := range l.Ops {
		if op.Type == OpCommit {
			m[op.Txn] = op.Seq
		}
	}
	return m
}

// refSavepointModels replays every SAVEPOINT/ROLLBACK independently of the
// auditor and returns, per write/read op seq, the seq of the rollback that
// invalidated it (0 when it stays effective), plus one expected Rollback
// report entry per ROLLBACK op.
//
// Savepoint rules: a rollback undoes the rolling-back transaction's own
// writes strictly after the target savepoint that are still alive, and
// discards its own reads in the same range; later savepoints are lost while
// the target stays effective and may be rolled back to again.
type refRollbackModel struct {
	undoneAt    map[int]int
	discardedAt map[int]int
	rollbacks   []Rollback
}

func refSavepointModels(l *Log) refRollbackModel {
	m := refRollbackModel{
		undoneAt:    map[int]int{},
		discardedAt: map[int]int{},
		rollbacks:   []Rollback{},
	}
	type sp struct {
		seq  int
		name string
	}
	stacks := map[string][]sp{}
	// Alive writes per transaction (no undo yet), in log order.
	aliveWrites := map[string][]int{}
	var reads []int // read seqs in log order
	for i := range l.Ops {
		op := &l.Ops[i]
		switch op.Type {
		case OpRead:
			reads = append(reads, op.Seq)
		case OpWrite:
			aliveWrites[op.Txn] = append(aliveWrites[op.Txn], op.Seq)
		case OpSavepoint:
			stacks[op.Txn] = append(stacks[op.Txn], sp{seq: op.Seq, name: op.Name})
		case OpRollback:
			entry := Rollback{
				Seq:            op.Seq,
				Txn:            op.Txn,
				Name:           op.Name,
				UndoneWrites:   []int{},
				DiscardedReads: []int{},
				AffectedReads:  []int{},
			}
			target := 0
			for k, s := range stacks[op.Txn] {
				if s.name == op.Name {
					target = s.seq
					stacks[op.Txn] = stacks[op.Txn][:k+1]
					break
				}
			}
			var keep []int
			for _, ws := range aliveWrites[op.Txn] {
				if ws > target {
					m.undoneAt[ws] = op.Seq
					entry.UndoneWrites = append(entry.UndoneWrites, ws)
				} else {
					keep = append(keep, ws)
				}
			}
			aliveWrites[op.Txn] = keep

			undoneSet := map[int]bool{}
			for _, ws := range entry.UndoneWrites {
				undoneSet[ws] = true
			}
			affected := map[int]bool{}
			for _, rs := range reads {
				if rs >= op.Seq {
					break
				}
				r := l.Ops[rs-1]
				if r.Txn == op.Txn {
					if _, gone := m.discardedAt[rs]; !gone && rs > target {
						m.discardedAt[rs] = op.Seq
						entry.DiscardedReads = append(entry.DiscardedReads, rs)
					}
				}
				if ws, ok := refSourceAt(l, rs, m.undoneAt); ok && undoneSet[ws] {
					affected[rs] = true
				}
			}
			for rs := range affected {
				entry.AffectedReads = append(entry.AffectedReads, rs)
			}
			sort.Ints(entry.AffectedReads)
			m.rollbacks = append(m.rollbacks, entry)
		}
	}
	return m
}

// refSourceAt returns the seq of the WRITE that is the source of the READ at
// readSeq: the latest WRITE on the same key before it which was effective at
// that point (a write undone at or before readSeq is skipped; an aborted
// transaction's write is not), or false when the read sees the initial
// version.
func refSourceAt(l *Log, readSeq int, undoneAt map[int]int) (int, bool) {
	key := l.Ops[readSeq-1].Key
	for j := readSeq - 1; j >= 1; j-- {
		if l.Ops[j-1].Type != OpWrite || l.Ops[j-1].Key != key {
			continue
		}
		if at, ok := undoneAt[j]; ok && at <= readSeq {
			continue
		}
		return j, true
	}
	return 0, false
}

// refReads recomputes every read fact by an independent backward scan,
// skipping writes undone before the read.
func refReads(l *Log, undoneAt map[int]int) []ReadFact {
	out := []ReadFact{}
	for _, op := range l.Ops {
		if op.Type != OpRead {
			continue
		}
		f := ReadFact{Seq: op.Seq, Txn: op.Txn, Key: op.Key}
		if ws, ok := refSourceAt(l, op.Seq, undoneAt); ok {
			w := l.Ops[ws-1]
			v := w.Value
			f.Value = &v
			f.Source = Source{Kind: "write", Seq: w.Seq, Txn: w.Txn, Value: &v}
		} else {
			f.Source = Source{Kind: "initial"}
		}
		out = append(out, f)
	}
	return out
}

// refLatestOtherWriteAt returns the latest effective WRITE on key at seq by
// a transaction other than txn (undone writes skipped).
func refLatestOtherWriteAt(l *Log, key, txn string, seq int, undoneAt map[int]int) (int, bool) {
	for j := seq - 1; j >= 1; j-- {
		o := l.Ops[j-1]
		if o.Type != OpWrite || o.Key != key || o.Txn == txn {
			continue
		}
		if at, ok := undoneAt[j]; ok && at <= seq {
			continue
		}
		return j, true
	}
	return 0, false
}

// refEdges recomputes the conflict edges with a plain all-pairs loop.
func refEdges(l *Log) []Edge {
	type pair struct{ from, to string }
	byPair := map[pair][]Conflict{}
	for i := 0; i < len(l.Ops); i++ {
		for j := i + 1; j < len(l.Ops); j++ {
			a, b := l.Ops[i], l.Ops[j]
			dataA := a.Type == OpRead || a.Type == OpWrite
			dataB := b.Type == OpRead || b.Type == OpWrite
			if !dataA || !dataB || a.Txn == b.Txn || a.Key != b.Key {
				continue
			}
			if a.Type == OpRead && b.Type == OpRead {
				continue
			}
			p := pair{a.Txn, b.Txn}
			byPair[p] = append(byPair[p], Conflict{
				Key:     a.Key,
				Kind:    string(a.Type[0]) + string(b.Type[0]),
				FromSeq: a.Seq,
				ToSeq:   b.Seq,
			})
		}
	}
	edges := make([]Edge, 0, len(byPair))
	for p, cs := range byPair {
		edges = append(edges, Edge{From: p.from, To: p.to, Conflicts: cs})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	return edges
}

// refSerialOrder enumerates all permutations of the transactions in
// lexicographic (byte) order and returns the first one consistent with
// every conflict pair — i.e. the lexicographically smallest serial order.
// ok is false exactly when the conflict graph has a cycle.
func refSerialOrder(l *Log) (order []string, ok bool) {
	must := map[[2]string]bool{}
	for i := 0; i < len(l.Ops); i++ {
		for j := i + 1; j < len(l.Ops); j++ {
			a, b := l.Ops[i], l.Ops[j]
			dataA := a.Type == OpRead || a.Type == OpWrite
			dataB := b.Type == OpRead || b.Type == OpWrite
			if !dataA || !dataB || a.Txn == b.Txn || a.Key != b.Key {
				continue
			}
			if a.Type == OpRead && b.Type == OpRead {
				continue
			}
			must[[2]string{a.Txn, b.Txn}] = true
		}
	}
	perm := append([]string{}, l.Txns...)
	sort.Strings(perm)
	for {
		pos := make(map[string]int, len(perm))
		for i, t := range perm {
			pos[t] = i
		}
		valid := true
		for m := range must {
			if pos[m[0]] > pos[m[1]] {
				valid = false
				break
			}
		}
		if valid {
			return append([]string{}, perm...), true
		}
		if !nextPerm(perm) {
			return nil, false
		}
	}
}

// nextPerm advances p to the next lexicographic permutation, returning
// false when p was the last one.
func nextPerm(p []string) bool {
	i := len(p) - 2
	for i >= 0 && p[i] >= p[i+1] {
		i--
	}
	if i < 0 {
		return false
	}
	j := len(p) - 1
	for p[j] <= p[i] {
		j--
	}
	p[i], p[j] = p[j], p[i]
	for lo, hi := i+1, len(p)-1; lo < hi; lo, hi = lo+1, hi-1 {
		p[lo], p[hi] = p[hi], p[lo]
	}
	return true
}

// refFirstViolations recomputes the first violating op seq (0 = none) of
// each property directly from the definitions, using a precomputed commit
// table and savepoint model instead of the auditor's incremental scan.
func refFirstViolations(l *Log, sp refRollbackModel) (recoverable, cascadeless, strict int) {
	commit := refCommitSeq(l)
	committedBefore := func(txn string, seq int) bool {
		c, ok := commit[txn]
		return ok && c < seq
	}
	for _, op := range l.Ops {
		switch op.Type {
		case OpRead:
			if ws, ok := refSourceAt(l, op.Seq, sp.undoneAt); ok {
				wt := l.Ops[ws-1].Txn
				if wt != op.Txn && !committedBefore(wt, op.Seq) && cascadeless == 0 {
					cascadeless = op.Seq
				}
			}
			if ws, ok := refLatestOtherWriteAt(l, op.Key, op.Txn, op.Seq, sp.undoneAt); ok {
				wt := l.Ops[ws-1].Txn
				if !committedBefore(wt, op.Seq) && strict == 0 {
					strict = op.Seq
				}
			}
		case OpWrite:
			if ws, ok := refLatestOtherWriteAt(l, op.Key, op.Txn, op.Seq, sp.undoneAt); ok {
				wt := l.Ops[ws-1].Txn
				if !committedBefore(wt, op.Seq) && strict == 0 {
					strict = op.Seq
				}
			}
		case OpCommit:
			bad := false
			for _, r := range l.Ops {
				if r.Seq >= op.Seq {
					break
				}
				if r.Txn != op.Txn || r.Type != OpRead {
					continue
				}
				// A read the reader itself discarded is no longer a commit
				// dependency, even though the dirty read remains recorded.
				if at, ok := sp.discardedAt[r.Seq]; ok && at < op.Seq {
					continue
				}
				ws, ok := refSourceAt(l, r.Seq, sp.undoneAt)
				if !ok {
					continue
				}
				w := l.Ops[ws-1]
				if w.Txn == op.Txn {
					continue
				}
				if !committedBefore(w.Txn, op.Seq) {
					bad = true
					break
				}
				if at, gone := sp.undoneAt[ws]; gone && at < op.Seq {
					bad = true
					break
				}
			}
			if bad && recoverable == 0 {
				recoverable = op.Seq
			}
		}
	}
	return recoverable, cascadeless, strict
}

// refFinalState applies all committed, not-undone writes in log order.
func refFinalState(l *Log, sp refRollbackModel) map[string]int64 {
	commit := refCommitSeq(l)
	out := map[string]int64{}
	for _, op := range l.Ops {
		if op.Type != OpWrite {
			continue
		}
		if _, ok := commit[op.Txn]; !ok {
			continue
		}
		if _, gone := sp.undoneAt[op.Seq]; gone {
			continue
		}
		out[op.Key] = op.Value
	}
	return out
}

// genLog builds a random valid log: 2..5 transactions (ids chosen to stress
// byte ordering), up to 3 reads/writes each on 3 shared keys interleaved
// with savepoints and rollbacks to currently active savepoints, one random
// terminator per transaction, all randomly interleaved.
func genLog(r *rand.Rand) *Log {
	ids := []string{"A", "B", "C", "D", "a", "b", "T1", "T10", "T2", "z"}
	perm := r.Perm(len(ids))
	n := 2 + r.Intn(4)
	txns := make([]string, n)
	for i := range txns {
		txns[i] = ids[perm[i]]
	}
	keys := []string{"x", "y", "z"}
	queues := make([][]Op, n)
	for i := range queues {
		// Local active-savepoint stack so generated logs are valid. Names
		// are globally unique counters and may be reused only after the
		// savepoint has been rolled back (popped).
		var spStack []string
		nextSP := 0
		spName := func() string {
			s := fmt.Sprintf("sp%d", nextSP)
			nextSP++
			return s
		}
		for k := 0; k < r.Intn(4); k++ {
			// Occasionally open a savepoint.
			if r.Intn(3) == 0 {
				name := spName()
				queues[i] = append(queues[i], Op{Txn: txns[i], Type: OpSavepoint, Name: name})
				spStack = append(spStack, name)
			}
			if r.Intn(2) == 0 {
				queues[i] = append(queues[i], Op{Txn: txns[i], Type: OpRead, Key: keys[r.Intn(len(keys))]})
			} else {
				queues[i] = append(queues[i], Op{Txn: txns[i], Type: OpWrite, Key: keys[r.Intn(len(keys))], Value: int64(r.Intn(10))})
			}
			// Occasionally roll back to the latest active savepoint (it
			// survives and may be targeted again).
			if len(spStack) > 0 && r.Intn(3) == 0 {
				name := spStack[len(spStack)-1]
				queues[i] = append(queues[i], Op{Txn: txns[i], Type: OpRollback, Name: name})
				spStack = spStack[:len(spStack)-1]
			}
		}
		term := OpCommit
		if r.Intn(3) == 0 {
			term = OpAbort
		}
		queues[i] = append(queues[i], Op{Txn: txns[i], Type: term})
	}
	var ops []Op
	for {
		var avail []int
		for i, q := range queues {
			if len(q) > 0 {
				avail = append(avail, i)
			}
		}
		if len(avail) == 0 {
			break
		}
		i := avail[r.Intn(len(avail))]
		ops = append(ops, queues[i][0])
		queues[i] = queues[i][1:]
	}
	// Round-trip through JSON so the parser is exercised as well.
	data, err := json.Marshal(Log{Txns: txns, Ops: ops})
	if err != nil {
		panic(err)
	}
	l, err := ParseLog(data)
	if err != nil {
		panic(err)
	}
	return l
}

func TestCrossCheckRandomLogs(t *testing.T) {
	for seed := int64(1); seed <= 3000; seed++ {
		l := genLog(rand.New(rand.NewSource(seed)))
		rep := Audit(l)
		prefix := func() {
			data, _ := json.Marshal(l)
			t.Logf("seed %d log: %s", seed, data)
		}
		sp := refSavepointModels(l)

		// 1. read sources
		if want := refReads(l, sp.undoneAt); !reflect.DeepEqual(rep.Reads, want) {
			prefix()
			t.Fatalf("reads mismatch:\n got %+v\nwant %+v", rep.Reads, want)
		}

		// 1b. rollback reports
		if want := sp.rollbacks; !reflect.DeepEqual(rep.Rollbacks, want) {
			prefix()
			t.Fatalf("rollbacks mismatch:\n got %+v\nwant %+v", rep.Rollbacks, want)
		}

		// 2. conflict edges
		refE := refEdges(l)
		if !reflect.DeepEqual(rep.Edges, refE) {
			prefix()
			t.Fatalf("edges mismatch:\n got %+v\nwant %+v", rep.Edges, refE)
		}
		edgeSet := map[[2]string]bool{}
		for _, e := range refE {
			edgeSet[[2]string{e.From, e.To}] = true
		}

		// 3. serializability: order when acyclic, a real cycle otherwise
		wantOrder, acyclic := refSerialOrder(l)
		if acyclic {
			if !rep.Serializability.Acyclic {
				prefix()
				t.Fatalf("auditor reports cycle %v but reference found order %v",
					rep.Serializability.Cycle, wantOrder)
			}
			if !reflect.DeepEqual(rep.Serializability.Order, wantOrder) {
				prefix()
				t.Fatalf("order mismatch: got %v want %v", rep.Serializability.Order, wantOrder)
			}
		} else {
			if rep.Serializability.Acyclic {
				prefix()
				t.Fatalf("auditor reports order %v but reference found no serial order",
					rep.Serializability.Order)
			}
			checkRealCycle(t, rep.Serializability.Cycle, edgeSet)
		}

		// 4. the three properties
		wantRec, wantCas, wantStr := refFirstViolations(l, sp)
		if got := violSeq(rep.Recoverable); got != wantRec {
			prefix()
			t.Fatalf("recoverable first violation: got %d want %d", got, wantRec)
		}
		if got := violSeq(rep.Cascadeless); got != wantCas {
			prefix()
			t.Fatalf("cascadeless first violation: got %d want %d", got, wantCas)
		}
		if got := violSeq(rep.Strict); got != wantStr {
			prefix()
			t.Fatalf("strict first violation: got %d want %d", got, wantStr)
		}
		if rep.Recoverable.Violation != nil && rep.Recoverable.Violation.Op != OpCommit {
			prefix()
			t.Fatalf("recoverable violation must be a COMMIT, got %+v", rep.Recoverable.Violation)
		}
		if rep.Cascadeless.Violation != nil && rep.Cascadeless.Violation.Op != OpRead {
			prefix()
			t.Fatalf("cascadeless violation must be a READ, got %+v", rep.Cascadeless.Violation)
		}
		if rep.Strict.Violation != nil &&
			rep.Strict.Violation.Op != OpRead && rep.Strict.Violation.Op != OpWrite {
			prefix()
			t.Fatalf("strict violation must be a READ or WRITE, got %+v", rep.Strict.Violation)
		}

		// 5. final state
		if want := refFinalState(l, sp); !reflect.DeepEqual(rep.FinalState, want) {
			prefix()
			t.Fatalf("finalState mismatch: got %v want %v", rep.FinalState, want)
		}
	}
}

// checkRealCycle verifies that c is a genuine directed cycle of the
// conflict graph: closed, simple, and every consecutive pair an edge.
func checkRealCycle(t *testing.T, c []string, edgeSet map[[2]string]bool) {
	t.Helper()
	if len(c) < 3 {
		t.Fatalf("cycle %v too short", c)
	}
	if c[0] != c[len(c)-1] {
		t.Fatalf("cycle %v not closed", c)
	}
	seen := map[string]bool{}
	for _, n := range c[:len(c)-1] {
		if seen[n] {
			t.Fatalf("cycle %v repeats node %s", c, n)
		}
		seen[n] = true
	}
	for i := 0; i+1 < len(c); i++ {
		if !edgeSet[[2]string{c[i], c[i+1]}] {
			t.Fatalf("cycle edge %s -> %s is not a conflict edge", c[i], c[i+1])
		}
	}
}
