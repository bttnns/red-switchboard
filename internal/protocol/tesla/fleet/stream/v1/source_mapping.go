package v1

// source_mapping.go is the Fleet Telemetry -> canonical half: the analogue of
// fleet/poll/v1's DecodeVehicleData, but the input is a Tesla Fleet Telemetry
// protobuf Payload (pushed over mTLS) instead of a REST vehicle_data JSON blob.
// Only the streamed live fields are populated (Location/Speed/Heading/Gear/
// Odometer/SOC/Range/charge-power/charge-energy); the poll loop owns the rest. Unknown Datum
// keys are ignored (forward-compat: Tesla adds Field values; we must not crash).
//
// Verified against github.com/teslamotors/fleet-telemetry@v0.9.1. Re-verify the
// Field enum and Value oneof on every pin bump (Tesla renames Field values across
// releases); the fixture in source_mapping_test.go is the guard.

import (
	"math"
	"strconv"
	"strings"
	"time"

	fleetcsv "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	"github.com/bttnns/red-switchboard/internal/units"
	"github.com/bttnns/red-switchboard/internal/vehicle"
	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
)

// DecodePayload maps a Fleet Telemetry protobuf Payload into the canonical
// vehicle.Snapshot. It takes the already-deserialized *protos.Payload (extracted
// from a telemetry.Record by payloadOf) and walks its Data slice. Only the
// fields a frame carries are marked present (StreamPresent); the cache merges
// field-by-field so a delta frame cannot zero a field it omitted. Liveness is
// set by the cache merge (a frame with data implies awake); an empty/keepalive
// frame stays zeroish and is dropped before the merge.
//
// Units: Fleet Telemetry carries vehicle display units, not a fixed system. The
// fields below assume US display units (miles/mph), matching what the REST source
// decodes; a metric-region car emits km/km-h. The fixture pins the US case.
func DecodePayload(payload *protos.Payload) vehicle.Snapshot {
	if payload == nil {
		return vehicle.Snapshot{}
	}
	st := &vehicle.State{}
	var live *vehicle.LiveSession
	var present vehicle.StreamField
	// Fleet Telemetry frames carry no per-field timestamp we can trust, so the
	// decoder's frame time is receive time; the cache derives StreamFields from
	// this same FetchedAt. Stamp the Location with it so the fix is never Go-zero.
	now := time.Now()
	for _, d := range payload.GetData() {
		if d == nil {
			continue
		}
		switch d.GetKey() {
		case protos.Field_VehicleSpeed:
			st.SpeedMps = units.MphToMps(numVal(d))
			present |= vehicle.StreamSpeed
		case protos.Field_Odometer:
			st.OdometerMeters = int(math.Round(units.MilesToMeters(numVal(d))))
			present |= vehicle.StreamOdometer
		case protos.Field_Soc:
			st.BatteryLevelPct = numVal(d)
			present |= vehicle.StreamSOC
		case protos.Field_Gear:
			st.Gear = gearVal(d)
			present |= vehicle.StreamGear
		case protos.Field_GpsHeading:
			st.HeadingDeg = numVal(d)
			present |= vehicle.StreamHeading
		case protos.Field_Location:
			if lat, lng, ok := locationVal(d); ok {
				st.Location = &vehicle.Location{Latitude: lat, Longitude: lng, TimeStamp: now}
				present |= vehicle.StreamLoc
			}
		case protos.Field_RatedRange:
			st.RangeKm = int(math.Round(units.MilesToKm(numVal(d))))
			present |= vehicle.StreamRange
		case protos.Field_InsideTemp:
			// Tesla telemetry temps are already Celsius, as is the canonical field.
			st.CabinTempC = numVal(d)
			present |= vehicle.StreamCabinTemp
		case protos.Field_OutsideTemp:
			st.OutsideTempC = numVal(d)
			present |= vehicle.StreamOutsideTemp
		case protos.Field_TpmsPressureFl:
			// Tesla telemetry TPMS pressures are in bar, matching the canonical field.
			st.TpmsPressureFlBar = numVal(d)
			present |= vehicle.StreamTpmsFl
		case protos.Field_TpmsPressureFr:
			st.TpmsPressureFrBar = numVal(d)
			present |= vehicle.StreamTpmsFr
		case protos.Field_TpmsPressureRl:
			st.TpmsPressureRlBar = numVal(d)
			present |= vehicle.StreamTpmsRl
		case protos.Field_TpmsPressureRr:
			st.TpmsPressureRrBar = numVal(d)
			present |= vehicle.StreamTpmsRr
		case protos.Field_DCChargingPower, protos.Field_ACChargingPower:
			// Surface even a 0 reading (charge stopped): the merge keeps the last
			// non-zero power for the served session, but the cache uses the 0 to
			// detect charge-end and trigger a terminal poll.
			if live == nil {
				live = &vehicle.LiveSession{}
			}
			live.PowerKw = numVal(d)
			present |= vehicle.StreamChargePower
		case protos.Field_DCChargingEnergyIn, protos.Field_ACChargingEnergyIn:
			// Energy-in (kWh) is cumulative for the session, the same semantic as
			// TeslaMate's required charge_energy_added; without this a fully-streamed
			// charge would have no kWh (the poll's LiveSession was the only source).
			if live == nil {
				live = &vehicle.LiveSession{}
			}
			live.TotalChargedEnergy = numVal(d)
			present |= vehicle.StreamChargeEnergyIn
		case protos.Field_ChargeLimitSoc:
			st.BatteryLimitPct = numVal(d)
			present |= vehicle.StreamChargeLimit
		case protos.Field_TimeToFullCharge:
			// Telemetry reports hours; the canonical field is minutes.
			st.TimeToEndOfChargeMin = int(math.Round(numVal(d) * 60.0))
			present |= vehicle.StreamTimeToFull
		case protos.Field_ChargerVoltage:
			st.ChargerVoltageV = int(math.Round(numVal(d)))
			present |= vehicle.StreamChargerVoltage
		case protos.Field_ChargeAmps:
			if live == nil {
				live = &vehicle.LiveSession{}
			}
			live.CurrentA = numVal(d)
			present |= vehicle.StreamChargeCurrent
		case protos.Field_DetailedChargeState:
			st.Charger = chargerVal(d)
			st.Plug = plugVal(d)
			present |= vehicle.StreamChargeState
		case protos.Field_BatteryHeaterOn:
			st.BatteryHeaterOn = boolVal(d)
			present |= vehicle.StreamBatteryHeater
		case protos.Field_Locked:
			lock := lockedStr(boolVal(d))
			st.DoorFrontLeftLocked = lock
			st.DoorFrontRightLocked = lock
			st.DoorRearLeftLocked = lock
			st.DoorRearRightLocked = lock
			present |= vehicle.StreamLocked
		case protos.Field_SentryMode:
			st.GearGuardStatus = sentryVal(d)
			present |= vehicle.StreamSentry
		}
	}
	return vehicle.Snapshot{State: st, Live: live, FetchedAt: now, StreamPresent: present}
}

// payloadOf extracts the protos.Payload from a telemetry.Record. Record.protoMessage
// is private and there is no exported decoded-payload getter, so payloadOf always
// proto.Unmarshal's the record's payload bytes. transmitDecodedRecords only changes
// whether those bytes are the re-marshaled decoded form vs the raw frame; either
// unmarshals to the same Payload.
func payloadOf(payloadBytes []byte) *protos.Payload {
	if len(payloadBytes) == 0 {
		return nil
	}
	var p protos.Payload
	if err := proto.Unmarshal(payloadBytes, &p); err != nil {
		return nil
	}
	return &p
}

// numVal coalesces the numeric Datum.Value oneof variants. A field's type varies
// by firmware (Speed/SOC/Range may be Int, Float, Long, or Double), so reading
// only GetDoubleValue() would silently yield 0; this mirrors the datastore's
// type switch.
func numVal(d *protos.Datum) float64 {
	oneof := d.GetValue().GetValue()
	if oneof == nil {
		return 0
	}
	switch vv := oneof.(type) {
	case *protos.Value_DoubleValue:
		return vv.DoubleValue
	case *protos.Value_FloatValue:
		return float64(vv.FloatValue)
	case *protos.Value_IntValue:
		return float64(vv.IntValue)
	case *protos.Value_LongValue:
		return float64(vv.LongValue)
	case *protos.Value_StringValue:
		f, _ := strconv.ParseFloat(vv.StringValue, 64)
		return f
	default:
		return 0
	}
}

// gearVal maps Field_Gear to the canonical Gear. Modern firmware sends a
// ShiftState enum (ShiftState.String() is "ShiftStateP" etc., NOT "P"); older
// firmware sends a bare "P"/"R"/"N"/"D" string. Handle both, then reuse the REST
// source's letter->Gear map for the final step.
func gearVal(d *protos.Datum) vehicle.Gear {
	oneof := d.GetValue().GetValue()
	if oneof == nil {
		return vehicle.GearUnknown
	}
	switch vv := oneof.(type) {
	case *protos.Value_ShiftStateValue:
		return fleetcsv.GearFromTesla(strings.TrimPrefix(vv.ShiftStateValue.String(), "ShiftState"))
	case *protos.Value_StringValue:
		return fleetcsv.GearFromTesla(vv.StringValue)
	default:
		return vehicle.GearUnknown
	}
}

// chargeStateStr resolves Field_DetailedChargeState to the bare Tesla
// charging_state name ("Charging"/"Complete"/...). Modern firmware sends the typed
// DetailedChargeStateValue enum (String() is "DetailedChargeStateComplete", NOT
// "Complete"); older firmware may send a bare string. Trim the enum prefix so both
// feed the shared poll table.
func chargeStateStr(d *protos.Datum) string {
	oneof := d.GetValue().GetValue()
	switch vv := oneof.(type) {
	case *protos.Value_DetailedChargeStateValue:
		return strings.TrimPrefix(vv.DetailedChargeStateValue.String(), "DetailedChargeState")
	case *protos.Value_StringValue:
		return strings.TrimPrefix(vv.StringValue, "DetailedChargeState")
	default:
		return ""
	}
}

func chargerVal(d *protos.Datum) vehicle.ChargerState {
	return fleetcsv.ChargerFromTesla(chargeStateStr(d))
}

func plugVal(d *protos.Datum) vehicle.ChargePlug {
	return fleetcsv.PlugFromTesla(chargeStateStr(d))
}

// boolVal coalesces a boolean Datum: the typed BooleanValue, or a numeric/string
// fallback for firmware that encodes the flag differently.
func boolVal(d *protos.Datum) bool {
	switch vv := d.GetValue().GetValue().(type) {
	case *protos.Value_BooleanValue:
		return vv.BooleanValue
	case *protos.Value_StringValue:
		return strings.EqualFold(vv.StringValue, "true")
	default:
		return numVal(d) != 0
	}
}

// lockedStr mirrors poll source_mapping.go's lockedFromTesla: the Tesla wire
// carries one fleet-wide locked bit, so every door round-trips to the same value.
func lockedStr(locked bool) string {
	if locked {
		return "locked"
	}
	return "unlocked"
}

// sentryVal maps Field_SentryMode to the canonical Gear Guard status, matching the
// poll path's gearGuardFromTesla ("enabled"/"disabled"). Modern firmware sends a
// typed SentryModeState enum; treat any active state (not Unknown/Off) as enabled.
func sentryVal(d *protos.Datum) string {
	on := false
	switch vv := d.GetValue().GetValue().(type) {
	case *protos.Value_SentryModeStateValue:
		s := vv.SentryModeStateValue
		on = s != protos.SentryModeState_SentryModeStateUnknown && s != protos.SentryModeState_SentryModeStateOff
	default:
		on = boolVal(d)
	}
	if on {
		return "enabled"
	}
	return "disabled"
}

// locationVal extracts lat/lng from a Field_Location Datum (Value_LocationValue).
func locationVal(d *protos.Datum) (lat, lng float64, ok bool) {
	if lv := d.GetValue().GetLocationValue(); lv != nil {
		return lv.GetLatitude(), lv.GetLongitude(), true
	}
	return 0, 0, false
}
