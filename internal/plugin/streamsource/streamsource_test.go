package streamsource

import (
	"context"
	"log"
	"testing"

	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// fakeSourceAdapter is a minimal StreamSource for the registry round-trip.
type fakeSourceAdapter struct{ name string }

func (f *fakeSourceAdapter) Name() string                                         { return f.name }
func (f *fakeSourceAdapter) Vehicles(context.Context) ([]vehicle.Identity, error) { return nil, nil }
func (f *fakeSourceAdapter) Run(context.Context, RecordSink, CacheWatcher) error  { return nil }

func TestRegisterOpenNames(t *testing.T) {
	// Register is package-global and panics on dup; use a distinct name per test.
	name := "test-stream-source-v1"
	Register(name, func(_ *yaml.Node, _ *log.Logger) (StreamSource, error) {
		return &fakeSourceAdapter{name: name}, nil
	})

	got, err := Open(name, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, name, got.Name())

	assert.Contains(t, Names(), name)

	_, err = Open("does-not-exist", nil, nil)
	require.Error(t, err)
}
