package models

import "testing"

func TestSnapshot(t *testing.T) {
	if Count() <= 150 {
		t.Fatalf("count %d", Count())
	}
	all := All()
	for i := 1; i < len(all); i++ {
		if all[i-1].VendorID >= all[i].VendorID {
			t.Fatalf("not sorted at %d", i)
		}
	}
	for _, v := range []uint16{23872, 23877} {
		p, ok := Lookup(v)
		if !ok || p.Panel == nil || p.Panel.EQ == nil {
			t.Fatalf("%d: %+v", v, p)
		}
		ids := map[int]bool{}
		for _, e := range p.Panel.EQ.SysEq {
			if e.ID == nil || e.MasterGain != -12 {
				t.Errorf("%d effect %+v", v, e)
				continue
			}
			ids[*e.ID] = true
		}
		for _, id := range []int{1, 2, 3, 4, 5, 6, 10, 14} {
			if !ids[id] {
				t.Errorf("%d: effect %d missing", v, id)
			}
		}
		if len(ids) != 8 {
			t.Errorf("%d: %d effects", v, len(ids))
		}
		b := p.Panel.Beep
		if b == nil || len(b.Items) != 4 || b.Items[0].Value != 4 || b.Items[3].Value != 10 {
			t.Errorf("%d beep %+v", v, b)
		}
		if p.Panel.Fit == nil || *p.Panel.Fit.CmdID != 17 {
			t.Errorf("%d fit", v)
		}
		if w := p.Panel.Wear; w == nil || w.Sensitivity == nil || *w.Sensitivity.CmdID != 72 {
			t.Errorf("%d wear", v)
		}
		if a := p.Panel.AutoOff; a == nil || *a.CmdID != 20 {
			t.Errorf("%d autoOff", v)
		}
	}
	for _, v := range []uint16{23880, 36977} {
		if p, ok := Lookup(v); !ok || p.Title == "" {
			t.Errorf("%d not resolved", v)
		}
	}
	if _, ok := Lookup(1); ok {
		t.Error("vendor 1 must not resolve")
	}
}
