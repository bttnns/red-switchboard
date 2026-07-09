package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestModelFromVIN pins the Rivian VIN line-digit mapping (char 4: T=R1T, S=R1S,
// 2=R2) gated on a Rivian WMI, so a Tesla VIN (whose char 4 'S' would otherwise look
// like an R1S) does not decode. Serials are synthetic.
func TestModelFromVIN(t *testing.T) {
	cases := map[string]string{
		"7FCTGAAA0NN000001": "R1T", // 7FC truck WMI, char 4 T
		"7PDSGAAA3NN000002": "R1S", // 7PD SUV WMI, char 4 S
		"7PD2EAAB5VN000003": "R2",  // 7PD, char 4 2
		"5YJSA1E40LF000001": "",    // Tesla VIN: not a Rivian WMI
		"7FC":               "",    // too short
		"":                  "",
	}
	for vin, want := range cases {
		assert.Equalf(t, want, modelFromVIN(vin), "modelFromVIN(%q)", vin)
	}
}

// TestModelPrecedence: an explicit per-VIN config override wins, else the VIN is
// auto-detected, else the settings default applies.
func TestModelPrecedence(t *testing.T) {
	p := &Plugin{settings: Settings{
		Model: "R1T", // applyDefaults sets this when unset
		Vehicles: map[string]VehicleOverride{
			"7PDSGAAA3NN000002": {Model: "Launch Edition"},
		},
	}}

	assert.Equal(t, "Launch Edition", p.model("7PDSGAAA3NN000002"), "per-VIN config override wins")
	assert.Equal(t, "R2", p.model("7PD2EAAB5VN000003"), "VIN auto-detect for a decodable VIN")
	assert.Equal(t, "R1T", p.model("WMIUNKNOWN0000000"), "settings default for an undecodable VIN")
}
