package models

import (
	"net/url"
	"testing"
)

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

func TestN70Colors(t *testing.T) {
	for _, v := range []uint16{23872, 23877} {
		p, ok := Lookup(v)
		if !ok {
			t.Fatalf("%d missing", v)
		}
		if len(p.Colors) != 3 {
			t.Fatalf("%d: %d colors", v, len(p.Colors))
		}
		names := []string{"Black", "Titanium", "Purple"}
		for i, c := range p.Colors {
			if c.Name != names[i] {
				t.Errorf("%d color %d name %q", v, i, c.Name)
			}
			if c.Index == nil || *c.Index != i {
				t.Errorf("%d color %d index %v", v, i, c.Index)
			}
			if c.Left == "" || c.Right == "" || c.Box == "" || c.Icon == "" {
				t.Errorf("%d color %d images %+v", v, i, c.Images)
			}
		}
		if p.Images.Left == "" {
			t.Errorf("%d: no default images", v)
		}
	}
}

func TestImageURLsAreOnTheCDN(t *testing.T) {
	check := func(s string) {
		if s == "" {
			return
		}
		u, err := url.Parse(s)
		if err != nil || u.Hostname() != "img.qcymall.com" || (u.Scheme != "http" && u.Scheme != "https") {
			t.Errorf("bad image URL %q", s)
		}
	}
	for _, p := range All() {
		for _, s := range []string{p.Images.Icon, p.Images.Left, p.Images.Right, p.Images.Box} {
			check(s)
		}
		for _, c := range p.Colors {
			for _, s := range []string{c.Icon, c.Left, c.Right, c.Box} {
				check(s)
			}
		}
	}
}
