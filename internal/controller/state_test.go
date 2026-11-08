package controller

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestLegacyStateMigrationPreservesIdentityAndRejectsActiveJournal(t *testing.T) {
	for _, active := range []bool{false, true} {
		f := newFixture(t)
		legacy := State{Version: 1, Owner: f.c.State.Owner, Scope: "https://github.com/clcdpc|CLC-Runners|clc-linux|", ScaleSetID: 42, Runners: map[string]*Runner{}}
		if active {
			f.scale(t, 1)
			legacy.Runners = f.c.State.Runners
		}
		b, err := json.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(f.path, b, 0600); err != nil {
			t.Fatal(err)
		}
		s, err := LoadState(f.path, f.c.Config)
		if active {
			if err == nil {
				t.Fatal("active legacy journal migrated")
			}
			after, _ := os.ReadFile(f.path)
			if string(after) != string(b) {
				t.Fatal("failed migration modified journal")
			}
			continue
		}
		if err != nil || s.Version != 2 || s.Owner != legacy.Owner || s.ScaleSetID != 42 {
			t.Fatal("migration changed permanent identity", s, err)
		}
		backup, err := os.ReadFile(f.path + ".wslc-v1.bak")
		if err != nil || string(backup) != string(b) {
			t.Fatal("exact migration backup missing")
		}
		if _, err = LoadState(f.path, f.c.Config); err != nil {
			t.Fatal("repeat migration not idempotent", err)
		}
	}
}

func TestStateSurvivesAtomicRoundTripAndRejectsWrongScope(t *testing.T) {
	f := newFixture(t)
	f.scale(t, 1)
	s, err := LoadState(f.path, f.c.Config)
	if err != nil {
		t.Fatal(err)
	}
	if s.Owner != f.c.State.Owner || len(s.Runners) != 1 {
		t.Fatal("state did not round trip")
	}
	c := f.c.Config
	c.HyperV.Subnet = "192.168.232.0/24"
	if _, err := LoadState(f.path, c); err == nil {
		t.Fatal("accepted different Hyper-V scope")
	}
	b, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.path, []byte(strings.ReplaceAll(string(b), f.name(), "../../foreign")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadState(f.path, f.c.Config); err == nil {
		t.Fatal("accepted path traversal in runner state")
	}
}
func TestCorruptStateFailsClosed(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(f.path, f.c.Config); err == nil {
		t.Fatal("silently replaced corrupt state")
	}
}

func TestLegacyReadOnlyInspectionDoesNotOverwriteConcurrentStartupState(t *testing.T) {
	f := newFixture(t)
	legacy := State{Version: 1, Owner: f.c.State.Owner, Scope: "https://github.com/clcdpc|CLC-Runners|clc-linux|", ScaleSetID: 42, Runners: map[string]*Runner{}}
	before, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.path, before, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := InspectState(f.path, f.c.Config)
	if err != nil || !s.MigrationRequired || s.Owner != legacy.Owner || s.ScaleSetID != 42 {
		t.Fatal("read-only legacy inspection failed", s, err)
	}
	after, err := os.ReadFile(f.path)
	if err != nil || string(after) != string(before) {
		t.Fatal("read-only inspection rewrote durable state", err)
	}
	if _, err = os.Stat(f.path + ".wslc-v1.bak"); !os.IsNotExist(err) {
		t.Fatal("inspection performed a migration side effect")
	}
}
