package cli

import (
	"context"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/lxzan/gws"
	"github.com/spf13/cobra"
	"github.com/teslamotors/fleet-telemetry/protos"

	"github.com/bttnns/red-switchboard/internal/mock"
)

// mockstream.go folds the two former cmd/ e2e doubles into `redswitchboard mock`
// subcommands, so the single binary can drive the streaming paths of the
// integration test:
//
//   mock fleet-push   - a fake vehicle that pushes one Fleet Telemetry protobuf
//                       frame to a tesla-fleet-stream-v1 mTLS listener (the source).
//   mock owner-stream - a fake Tesla Owner streaming SERVER that the
//                       tesla-owner-stream-v1 source dials, pushing data:update.

// newMockFleetPushCmd dials a Fleet Telemetry mTLS listener with a client cert
// and pushes one driving protobuf Payload, then exits.
func newMockFleetPushCmd() *cobra.Command {
	var url, clientCert, clientKey, serverCA, vin string
	cmd := &cobra.Command{
		Use:   "fleet-push",
		Short: "push one Fleet Telemetry protobuf frame to a stream source (mTLS)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			caPEM, err := os.ReadFile(serverCA)
			if err != nil {
				return fmt.Errorf("mock fleet-push: read ca: %w", err)
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(caPEM)
			cert, err := os.ReadFile(clientCert)
			if err != nil {
				return fmt.Errorf("mock fleet-push: read client cert: %w", err)
			}
			key, err := os.ReadFile(clientKey)
			if err != nil {
				return fmt.Errorf("mock fleet-push: read client key: %w", err)
			}

			// A driving frame: speed, soc, location, gear D, odometer, range. These
			// are the fields tesla-fleet-stream-v1's DecodePayload maps; the pushed
			// frame fills the cache and flows out the streaming sink.
			payload := &protos.Payload{
				Vin: vin,
				Data: []*protos.Datum{
					{Key: protos.Field_VehicleSpeed, Value: &protos.Value{Value: &protos.Value_FloatValue{FloatValue: 26.8}}}, // ~60mph
					{Key: protos.Field_Soc, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 78}}},
					{Key: protos.Field_Location, Value: &protos.Value{Value: &protos.Value_LocationValue{LocationValue: &protos.LocationValue{Latitude: 37.7749, Longitude: -122.4194}}}},
					{Key: protos.Field_Gear, Value: &protos.Value{Value: &protos.Value_ShiftStateValue{ShiftStateValue: protos.ShiftState_ShiftStateD}}},
					{Key: protos.Field_Odometer, Value: &protos.Value{Value: &protos.Value_FloatValue{FloatValue: 19893.4}}}, // ~12345mi
					{Key: protos.Field_RatedRange, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 338}}},       // ~210mi
				},
			}

			v, err := mock.NewStreamVehicle(url, "test-txid", payload, cert, key, pool)
			if err != nil {
				return fmt.Errorf("mock fleet-push: new vehicle: %w", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := v.Connect(ctx); err != nil {
				return fmt.Errorf("mock fleet-push: connect/push: %w", err)
			}
			// Give the listener a moment to decode + merge before we exit and close.
			time.Sleep(time.Second)
			log.Printf("mock fleet-push: pushed 1 Fleet Telemetry frame for VIN %s", vin)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&url, "url", "", "ws(s):// URL of the Fleet Telemetry listener (required)")
	f.StringVar(&clientCert, "client-cert", "", "mTLS client cert PEM (required)")
	f.StringVar(&clientKey, "client-key", "", "mTLS client key PEM (required)")
	f.StringVar(&serverCA, "ca", "", "CA PEM that signed the listener's server cert (required)")
	f.StringVar(&vin, "vin", "7FCTGAAA0NN000001", "VIN to stamp on the pushed frame")
	_ = cmd.MarkFlagRequired("url")
	_ = cmd.MarkFlagRequired("client-cert")
	_ = cmd.MarkFlagRequired("client-key")
	_ = cmd.MarkFlagRequired("ca")
	return cmd
}

// newMockOwnerStreamCmd runs a fake Tesla Owner streaming server: it accepts the
// data:subscribe_oauth handshake and pushes a driving data:update frame on an
// interval to every connected source, until interrupted.
func newMockOwnerStreamCmd() *cobra.Command {
	var addr, vin string
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "owner-stream",
		Short: "run a fake Tesla Owner streaming server (the owner stream source dials it)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runOwnerStreamServer(ctx, addr, vin, interval)
		},
	}
	f := cmd.Flags()
	f.StringVar(&addr, "addr", ":6000", "listen address")
	f.StringVar(&vin, "vin", "7FCTGAAA0NN000001", "VIN (tag) on the pushed data:update frames")
	f.DurationVar(&interval, "interval", 2*time.Second, "how often to push a data:update frame")
	return cmd
}

// ownerStreamHandler tracks connected sources so the push loop can broadcast.
type ownerStreamHandler struct {
	mu    *sync.Mutex
	conns *[]*gws.Conn
}

func (h *ownerStreamHandler) OnOpen(socket *gws.Conn) {
	h.mu.Lock()
	*h.conns = append(*h.conns, socket)
	h.mu.Unlock()
}
func (h *ownerStreamHandler) OnMessage(*gws.Conn, *gws.Message) {}
func (h *ownerStreamHandler) OnClose(socket *gws.Conn, _ error) {
	h.mu.Lock()
	for i, c := range *h.conns {
		if c == socket {
			*h.conns = append((*h.conns)[:i], (*h.conns)[i+1:]...)
			break
		}
	}
	h.mu.Unlock()
}
func (h *ownerStreamHandler) OnPing(*gws.Conn, []byte) {}
func (h *ownerStreamHandler) OnPong(*gws.Conn, []byte) {}

// runOwnerStreamServer serves /streaming/ and pushes a driving data:update frame
// every interval. Columns (TeslaMate's order): ts,speed,odometer,soc,elevation,
// est_heading,est_lat,est_lng,power,shift_state,range,est_range,heading.
func runOwnerStreamServer(ctx context.Context, addr, vin string, interval time.Duration) error {
	var (
		mu    sync.Mutex
		conns []*gws.Conn
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/streaming/", func(w http.ResponseWriter, r *http.Request) {
		upgrader := gws.NewUpgrader(&ownerStreamHandler{mu: &mu, conns: &conns}, &gws.ServerOption{
			ReadMaxPayloadSize: 1 << 20,
			HandshakeTimeout:   5 * time.Second,
		})
		conn, err := upgrader.Upgrade(w, r)
		if err != nil {
			return
		}
		conn.ReadLoop()
	})

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
				val := ts + ",60,12345.6,78,120,90,37.7749,-122.4194,0,D,210,205,90"
				frame := `{"msg_type":"data:update","tag":"` + vin + `","value":"` + val + `"}`
				mu.Lock()
				cs := append([]*gws.Conn(nil), conns...)
				mu.Unlock()
				for _, c := range cs {
					_ = c.WriteMessage(gws.OpcodeText, []byte(frame))
				}
			}
		}
	}()

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Printf("mock owner-stream: listening on %s (pushing data:update every %s for VIN %s)", addr, interval, vin)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("mock owner-stream: %w", err)
	}
	return nil
}
