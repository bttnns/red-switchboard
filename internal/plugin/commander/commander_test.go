package commander

import (
	"context"
	"errors"
	"log"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeCommander is a test plugin that records the last call and returns a
// canned ack, used by the registry round-trip test.
type fakeCommander struct {
	name   string
	lastID string
	last   string
	params map[string]any
}

func (f *fakeCommander) Name() string { return f.name }
func (f *fakeCommander) SendCommand(_ context.Context, id, name string, params map[string]any) (Ack, error) {
	f.lastID, f.last, f.params = id, name, params
	return Ack{Result: true}, nil
}

func TestRegisterOpenNames(t *testing.T) {
	// Register under a test-only name (panic-free since the registry is global;
	// a duplicate would panic, so use a unique name per test run via t.Name).
	name := t.Name()
	Register(name, func(_ *yaml.Node, _ *log.Logger) (Commander, error) {
		return &fakeCommander{name: name}, nil
	})

	if got := Names(); !contains(got, name) {
		t.Fatalf("Names() = %v, want %q present", got, name)
	}

	c, err := Open(name, nil, log.Default())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c.Name() != name {
		t.Errorf("Name = %q, want %q", c.Name(), name)
	}

	ack, err := c.SendCommand(context.Background(), "VIN", "charge_start", map[string]any{"foo": 1})
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if !ack.Result {
		t.Errorf("ack.Result = false, want true")
	}
	fc := c.(*fakeCommander)
	if fc.lastID != "VIN" || fc.last != "charge_start" {
		t.Errorf("recorded call = %q/%q, want VIN/charge_start", fc.lastID, fc.last)
	}
}

func TestOpenUnknown(t *testing.T) {
	if _, err := Open("does-not-exist", nil, log.Default()); err == nil {
		t.Fatal("Open of unknown plugin succeeded; want error")
	}
}

func TestRegisterNilPanics(t *testing.T) {
	defer func() {
		if err := recover(); err == nil {
			t.Fatal("Register(nil) did not panic")
		}
	}()
	Register("commander-nil-panic-test", nil)
}

func TestRegisterDuplicatePanics(t *testing.T) {
	name := t.Name() + "-dup"
	Register(name, func(_ *yaml.Node, _ *log.Logger) (Commander, error) {
		return &fakeCommander{name: name}, nil
	})
	defer func() {
		if err := recover(); err == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	Register(name, func(_ *yaml.Node, _ *log.Logger) (Commander, error) {
		return nil, errors.New("unreachable")
	})
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
