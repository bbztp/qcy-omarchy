package main

import "testing"

// batterySession serves a fixed battery characteristic over the script session.
type batterySession struct {
	*scriptSession
	raw []byte
}

func (b batterySession) Read(k charKind, buf []byte) (int, error) {
	if k == charBattery {
		return copy(buf, b.raw), nil
	}
	return b.scriptSession.Read(k, buf)
}

func readBatteryWith(t *testing.T, raw ...byte) map[string]interface{} {
	t.Helper()
	d, s := scriptDaemon(t, nil)
	d.mu.Lock()
	d.sess = batterySession{s, raw}
	d.mu.Unlock()
	m, err := d.readBattery()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBatteryChargingBitIsStrippedFromTheLevel(t *testing.T) {
	m := readBatteryWith(t, 0x80|55, 70, 0x80|60)
	if m["left"] != byte(55) || m["leftCharging"] != true {
		t.Fatalf("left = %v charging %v, want 55 true", m["left"], m["leftCharging"])
	}
	if m["right"] != byte(70) || m["rightCharging"] != false {
		t.Fatalf("right = %v charging %v, want 70 false", m["right"], m["rightCharging"])
	}
	if m["case"] != byte(60) || m["caseCharging"] != true || m["casePresent"] != true {
		t.Fatalf("case = %v charging %v present %v", m["case"], m["caseCharging"], m["casePresent"])
	}
}

func TestBatteryCaseSentinels(t *testing.T) {
	for _, tc := range []struct {
		raw     byte
		present bool
		level   byte
	}{
		{0x7f, false, 0x7f},
		{0x00, false, 0},
		{0xff, false, 0x7f}, // sentinel under the charging bit still masks to 0x7f
		{0x80, false, 0},
		{50, true, 50},
	} {
		m := readBatteryWith(t, 80, 70, tc.raw)
		if m["casePresent"] != tc.present || m["case"] != tc.level {
			t.Errorf("raw %#x: case=%v present=%v, want %d %v", tc.raw, m["case"], m["casePresent"], tc.level, tc.present)
		}
	}
}

func TestBatteryRawIsTheUndecodedCharacteristic(t *testing.T) {
	m := readBatteryWith(t, 0xe4, 0x64, 0x62, 0x01)
	if m["raw"] != "e4646201" {
		t.Fatalf("raw = %v, want e4646201 (all bytes, undecoded)", m["raw"])
	}
}
