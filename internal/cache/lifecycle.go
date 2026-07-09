// Lifecycle: the streaming shutdown coordinator. Streaming plugs into the same
// signal ctx as the REST server; on ctx.Done it stops accepting new streaming
// connections, drains in-flight frames within a bounded timeout, then runs the
// after callback (which shuts the REST server). The streaming sink and any
// streaming source close their own listeners on ctx.Done; ShutdownOn enforces the
// ordered drain so a hung WS cannot block process exit.
package cache

import (
	"context"
	"time"
)

// ShutdownOn blocks until ctx is cancelled, then runs after under a bounded
// timeout. It is the streaming side of the ordered shutdown: the streaming sink
// and source close their own listeners on ctx.Done (their Run loops return), and
// after runs the REST server's Shutdown. Call once in a goroutine from serve.
func (s *Service) ShutdownOn(ctx context.Context, after func()) {
	<-ctx.Done()
	if after == nil {
		return
	}
	// Bound the drain so a misbehaving consumer cannot stall exit. The configured
	// server.shutdown_timeout is the natural cap; the caller wraps it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		after()
	}()
	select {
	case <-done:
	case <-time.After(shutdownDrainTimeout):
	}
}

// shutdownDrainTimeout bounds the graceful drain. It mirrors the REST server's
// shutdown_timeout default; serve passes its own bounded after, so this is a
// backstop against a hung after callback rather than the primary cap.
const shutdownDrainTimeout = 10 * time.Second
