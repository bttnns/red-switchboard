package streamsink

import (
	"context"
	"log"
	"net/http"
	"testing"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// fakeSink is a minimal StreamSink for the registry round-trip.
type fakeSink struct{ name string }

func (f *fakeSink) Name() string                   { return f.name }
func (f *fakeSink) Handler() (http.Handler, error) { return http.NotFoundHandler(), nil }
func (f *fakeSink) Run(context.Context) error      { return nil }

// fakeWatcher is a minimal CacheWatcher.
type fakeWatcher struct{}

func (fakeWatcher) Latest(string) vehicle.Snapshot { return vehicle.Snapshot{} }
func (fakeWatcher) Vehicles() []vehicle.Identity   { return nil }
func (fakeWatcher) Subscribe(context.Context, string) <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestRegisterOpenNames(t *testing.T) {
	name := "test-stream-sink-v1"
	Register(name, func(_ *yaml.Node, _ CacheWatcher, _ *log.Logger) (StreamSink, error) {
		return &fakeSink{name: name}, nil
	})

	got, err := Open(name, nil, fakeWatcher{}, nil)
	require.NoError(t, err)
	assert.Equal(t, name, got.Name())
	assert.Contains(t, Names(), name)

	_, err = Open("nope", nil, fakeWatcher{}, nil)
	require.Error(t, err)
}
