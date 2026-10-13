package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// `qcyctl fn ...`: the generic functions the connected model's panel declares
// and no specialised command covers (daemon commands fnList and fnSet). The
// daemon is required: the bytes written are the panel's, validated there.

const fnUsage = `usage: qcyctl fn <list|set>
  fn list [--json]                          (functions the model's panel declares and no other command covers)
  fn set <cmdId> <value|on|off> [--yes]     (toggle: on|off; list: an option label or its hex value; --yes confirms a
                                             restart warning; a reset-type command needs --yes --yes)`

// fnDaemonRequest sends one request to a running daemon. Tests replace it.
var fnDaemonRequest = func(cmd string, args map[string]interface{}) (interface{}, error) {
	dc, err := dialDaemon(500 * time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("fn needs the qcyctld daemon: %w", err)
	}
	defer dc.Close()
	return dc.request(cmd, args)
}

type fnOptionJSON struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type fnItemJSON struct {
	CmdID   int            `json:"cmdId"`
	Type    string         `json:"type"`
	Kind    string         `json:"kind"`
	Label   string         `json:"label"`
	Options []fnOptionJSON `json:"options,omitempty"`
	State   string         `json:"state,omitempty"`
	Raw     string         `json:"raw,omitempty"`
	Text    string         `json:"text,omitempty"`
	Restart bool           `json:"restart,omitempty"`
	Confirm bool           `json:"confirm,omitempty"`
	Danger  bool           `json:"danger,omitempty"`
}

func fnFetchList() ([]fnItemJSON, error) {
	data, err := fnDaemonRequest("fnList", nil)
	if err != nil {
		return nil, err
	}
	var items []fnItemJSON
	if err := remarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func cmdFn(args []string) error { return fnRun(args, os.Stdout) }

func fnRun(args []string, out io.Writer) error {
	if len(args) < 1 {
		return errors.New(fnUsage)
	}
	switch args[0] {
	case "list":
		return fnListCmd(args[1:], out)
	case "set":
		return fnSetCmd(args[1:], out)
	}
	return fmt.Errorf("fn: unknown subcommand %q\n%s", args[0], fnUsage)
}

func fnListCmd(args []string, out io.Writer) error {
	jsonOut := false
	for _, a := range args {
		if a != "--json" {
			return errors.New("usage: qcyctl fn list [--json]")
		}
		jsonOut = true
	}
	items, err := fnFetchList()
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if items == nil {
			items = []fnItemJSON{}
		}
		return enc.Encode(items)
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "no generic functions declared for this model")
		return nil
	}
	for _, it := range items {
		state := it.State
		if state == "" {
			state = "?"
		}
		for _, o := range it.Options {
			if o.Value == it.State {
				state = o.Label
			}
		}
		if it.Type == "action" {
			state = "-"
		}
		fmt.Fprintf(out, "%3d  %-8s %-32s %s\n", it.CmdID, it.Type, it.Label, state)
		if it.Type == "list" {
			var opts []string
			for _, o := range it.Options {
				opts = append(opts, o.Label+"="+o.Value)
			}
			fmt.Fprintf(out, "       options: %s\n", strings.Join(opts, ", "))
		}
		switch {
		case it.Danger:
			fmt.Fprintf(out, "       reset-type command: needs --yes --yes\n")
		case it.Confirm:
			fmt.Fprintf(out, "       needs --yes (restart warning)\n")
		}
		if it.Text != "" {
			fmt.Fprintf(out, "       %s\n", it.Text)
		}
	}
	return nil
}

func fnSetCmd(args []string, out io.Writer) error {
	const usage = "usage: qcyctl fn set <cmdId> <value|on|off> [--yes]"
	yes := 0
	var rest []string
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			yes++
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) < 1 || len(rest) > 2 {
		return errors.New(usage)
	}
	id, err := strconv.Atoi(rest[0])
	if err != nil || id < 0 || id > 255 {
		return errors.New(usage)
	}
	items, err := fnFetchList()
	if err != nil {
		return err
	}
	var it *fnItemJSON
	for i := range items {
		if items[i].CmdID == id {
			it = &items[i]
		}
	}
	if it == nil {
		return fmt.Errorf("fn set: function %d is not declared by this model (see: qcyctl fn list)", id)
	}
	req := map[string]interface{}{"cmdId": float64(id)}
	switch it.Type {
	case "action":
		if len(rest) != 1 {
			return fmt.Errorf("fn set: %q is a command and takes no value", it.Label)
		}
	default:
		if len(rest) != 2 {
			return errors.New(usage)
		}
		req["value"] = rest[1]
	}
	switch {
	case it.Danger && yes < 2:
		return fmt.Errorf("fn set: %q is a reset-type command (%s); repeat the confirmation: qcyctl fn set %d --yes --yes", it.Label, it.Text, id)
	case it.Confirm && yes < 1:
		return fmt.Errorf("fn set: %q asks for confirmation (%s); add --yes", it.Label, it.Text)
	}
	if yes >= 1 {
		req["confirm"] = true
	}
	if yes >= 2 {
		req["confirmAgain"] = true
	}
	data, err := fnDaemonRequest("fnSet", req)
	if err != nil {
		return err
	}
	var res struct {
		Sent      string `json:"sent"`
		Confirmed bool   `json:"confirmed"`
		State     string `json:"state"`
		Restart   bool   `json:"restart"`
		Note      string `json:"note"`
	}
	if err := remarshal(data, &res); err != nil {
		return err
	}
	fmt.Fprintf(out, "sent %s  confirmed=%v", res.Sent, res.Confirmed)
	if res.State != "" {
		fmt.Fprintf(out, "  state=%s", res.State)
	}
	if res.Restart {
		fmt.Fprint(out, "  (the earbuds restart)")
	}
	fmt.Fprintln(out)
	if res.Note != "" {
		fmt.Fprintln(out, res.Note)
	}
	return nil
}
