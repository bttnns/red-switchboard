package mock

// stream_vehicle.go is a fake-vehicle test double for the Fleet Telemetry
// source: it dials the source's mTLS WebSocket listener with a client cert,
// builds a Fleet Telemetry protobuf Payload, wraps it in the flatbuffers stream
// envelope (tesla.FlatbuffersStreamToBytes, the same encoder the real vehicle
// SDK uses), and pushes it as a binary WS frame. The source integration test
// uses it to assert the cache receives a merged snapshot from a pushed frame.
// It uses gws (the same WS library the source uses) and the broker-free
// fleet-telemetry messages/tesla + protos packages, so the test exercises the
// real wire path end to end.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"time"

	"github.com/lxzan/gws"
	"github.com/teslamotors/fleet-telemetry/messages/tesla"
	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
)

// StreamVehicle is a fake vehicle that pushes one Fleet Telemetry frame.
type StreamVehicle struct {
	url     string // ws(s)://host:port/path
	tlsCfg  *tls.Config
	txid    string
	topic   string
	payload *protos.Payload
	done    chan struct{}
	conn    *gws.Conn
}

// vehicleHandler absorbs the source's ack frames (and ignores everything else).
type vehicleHandler struct {
	sv *StreamVehicle
}

func (h *vehicleHandler) OnOpen(socket *gws.Conn)           { h.sv.conn = socket }
func (h *vehicleHandler) OnMessage(*gws.Conn, *gws.Message) {}
func (h *vehicleHandler) OnClose(*gws.Conn, error) {
	select {
	case <-h.sv.done:
	default:
		close(h.sv.done)
	}
}
func (h *vehicleHandler) OnPing(*gws.Conn, []byte) {}
func (h *vehicleHandler) OnPong(*gws.Conn, []byte) {}

// NewStreamVehicle builds a fake vehicle that will dial url and push payload
// (a protos.Payload) as one vehicle_data ("V") frame. clientCert/clientKey are
// the mTLS client credentials; serverCA is the CA that signed the source's
// server cert (so the vehicle trusts it).
func NewStreamVehicle(url, txid string, payload *protos.Payload, clientCert, clientKey []byte, serverCA *x509.CertPool) (*StreamVehicle, error) {
	cert, err := tls.X509KeyPair(clientCert, clientKey)
	if err != nil {
		return nil, err
	}
	return &StreamVehicle{
		url: url,
		tlsCfg: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			RootCAs:            serverCA,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: serverCA == nil, // test double: the source's mTLS is the property under test
		},
		txid:    txid,
		topic:   "V",
		payload: payload,
		done:    make(chan struct{}),
	}, nil
}

// Connect dials the listener, pushes the frame, and starts a background read
// loop (so the source's ack is read and the connection stays up until Close).
func (v *StreamVehicle) Connect(ctx context.Context) error {
	h := &vehicleHandler{sv: v}
	conn, _, err := gws.NewClient(h, &gws.ClientOption{
		Addr:             v.url,
		TlsConfig:        v.tlsCfg,
		HandshakeTimeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	v.conn = conn
	go conn.ReadLoop()
	// Wait for OnOpen.
	time.Sleep(50 * time.Millisecond)
	frame, err := v.frame()
	if err != nil {
		return err
	}
	return conn.WriteMessage(gws.OpcodeBinary, frame)
}

// frame builds the on-wire Fleet Telemetry message: the protobuf Payload wrapped
// in the flatbuffers stream envelope with topic "V".
func (v *StreamVehicle) frame() ([]byte, error) {
	buf, err := proto.Marshal(v.payload)
	if err != nil {
		return nil, err
	}
	// FlatbuffersStreamToBytes(senderID, messageTopic, txid, payload, createdAt,
	// envMessageID, deviceType, deviceID, deliveredAtEpochMs).
	return tesla.FlatbuffersStreamToBytes(
		[]byte("vehicle_device."+v.deviceID()),
		[]byte(v.topic),
		[]byte(v.txid),
		buf,
		uint32(time.Now().Unix()),
		nil, nil, []byte(v.deviceID()),
		uint64(time.Now().UnixMilli()),
	), nil
}

func (v *StreamVehicle) deviceID() string {
	// The source sets record.Vin = RequestIdentity.DeviceID, which comes from the
	// client cert CN (dots -> dashes). The test cert's CN is the VIN.
	if v.tlsCfg != nil && len(v.tlsCfg.Certificates) > 0 {
		for _, chain := range v.tlsCfg.Certificates[0].Certificate {
			if c, err := x509.ParseCertificate(chain); err == nil && c.Subject.CommonName != "" {
				return c.Subject.CommonName
			}
		}
	}
	return "VIN000"
}

// Done returns a channel closed when the connection ends.
func (v *StreamVehicle) Done() <-chan struct{} { return v.done }

// Close tears down the connection.
func (v *StreamVehicle) Close() {
	if v.conn != nil {
		_ = v.conn.WriteClose(1000, nil)
	}
}
