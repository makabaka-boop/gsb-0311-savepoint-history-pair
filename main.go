// Command txcheck audits a database operation log (JSON) for reads-from
// relationships, conflict-graph serializability, and the recoverable /
// cascadeless / strict execution properties.
//
// Usage:
//
//	txcheck [log.json]     (reads stdin when no file is given)
//
// The audit report is written to stdout as JSON. Input/validation errors
// are reported as a JSON object on stderr with exit code 1.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func main() {
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: txcheck [log.json]   (reads stdin when no file is given)")
		os.Exit(2)
	}
	var (
		data []byte
		err  error
	)
	if len(os.Args) == 2 {
		data, err = os.ReadFile(os.Args[1])
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fail(err)
	}
	log, err := ParseLog(data)
	if err != nil {
		fail(err)
	}
	rep := Audit(log)
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(out))
}

// fail reports err as a JSON object on stderr and exits non-zero.
func fail(err error) {
	msg, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	fmt.Fprintln(os.Stderr, string(msg))
	os.Exit(1)
}
