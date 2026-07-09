package v1

// THE SINK MAP for tesla-owner-poll-v1 (canonical -> vendor wire).
//
// The Owner API output surface is the same Tesla vehicle_data shape as the Fleet
// API, so the canonical -> vehicle_data encode is shared with tesla-fleet-poll-v1
// rather than duplicated here: the Owner sink serves the data through
// teslafleet.BuildHandlerSampler (see sink.go), which internally calls
// tesla-fleet-poll-v1's VehicleData encoder. The authoritative, documented field-correspondence table
// (canonical vehicle.State <-> vehicle_data field) lives in
// internal/protocol/tesla/fleet/poll/v1/sink_mapping.go. This file exists so the
// mapping for tesla-owner-poll-v1 has one discoverable, documented home that points at
// the shared implementation; the Owner-specific differences are in the transport,
// not the field mapping.
