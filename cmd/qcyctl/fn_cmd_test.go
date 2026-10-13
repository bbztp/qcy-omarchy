package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func fakeFnDaemon(t *testing.T, list []interface{}) *[]map[string]interface{} {
	t.Helper()
	var sets []map[string]interface{}
	saved := fnDaemonRequest
	t.Cleanup(func() { fnDaemonRequest = saved })
	fnDaemonRequest = func(cmd string, args map[string]interface{}) (interface{}, error) {
		switch cmd {
		case "fnList":
			return list, nil
		case "fnSet":
			sets = append(sets, args)
			return map[string]interface{}{"sent": "ff03400101", "confirmed": true, "state": "on"}, nil
		}
		return nil, errors.New("unexpected " + cmd)
	}
	return &sets
}

func sampleFnList() []interface{} {
	return []interface{}{
		map[string]interface{}{"cmdId": 64.0, "type": "toggle", "kind": "toggle", "label": "AdaptiveVolume", "state": "off"},
		map[string]interface{}{"cmdId": 25.0, "type": "list", "kind": "language", "label": "Tone Switch", "state": "4C583030",
			"options": []interface{}{map[string]interface{}{"label": "English", "value": "4C583030"}, map[string]interface{}{"label": "Chinese", "value": "4C583031"}}},
		map[string]interface{}{"cmdId": 77.0, "type": "toggle", "kind": "toggle", "label": "Restarting", "confirm": true, "restart": true, "text": "restarts"},
		map[string]interface{}{"cmdId": 3.0, "type": "action", "kind": "command", "label": "Reset to factory", "confirm": true, "danger": true, "text": "close to the case"},
	}
}

func TestFnListHuman(t *testing.T) {
	fakeFnDaemon(t, sampleFnList())
	var b bytes.Buffer
	if err := fnRun([]string{"list"}, &b); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"AdaptiveVolume", "English", "Chinese=4C583031", "needs --yes --yes"} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("missing %q in\n%s", w, b.String())
		}
	}
	var j bytes.Buffer
	if err := fnRun([]string{"list", "--json"}, &j); err != nil || !strings.Contains(j.String(), `"cmdId": 64`) {
		t.Fatalf("%v %s", err, j.String())
	}
}

func TestFnListEmpty(t *testing.T) {
	fakeFnDaemon(t, nil)
	var b bytes.Buffer
	if err := fnRun([]string{"list", "--json"}, &b); err != nil || strings.TrimSpace(b.String()) != "[]" {
		t.Fatalf("%v %q", err, b.String())
	}
}

func TestFnSetSendsValueAndConfirmations(t *testing.T) {
	sets := fakeFnDaemon(t, sampleFnList())
	var b bytes.Buffer
	if err := fnRun([]string{"set", "64", "on"}, &b); err != nil {
		t.Fatal(err)
	}
	if got := (*sets)[0]; got["value"] != "on" || got["confirm"] != nil || got["cmdId"] != 64.0 {
		t.Errorf("%v", got)
	}
	// Restart warning: refused without --yes, sent with it.
	if err := fnRun([]string{"set", "77", "on"}, &b); err == nil {
		t.Error("restart function sent without --yes")
	}
	if len(*sets) != 1 {
		t.Fatalf("sets %v", *sets)
	}
	if err := fnRun([]string{"set", "77", "on", "--yes"}, &b); err != nil {
		t.Fatal(err)
	}
	if got := (*sets)[1]; got["confirm"] != true || got["confirmAgain"] != nil {
		t.Errorf("%v", got)
	}
	// Reset-type: --yes once is not enough.
	if err := fnRun([]string{"set", "3", "--yes"}, &b); err == nil {
		t.Error("factory reset with a single --yes")
	}
	if err := fnRun([]string{"set", "3", "--yes", "--yes"}, &b); err != nil {
		t.Fatal(err)
	}
	if got := (*sets)[2]; got["confirm"] != true || got["confirmAgain"] != true || got["value"] != nil {
		t.Errorf("%v", got)
	}
	// Undeclared id and bad usage.
	for _, a := range [][]string{{"set", "9", "on"}, {"set"}, {"set", "x", "on"}, {"set", "64"}, {"set", "3", "on", "--yes", "--yes"}, {"nope"}} {
		if err := fnRun(a, &b); err == nil {
			t.Errorf("%v accepted", a)
		}
	}
}
