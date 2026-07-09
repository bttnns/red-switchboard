// Package commander is the WRITE seam of the hub: the command analogue of
// source.Source / sink.Sink. A Commander sends a signed command to a vehicle and
// returns an ack. It is vendor-agnostic (a future Rivian command plugin plugs in
// the same way) and registered like the other seams via the database/sql driver
// pattern (Register/Open/Names, called from a plugin's init).
//
// Commands do NOT touch the cache: the cache is the read-decoupling layer, and a
// command is a one-shot write that goes straight to the vendor. (A plugin may
// optionally invalidate the cache on success so the next poll reflects the new
// state; that hook lives on the plugin, not the seam.) This package is
// VENDOR-AGNOSTIC: it imports only the stdlib + yaml, never any internal/protocol/*
// package, exactly like internal/source and internal/sink.
package commander

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// Ack is the vendor-neutral result of a command. The Tesla plugin maps the SDK's
// success/nominal-error outcomes into this; the REST route wraps it in the Tesla
// ack envelope {"response":{"result":...,"reason":...}}.
type Ack struct {
	Result bool   `json:"result"`
	Reason string `json:"reason"`
}

// Commander is a write plugin. SendCommand sends one command (e.g.
// "charge_start", "set_charge_limit", "wake_up") with plugin-defined params to
// the vehicle identified by id (the source-native id, a VIN for Tesla).
//
// A nominal error (the command was rejected by the vehicle for a known reason,
// e.g. "already charging") is returned as Ack{Result: false, Reason: ...} with a
// nil error, not as a Go error, so the route can answer 200 with the failure
// reason the way the real Tesla proxy does. An infrastructure error (network,
// auth, signing) is returned as a non-nil error so the route answers 5xx.
type Commander interface {
	// Name is the registry key (e.g. "tesla-command-v1").
	Name() string
	// SendCommand dispatches name with params to the vehicle. params may be nil.
	SendCommand(ctx context.Context, id string, name string, params map[string]any) (Ack, error)
}

// Factory constructs a Commander from its plugin-specific settings node and a
// logger. settings may be nil; the plugin applies its own defaults.
type Factory func(settings *yaml.Node, logger *log.Logger) (Commander, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a commander plugin under name. Plugins call this from init().
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		panic("commander: Register factory is nil")
	}
	if _, dup := factories[name]; dup {
		panic("commander: Register called twice for " + name)
	}
	factories[name] = f
}

// Open constructs the registered commander plugin named name.
func Open(name string, settings *yaml.Node, logger *log.Logger) (Commander, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("commander: unknown plugin %q (registered: %v)", name, Names())
	}
	return f(settings, logger)
}

// Names returns the registered commander plugin names, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
