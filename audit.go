package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// ---------- input model ----------

// OpType identifies a logged operation kind.
type OpType string

const (
	OpRead      OpType = "READ"
	OpWrite     OpType = "WRITE"
	OpCommit    OpType = "COMMIT"
	OpAbort     OpType = "ABORT"
	OpSavepoint OpType = "SAVEPOINT"
	OpRollback  OpType = "ROLLBACK"
)

// Op is a single logged operation. Seq is the 1-based position in the log,
// assigned by ParseLog (any seq present in the input is ignored). Value is
// only meaningful for WRITE (default 0).
type Op struct {
	Seq   int    `json:"seq"`
	Txn   string `json:"txn"`
	Type  OpType `json:"op"`
	Key   string `json:"key,omitempty"`
	Value int64  `json:"value,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Log is the audited input: 2..8 transactions and up to 500 ordered ops.
type Log struct {
	Txns []string `json:"transactions"`
	Ops  []Op     `json:"ops"`
}

const (
	minTxns = 2
	maxTxns = 8
	maxOps  = 500
)

// savepoint is one still-effective savepoint of a transaction.
type savepoint struct {
	seq  int
	name string
}

// validSavepointName reports whether s is 1..32 ASCII letters, digits,
// underscores or hyphens.
func validSavepointName(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// ParseLog decodes a log from JSON and validates it:
//   - 2..8 distinct, non-empty transaction ids;
//   - 1..500 ops on declared transactions;
//   - READ and WRITE carry a key, COMMIT and ABORT must not;
//   - SAVEPOINT/ROLLBACK carry a 1..32 char ASCII name and no key,
//     COMMIT/ABORT carry neither key nor name;
//   - active savepoint names are unique per transaction and a ROLLBACK may
//     only target a savepoint of its own transaction that is still effective;
//   - every transaction has exactly one terminating COMMIT or ABORT and no
//     operation of that transaction may appear after it.
func ParseLog(data []byte) (*Log, error) {
	var l Log
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(l.Txns) < minTxns || len(l.Txns) > maxTxns {
		return nil, fmt.Errorf("transactions: need %d..%d distinct ids, got %d", minTxns, maxTxns, len(l.Txns))
	}
	declared := make(map[string]bool, len(l.Txns))
	for _, t := range l.Txns {
		if t == "" {
			return nil, fmt.Errorf("transactions: empty id")
		}
		if declared[t] {
			return nil, fmt.Errorf("transactions: duplicate id %q", t)
		}
		declared[t] = true
	}
	if len(l.Ops) == 0 {
		return nil, fmt.Errorf("ops: log is empty")
	}
	if len(l.Ops) > maxOps {
		return nil, fmt.Errorf("ops: at most %d operations allowed, got %d", maxOps, len(l.Ops))
	}
	terminated := make(map[string]int, len(l.Txns)) // txn -> seq of its terminator
	savepoints := map[string][]savepoint{}          // txn -> active savepoint stack
	for i := range l.Ops {
		op := &l.Ops[i]
		op.Seq = i + 1
		if !declared[op.Txn] {
			return nil, fmt.Errorf("op %d: undeclared transaction %q", op.Seq, op.Txn)
		}
		switch op.Type {
		case OpRead, OpWrite:
			if op.Key == "" {
				return nil, fmt.Errorf("op %d: %s requires a key", op.Seq, op.Type)
			}
		case OpSavepoint, OpRollback:
			if op.Name == "" || op.Key != "" {
				return nil, fmt.Errorf("op %d: savepoint operation requires name and no key", op.Seq)
			}
			if !validSavepointName(op.Name) {
				return nil, fmt.Errorf("op %d: invalid savepoint name %q: 1..32 ASCII letters, digits, underscores or hyphens", op.Seq, op.Name)
			}
		case OpCommit, OpAbort:
			if op.Key != "" {
				return nil, fmt.Errorf("op %d: %s must not carry a key", op.Seq, op.Type)
			}
			if op.Name != "" {
				return nil, fmt.Errorf("op %d: %s must not carry a name", op.Seq, op.Type)
			}
		default:
			return nil, fmt.Errorf("op %d: unknown op %q (want READ, WRITE, SAVEPOINT, ROLLBACK, COMMIT or ABORT)", op.Seq, op.Type)
		}
		if t, ok := terminated[op.Txn]; ok {
			return nil, fmt.Errorf("op %d: transaction %q already terminated at op %d", op.Seq, op.Txn, t)
		}
		switch op.Type {
		case OpSavepoint:
			stack := savepoints[op.Txn]
			for _, sp := range stack {
				if sp.name == op.Name {
					return nil, fmt.Errorf("op %d: duplicate active savepoint %q in transaction %q", op.Seq, op.Name, op.Txn)
				}
			}
			savepoints[op.Txn] = append(stack, savepoint{seq: op.Seq, name: op.Name})
		case OpRollback:
			stack := savepoints[op.Txn]
			idx := -1
			for k := len(stack) - 1; k >= 0; k-- {
				if stack[k].name == op.Name {
					idx = k
					break
				}
			}
			if idx < 0 {
				return nil, fmt.Errorf("op %d: rollback target %q is not an active savepoint of transaction %q", op.Seq, op.Name, op.Txn)
			}
			// The targeted savepoint stays effective; later ones are lost.
			savepoints[op.Txn] = stack[:idx+1]
		case OpCommit, OpAbort:
			terminated[op.Txn] = op.Seq
		}
	}
	for _, t := range l.Txns {
		if _, ok := terminated[t]; !ok {
			return nil, fmt.Errorf("transaction %q never terminates: exactly one COMMIT or ABORT is required", t)
		}
	}
	return &l, nil
}

// ---------- report model ----------

// Source describes where a READ obtained its value: the latest preceding
// still-effective WRITE on the key (a write later undone by a savepoint
// rollback is skipped, but an aborted transaction's write is not — an abort
// does not cleanse a dirty read), or the initial version when no such write
// precedes the read.
type Source struct {
	Kind  string `json:"kind"` // "initial" or "write"
	Seq   int    `json:"seq,omitempty"`
	Txn   string `json:"txn,omitempty"`
	Value *int64 `json:"value,omitempty"`
}

// ReadFact records one READ and the source it observed. The fact stays in
// the report even when the reader later rolls this read back via a
// savepoint; such a read appears in the Rollback that discarded it.
type ReadFact struct {
	Seq    int    `json:"seq"`
	Txn    string `json:"txn"`
	Key    string `json:"key"`
	Value  *int64 `json:"value,omitempty"`
	Source Source `json:"source"`
}

// Rollback reports one ROLLBACK operation and what it invalidated.
type Rollback struct {
	Seq            int    `json:"seq"`
	Txn            string `json:"txn"`
	Name           string `json:"name"`
	UndoneWrites   []int  `json:"undoneWrites"`
	DiscardedReads []int  `json:"discardedReads"`
	AffectedReads  []int  `json:"affectedReads"`
}

// Conflict is one ordered pair of conflicting ops (same key, different
// transactions, at least one WRITE). Kind is "RW", "WR" or "WW".
type Conflict struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	FromSeq int    `json:"fromSeq"`
	ToSeq   int    `json:"toSeq"`
}

// Edge is a directed edge From -> To in the conflict graph, with every
// conflicting op pair that witnesses it.
type Edge struct {
	From      string     `json:"from"`
	To        string     `json:"to"`
	Conflicts []Conflict `json:"conflicts"`
}

// Violation pinpoints the first operation breaking a property.
type Violation struct {
	Seq    int    `json:"seq"`
	Txn    string `json:"txn"`
	Op     OpType `json:"op"`
	Key    string `json:"key,omitempty"`
	Reason string `json:"reason"`
}

// PropResult is the verdict for one execution property.
type PropResult struct {
	OK        bool       `json:"ok"`
	Violation *Violation `json:"violation,omitempty"`
}

// SerialResult reports conflict serializability: the lexicographically
// smallest serial order (ids compared bytewise) when the conflict graph is
// acyclic, otherwise one real directed cycle.
type SerialResult struct {
	Acyclic bool     `json:"acyclic"`
	Order   []string `json:"order,omitempty"`
	Cycle   []string `json:"cycle,omitempty"`
}

// Report is the full audit output.
type Report struct {
	OK              bool             `json:"ok"`
	Transactions    []string         `json:"transactions"`
	NumOps          int              `json:"numOps"`
	Reads           []ReadFact       `json:"reads"`
	Rollbacks       []Rollback       `json:"rollbacks"`
	Edges           []Edge           `json:"edges"`
	Serializability SerialResult     `json:"serializability"`
	Recoverable     PropResult       `json:"recoverable"`
	Cascadeless     PropResult       `json:"cascadeless"`
	Strict          PropResult       `json:"strict"`
	FinalState      map[string]int64 `json:"finalState"`
}

// ---------- audit ----------

// Audit scans the log once in order and derives the full report.
//
// The properties are judged purely by log position, never by the final
// state:
//
//   - A write becomes visible to other transactions only when its
//     transaction COMMITs; an ABORT never erases a write for reads that
//     already happened or happen afterwards at the physical log level.
//   - A savepoint ROLLBACK is different: the rolling-back transaction's own
//     writes after the target savepoint stop participating in subsequent
//     read-source selection, and its own reads after the savepoint stop
//     participating in later value selection and in its own commit
//     dependencies. Reads already performed (including dirty reads of those
//     writes) stay in the audit record; the physical conflict edges stay as
//     well.
func Audit(l *Log) *Report {
	rep := &Report{
		OK:           true,
		Transactions: l.Txns,
		NumOps:       len(l.Ops),
		Reads:        []ReadFact{},
		Rollbacks:    []Rollback{},
		FinalState:   map[string]int64{},
	}

	committed := make(map[string]int, len(l.Txns)) // txn -> commit seq
	undoneAt := make(map[int]int)                  // write seq -> seq of the ROLLBACK that undid it
	discardedAt := make(map[int]int)               // read seq -> seq of the ROLLBACK that discarded it
	// Per-transaction list of write seqs still alive (no undoAt entry), in
	// log order, kept compacted on rollback.
	writesByTxn := map[string][]int{}
	var readFacts []ReadFact
	stack := map[string][]savepoint{}
	var recV, casV, strV *Violation

	// liveSourceAt returns the latest WRITE seq on key that was effective at
	// readSeq: not undone by a rollback at or before that point.
	liveSourceAt := func(key string, readSeq int) (int, bool) {
		for j := readSeq - 1; j >= 1; j-- {
			o := &l.Ops[j-1]
			if o.Type != OpWrite || o.Key != key {
				continue
			}
			if at, ok := undoneAt[j]; ok && at <= readSeq {
				continue
			}
			return j, true
		}
		return 0, false
	}

	// latestOtherWriteAt returns the latest still-effective WRITE on key at
	// seq by a transaction other than txn.
	latestOtherWriteAt := func(key, txn string, seq int) (int, bool) {
		for j := seq - 1; j >= 1; j-- {
			o := &l.Ops[j-1]
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

	for i := range l.Ops {
		op := &l.Ops[i]
		switch op.Type {
		case OpRead:
			fact := ReadFact{Seq: op.Seq, Txn: op.Txn, Key: op.Key}
			if ws, ok := liveSourceAt(op.Key, op.Seq); ok {
				w := &l.Ops[ws-1]
				v := w.Value
				fact.Value = &v
				fact.Source = Source{Kind: "write", Seq: w.Seq, Txn: w.Txn, Value: &v}
			} else {
				fact.Source = Source{Kind: "initial"}
			}
			rep.Reads = append(rep.Reads, fact)
			readFacts = append(readFacts, fact)

			// Cascadeless: the read must not observe another transaction's
			// uncommitted write. A reader rolling this read back later does
			// not cleanse the dirty read itself. (Reading one's own
			// uncommitted write is not a dirty read.)
			if fact.Source.Kind == "write" && fact.Source.Txn != op.Txn {
				if c, ok := committed[fact.Source.Txn]; !ok || c > op.Seq {
					if casV == nil {
						casV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
							Reason: fmt.Sprintf("reads uncommitted write of transaction %s (op %d)", fact.Source.Txn, fact.Source.Seq)}
					}
				}
			}
			// Strict: the key must carry no uncommitted write of another
			// transaction — independent of which write this read sourced.
			if ws, ok := latestOtherWriteAt(op.Key, op.Txn, op.Seq); ok {
				w := &l.Ops[ws-1]
				if c, ok := committed[w.Txn]; !ok || c > op.Seq {
					if strV == nil {
						strV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
							Reason: fmt.Sprintf("reads while transaction %s has an uncommitted write on %s (op %d)", w.Txn, op.Key, w.Seq)}
					}
				}
			}

		case OpWrite:
			// Strict: must not overwrite/overlap another transaction's
			// still-uncommitted, still-effective write on the same key.
			if ws, ok := latestOtherWriteAt(op.Key, op.Txn, op.Seq); ok {
				w := &l.Ops[ws-1]
				if c, ok := committed[w.Txn]; !ok || c > op.Seq {
					if strV == nil {
						strV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
							Reason: fmt.Sprintf("overwrites uncommitted write of transaction %s (op %d)", w.Txn, w.Seq)}
					}
				}
			}
			writesByTxn[op.Txn] = append(writesByTxn[op.Txn], op.Seq)

		case OpSavepoint:
			stack[op.Txn] = append(stack[op.Txn], savepoint{seq: op.Seq, name: op.Name})

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
			for k, sp := range stack[op.Txn] {
				if sp.name == op.Name {
					target = sp.seq
					// Target stays effective; later savepoints are lost.
					stack[op.Txn] = stack[op.Txn][:k+1]
					break
				}
			}

			// Newly invalidate this transaction's own writes/reads strictly
			// after the target savepoint that are still alive.
			var liveWrites []int
			for _, ws := range writesByTxn[op.Txn] {
				if _, gone := undoneAt[ws]; gone {
					continue
				}
				if ws > target {
					undoneAt[ws] = op.Seq
					entry.UndoneWrites = append(entry.UndoneWrites, ws)
				} else {
					liveWrites = append(liveWrites, ws)
				}
			}
			writesByTxn[op.Txn] = liveWrites

			undoneSet := make(map[int]bool, len(entry.UndoneWrites))
			for _, ws := range entry.UndoneWrites {
				undoneSet[ws] = true
			}

			affectedSet := map[int]bool{}
			for _, rf := range readFacts {
				if rf.Seq >= op.Seq {
					break
				}
				if rf.Txn == op.Txn {
					if _, gone := discardedAt[rf.Seq]; !gone && rf.Seq > target {
						discardedAt[rf.Seq] = op.Seq
						entry.DiscardedReads = append(entry.DiscardedReads, rf.Seq)
					}
				}
				if rf.Source.Kind == "write" && undoneSet[rf.Source.Seq] {
					affectedSet[rf.Seq] = true
				}
			}
			for s := range affectedSet {
				entry.AffectedReads = append(entry.AffectedReads, s)
			}
			sort.Ints(entry.AffectedReads)
			rep.Rollbacks = append(rep.Rollbacks, entry)

		case OpCommit:
			// Recoverable: every still-retained read from another
			// transaction must source a write whose transaction committed
			// first and which was not later rolled back to a savepoint.
			// Reads this transaction discarded via its own rollback are not
			// commit dependencies anymore.
			if recV == nil {
				var bad []ReadFact
				for _, rf := range readFacts {
					if rf.Txn != op.Txn {
						continue
					}
					if at, gone := discardedAt[rf.Seq]; gone && at < op.Seq {
						continue
					}
					if rf.Source.Kind != "write" || rf.Source.Txn == op.Txn {
						continue
					}
					if c, ok := committed[rf.Source.Txn]; !ok || c >= op.Seq {
						bad = append(bad, rf)
						continue
					}
					if at, ok := undoneAt[rf.Source.Seq]; ok && at < op.Seq {
						bad = append(bad, rf)
					}
				}
				if len(bad) > 0 {
					f := bad[0] // read facts are in log order
					reason := fmt.Sprintf("commits before its source transaction %s (read at op %d) has committed", f.Source.Txn, f.Seq)
					if c, ok := committed[f.Source.Txn]; ok && c < op.Seq {
						reason = fmt.Sprintf("commits with a retained read at op %d depending on write op %d, which was rolled back before commit", f.Seq, f.Source.Seq)
					}
					recV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Reason: reason}
				}
			}
			committed[op.Txn] = op.Seq

		case OpAbort:
			// Aborting is always safe for the aborting transaction itself;
			// readers of its writes are caught at their own COMMIT.
		}
	}

	rep.Edges = conflictEdges(l)
	rep.Serializability = classify(l.Txns, rep.Edges)
	rep.Recoverable = PropResult{OK: recV == nil, Violation: recV}
	rep.Cascadeless = PropResult{OK: casV == nil, Violation: casV}
	rep.Strict = PropResult{OK: strV == nil, Violation: strV}

	// Final committed state: writes of committed transactions that were not
	// undone by a savepoint rollback, applied in log order. Shown for
	// contrast only — it can look perfectly correct while the properties
	// above are violated.
	for i := range l.Ops {
		o := &l.Ops[i]
		if o.Type != OpWrite {
			continue
		}
		if _, ok := committed[o.Txn]; !ok {
			continue
		}
		if _, gone := undoneAt[o.Seq]; gone {
			continue
		}
		rep.FinalState[o.Key] = o.Value
	}
	return rep
}

// conflictEdges builds the directed conflict graph: for every ordered pair
// of ops (a before b in the log) on the same key by different transactions
// where at least one is a WRITE, an edge a.Txn -> b.Txn.
func conflictEdges(l *Log) []Edge {
	type pair struct{ from, to string }
	byPair := map[pair][]Conflict{}
	for i := range l.Ops {
		a := &l.Ops[i]
		if a.Type != OpRead && a.Type != OpWrite {
			continue
		}
		for j := i + 1; j < len(l.Ops); j++ {
			b := &l.Ops[j]
			if b.Type != OpRead && b.Type != OpWrite {
				continue
			}
			if a.Txn == b.Txn || a.Key != b.Key {
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

// classify topologically sorts the conflict graph. When it is acyclic the
// result is the lexicographically smallest serial order (transaction ids
// compared bytewise); otherwise a real directed cycle is returned.
func classify(txns []string, edges []Edge) SerialResult {
	adj := make(map[string][]string, len(txns))
	indeg := make(map[string]int, len(txns))
	for _, t := range txns {
		indeg[t] = 0
	}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
		indeg[e.To]++
	}
	for t := range adj {
		sort.Strings(adj[t])
	}

	// Kahn's algorithm, always emitting the byte-order-smallest available
	// id: this yields the lexicographically smallest topological order.
	remaining := make(map[string]bool, len(txns))
	for _, t := range txns {
		remaining[t] = true
	}
	order := make([]string, 0, len(txns))
	for len(remaining) > 0 {
		best := ""
		for t := range remaining {
			if indeg[t] == 0 && (best == "" || t < best) {
				best = t
			}
		}
		if best == "" {
			return SerialResult{Acyclic: false, Cycle: findCycle(txns, adj)}
		}
		order = append(order, best)
		delete(remaining, best)
		for _, m := range adj[best] {
			indeg[m]--
		}
	}
	return SerialResult{Acyclic: true, Order: order}
}

// findCycle returns one real directed cycle as a list of transaction ids
// with the first id repeated at the end; every consecutive pair (including
// last -> first) is an edge of the graph. Deterministic: transactions and
// adjacency lists are visited in byte order.
func findCycle(txns []string, adj map[string][]string) []string {
	const (
		white = iota
		gray
		black
	)
	color := make(map[string]int, len(txns))
	var stack []string
	var visit func(u string) []string
	visit = func(u string) []string {
		color[u] = gray
		stack = append(stack, u)
		for _, v := range adj[u] {
			switch color[v] {
			case gray:
				start := 0
				for stack[start] != v {
					start++
				}
				cycle := append([]string{}, stack[start:]...)
				return append(cycle, v)
			case white:
				if c := visit(v); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return nil
	}
	ordered := append([]string{}, txns...)
	sort.Strings(ordered)
	for _, t := range ordered {
		if color[t] == white {
			if c := visit(t); c != nil {
				return c
			}
		}
	}
	return nil
}
