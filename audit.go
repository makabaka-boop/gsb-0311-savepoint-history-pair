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

// activeSavepoint is one frame of a transaction's live savepoint stack.
type activeSavepoint struct {
	name string
	seq  int
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
//   - READ and WRITE carry a key; SAVEPOINT and ROLLBACK carry a 1..32 char
//     ASCII name and no key; COMMIT and ABORT carry neither;
//   - savepoint names are unique among a transaction's live savepoints, and
//     ROLLBACK may only name a live savepoint of the same transaction;
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
	savepoints := make(map[string][]activeSavepoint, len(l.Txns))
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
		case OpSavepoint:
			if !validSavepointName(op.Name) || op.Key != "" {
				return nil, fmt.Errorf("op %d: SAVEPOINT requires a 1..32 char name of ASCII letters, digits, '_' or '-' and no key", op.Seq)
			}
		case OpRollback:
			if !validSavepointName(op.Name) || op.Key != "" {
				return nil, fmt.Errorf("op %d: ROLLBACK requires a 1..32 char name of ASCII letters, digits, '_' or '-' and no key", op.Seq)
			}
		case OpCommit, OpAbort:
			if op.Key != "" || op.Name != "" {
				return nil, fmt.Errorf("op %d: %s must not carry a key or name", op.Seq, op.Type)
			}
		default:
			return nil, fmt.Errorf("op %d: unknown op %q (want READ, WRITE, SAVEPOINT, ROLLBACK, COMMIT or ABORT)", op.Seq, op.Type)
		}
		if t, ok := terminated[op.Txn]; ok {
			return nil, fmt.Errorf("op %d: transaction %q already terminated at op %d", op.Seq, op.Txn, t)
		}
		// Savepoint well-formedness: names must not collide among this
		// transaction's live savepoints, and a ROLLBACK must name one.
		// Rolling back drops every later savepoint; the target survives and
		// may be rolled back again.
		switch op.Type {
		case OpSavepoint:
			for _, sp := range savepoints[op.Txn] {
				if sp.name == op.Name {
					return nil, fmt.Errorf("op %d: savepoint %q already exists in transaction %q", op.Seq, op.Name, op.Txn)
				}
			}
			savepoints[op.Txn] = append(savepoints[op.Txn], activeSavepoint{name: op.Name, seq: op.Seq})
		case OpRollback:
			stack := savepoints[op.Txn]
			idx := -1
			for k, sp := range stack {
				if sp.name == op.Name {
					idx = k
					break
				}
			}
			if idx < 0 {
				return nil, fmt.Errorf("op %d: no live savepoint %q in transaction %q", op.Seq, op.Name, op.Txn)
			}
			savepoints[op.Txn] = stack[:idx+1]
		}
		if op.Type == OpCommit || op.Type == OpAbort {
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
// WRITE on the key (even if that write was later aborted), or the initial
// version when no WRITE precedes the read.
type Source struct {
	Kind  string `json:"kind"` // "initial" or "write"
	Seq   int    `json:"seq,omitempty"`
	Txn   string `json:"txn,omitempty"`
	Value *int64 `json:"value,omitempty"`
}

// ReadFact records one READ and the source it observed.
type ReadFact struct {
	Seq    int    `json:"seq"`
	Txn    string `json:"txn"`
	Key    string `json:"key"`
	Value  *int64 `json:"value,omitempty"`
	Source Source `json:"source"`
}

// RollbackFact records one ROLLBACK: the writes of this transaction that
// newly stop participating in later read selection (UndoneWrites), the reads
// of this transaction newly discarded as commit dependencies
// (DiscardedReads), and every historical READ that observed one of those
// writes (AffectedReads), including reads later discarded by their reader.
type RollbackFact struct {
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
	Rollbacks       []RollbackFact   `json:"rollbacks"`
	Edges           []Edge           `json:"edges"`
	Serializability SerialResult     `json:"serializability"`
	Recoverable     PropResult       `json:"recoverable"`
	Cascadeless     PropResult       `json:"cascadeless"`
	Strict          PropResult       `json:"strict"`
	FinalState      map[string]int64 `json:"finalState"`
}

// ---------- audit ----------

// spFrame holds the writes and reads a transaction performed after one of
// its live savepoints. Rolling back to that savepoint undoes/discards them.
type spFrame struct {
	seq    int
	writes []int
	reads  []int
}

// Audit scans the log once in order and derives the full report.
//
// The properties are judged purely by log position, never by the final
// state: a write becomes visible to other transactions only when its
// transaction COMMITs, and an ABORT never erases a dirty read that already
// happened. ROLLBACK TO SAVEPOINT is stronger: the writes it undoes stop
// participating in later read-value selection (for everyone) and in commit
// dependencies, and reads the reader rolls back stop being its commit
// dependencies. The physical operations still took place, so they remain in
// the read audit trail and in the conflict graph.
func Audit(l *Log) *Report {
	rep := &Report{
		OK:           true,
		Transactions: l.Txns,
		NumOps:       len(l.Ops),
		Reads:        []ReadFact{},
		Rollbacks:    []RollbackFact{},
		FinalState:   map[string]int64{},
	}

	committed := make(map[string]int, len(l.Txns)) // txn -> commit seq
	undoneAt := make(map[int]int)                  // write seq -> seq of the ROLLBACK that undid it
	discardedAt := make(map[int]int)               // read seq -> seq of the ROLLBACK that discarded it
	frames := make(map[string][]spFrame, len(l.Txns))
	var recV, casV, strV *Violation

	// latestWriteOn returns the seq of the latest still-effective WRITE on
	// key at the current position (beforeSeq): the writer's own savepoint
	// rollback removes a write from visibility for every later read.
	latestWriteOn := func(key string, beforeSeq int) (int, bool) {
		for j := beforeSeq - 2; j >= 0; j-- {
			w := &l.Ops[j]
			if w.Type != OpWrite || w.Key != key {
				continue
			}
			if s, ok := undoneAt[w.Seq]; ok && s < beforeSeq {
				continue
			}
			return w.Seq, true
		}
		return 0, false
	}
	// latestForeignWriteOn is the strictness probe: the latest
	// still-effective WRITE on the key by a different transaction.
	latestForeignWriteOn := func(txn, key string, beforeSeq int) (int, bool) {
		for j := beforeSeq - 2; j >= 0; j-- {
			w := &l.Ops[j]
			if w.Type != OpWrite || w.Key != key || w.Txn == txn {
				continue
			}
			if s, ok := undoneAt[w.Seq]; ok && s < beforeSeq {
				continue
			}
			return w.Seq, true
		}
		return 0, false
	}

	for i := range l.Ops {
		op := &l.Ops[i]
		switch op.Type {
		case OpRead:
			fact := ReadFact{Seq: op.Seq, Txn: op.Txn, Key: op.Key}
			if ws, ok := latestWriteOn(op.Key, op.Seq); ok {
				w := &l.Ops[ws-1]
				v := w.Value
				fact.Value = &v
				fact.Source = Source{Kind: "write", Seq: w.Seq, Txn: w.Txn, Value: &v}
				if w.Txn != op.Txn {
					if _, ok := committed[w.Txn]; !ok {
						// The source write is uncommitted: its transaction is
						// still active or already aborted (an abort does not
						// cleanse the write). A later savepoint rollback by
						// the reader does not erase this dirty read either.
						if casV == nil {
							casV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
								Reason: fmt.Sprintf("reads uncommitted write of transaction %s (op %d)", w.Txn, w.Seq)}
						}
						if strV == nil {
							strV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
								Reason: fmt.Sprintf("reads uncommitted write of transaction %s (op %d)", w.Txn, w.Seq)}
						}
					}
				}
			} else {
				fact.Source = Source{Kind: "initial"}
			}
			// Strict is defined on the latest foreign write even when this
			// read's own source is an earlier write of the reader.
			if strV == nil {
				if ws, ok := latestForeignWriteOn(op.Txn, op.Key, op.Seq); ok {
					w := &l.Ops[ws-1]
					if _, ok := committed[w.Txn]; !ok {
						strV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
							Reason: fmt.Sprintf("reads uncommitted write of transaction %s (op %d)", w.Txn, w.Seq)}
					}
				}
			}
			rep.Reads = append(rep.Reads, fact)
			if st := frames[op.Txn]; len(st) > 0 {
				st[len(st)-1].reads = append(st[len(st)-1].reads, op.Seq)
				frames[op.Txn] = st
			}
		case OpWrite:
			// Strict: the most recent write of another transaction on this
			// key must already be committed (and not rolled back).
			if ws, ok := latestForeignWriteOn(op.Txn, op.Key, op.Seq); ok {
				w := &l.Ops[ws-1]
				if _, ok := committed[w.Txn]; !ok && strV == nil {
					strV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type, Key: op.Key,
						Reason: fmt.Sprintf("overwrites uncommitted write of transaction %s (op %d)", w.Txn, w.Seq)}
				}
			}
			if st := frames[op.Txn]; len(st) > 0 {
				st[len(st)-1].writes = append(st[len(st)-1].writes, op.Seq)
				frames[op.Txn] = st
			}
		case OpSavepoint:
			frames[op.Txn] = append(frames[op.Txn], spFrame{seq: op.Seq})
		case OpRollback:
			stack := frames[op.Txn]
			// The parser already verified the named savepoint is live; find
			// the frame it opened (the stack index matches savepoint order).
			idx := len(stack) - 1
			for k := range stack {
				if l.Ops[stack[k].seq-1].Name == op.Name {
					idx = k
					break
				}
			}
			var undone, discarded []int
			for k := idx; k < len(stack); k++ {
				undone = append(undone, stack[k].writes...)
				discarded = append(discarded, stack[k].reads...)
			}
			// Operations of the target frame were performed strictly after
			// the target savepoint; dropping them and keeping an empty target
			// frame lets the same savepoint be rolled back again.
			target := spFrame{seq: stack[idx].seq}
			frames[op.Txn] = append(stack[:idx:idx], target)
			for _, ws := range undone {
				undoneAt[ws] = op.Seq
			}
			for _, rs := range discarded {
				discardedAt[rs] = op.Seq
			}
			sort.Ints(undone)
			sort.Ints(discarded)
			if undone == nil {
				undone = []int{}
			}
			if discarded == nil {
				discarded = []int{}
			}
			// affectedReads: every READ before this ROLLBACK whose source
			// was one of the writes undone now — including reads already
			// discarded by their own reader and reads by this transaction.
			undoneSet := make(map[int]bool, len(undone))
			for _, ws := range undone {
				undoneSet[ws] = true
			}
			affected := []int{}
			for _, f := range rep.Reads {
				if f.Seq >= op.Seq {
					break
				}
				if f.Source.Kind == "write" && undoneSet[f.Source.Seq] {
					affected = append(affected, f.Seq)
				}
			}
			rep.Rollbacks = append(rep.Rollbacks, RollbackFact{
				Seq:            op.Seq,
				Txn:            op.Txn,
				Name:           op.Name,
				UndoneWrites:   undone,
				DiscardedReads: discarded,
				AffectedReads:  affected,
			})
		case OpCommit:
			// Recoverable: every read this transaction kept (did not roll
			// back itself) must come from a committed transaction whose
			// write has not been rolled back to a savepoint. Readers that
			// discarded their dirty read commit cleanly; readers keeping a
			// read of a write later undone do not.
			if recV == nil {
				for _, f := range rep.Reads {
					if f.Txn != op.Txn || f.Seq > op.Seq {
						continue
					}
					if _, gone := discardedAt[f.Seq]; gone {
						continue
					}
					if f.Source.Kind != "write" || f.Source.Txn == op.Txn {
						continue
					}
					if s, undone := undoneAt[f.Source.Seq]; undone && s < op.Seq {
						recV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type,
							Reason: fmt.Sprintf("commits while a retained read (op %d) depends on rolled-back write of transaction %s (op %d)", f.Seq, f.Source.Txn, f.Source.Seq)}
						break
					}
					if _, ok := committed[f.Source.Txn]; !ok {
						recV = &Violation{Seq: op.Seq, Txn: op.Txn, Op: op.Type,
							Reason: fmt.Sprintf("commits before its source transaction %s (read at op %d) has committed", f.Source.Txn, f.Seq)}
						break
					}
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

	// Final committed state: writes of committed transactions that survived
	// savepoint rollback, applied in log order. Shown for contrast only — it
	// can look perfectly correct while the properties above are violated.
	for i := range l.Ops {
		op := &l.Ops[i]
		if op.Type != OpWrite {
			continue
		}
		if _, ok := committed[op.Txn]; !ok {
			continue
		}
		if _, ok := undoneAt[op.Seq]; ok {
			continue
		}
		rep.FinalState[op.Key] = op.Value
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
