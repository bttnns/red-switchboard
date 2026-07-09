package v1

// vehicleStateQuery builds the gateway GetVehicleState request body for the
// given vehicle GUID. It requests the poll-safe (sans-TPMS,
// sans-@subscriptionOnly) field set: every scalar is wrapped as
// `{ timeStamp value }`, gnssLocation is `{ latitude longitude timeStamp }`,
// and cloudConnection is `{ isOnline lastSync }`. Numeric tirePressure*,
// chargingTripTarget*, activeDriverName and the other subscription-only fields
// are intentionally omitted because they error over HTTP polling (mirrors HA's
// VEHICLE_STATE_SANS_TPMS_API_FIELDS).
func vehicleStateQuery(vehicleID string) string {
	const vt = "{ timeStamp value }"
	fields := "" +
		"cloudConnection { isOnline lastSync } " +
		"gnssLocation { latitude longitude timeStamp } " +
		"gnssSpeed " + vt + " " +
		"gnssBearing " + vt + " " +
		"gnssAltitude " + vt + " " +
		"vehicleMileage " + vt + " " +
		"distanceToEmpty " + vt + " " +
		"batteryLevel " + vt + " " +
		"batteryLimit " + vt + " " +
		"batteryCapacity " + vt + " " +
		"powerState " + vt + " " +
		"gearStatus " + vt + " " +
		"driveMode " + vt + " " +
		"chargerState " + vt + " " +
		"chargerStatus " + vt + " " +
		"chargePortState " + vt + " " +
		"timeToEndOfCharge " + vt + " " +
		"cabinClimateInteriorTemperature " + vt + " " +
		"cabinClimateDriverTemperature " + vt + " " +
		"cabinPreconditioningStatus " + vt + " " +
		"defrostDefogStatus " + vt + " " +
		"seatFrontLeftHeat " + vt + " " +
		"seatFrontRightHeat " + vt + " " +
		"seatRearLeftHeat " + vt + " " +
		"seatRearRightHeat " + vt + " " +
		"steeringWheelHeat " + vt + " " +
		"gearGuardVideoStatus " + vt + " " +
		"tirePressureStatusFrontLeft " + vt + " " +
		"tirePressureStatusFrontRight " + vt + " " +
		"tirePressureStatusRearLeft " + vt + " " +
		"tirePressureStatusRearRight " + vt + " " +
		"doorFrontLeftLocked " + vt + " " +
		"doorFrontLeftClosed " + vt + " " +
		"doorFrontRightLocked " + vt + " " +
		"doorFrontRightClosed " + vt + " " +
		"doorRearLeftLocked " + vt + " " +
		"doorRearLeftClosed " + vt + " " +
		"doorRearRightLocked " + vt + " " +
		"doorRearRightClosed " + vt + " " +
		"windowFrontLeftClosed " + vt + " " +
		"windowFrontRightClosed " + vt + " " +
		"windowRearLeftClosed " + vt + " " +
		"windowRearRightClosed " + vt + " " +
		"closureFrunkLocked " + vt + " " +
		"closureFrunkClosed " + vt + " " +
		"closureLiftgateLocked " + vt + " " +
		"closureLiftgateClosed " + vt + " " +
		"closureTonneauLocked " + vt + " " +
		"closureTonneauClosed " + vt + " " +
		"closureTailgateLocked " + vt + " " +
		"closureTailgateClosed " + vt + " " +
		"gearGuardLocked " + vt + " " +
		"otaCurrentVersion " + vt + " " +
		"otaAvailableVersion " + vt + " " +
		"otaStatus " + vt + " " +
		"otaInstallProgress " + vt + " " +
		"otaDownloadProgress " + vt + " "

	query := "query GetVehicleState($vehicleID: String!) { vehicleState(id: $vehicleID) { " + fields + "} }"

	return marshalGraphQL("GetVehicleState", query, map[string]any{"vehicleID": vehicleID})
}

// liveSessionQuery builds the chrg getLiveSessionData request body. Value-record
// fields are wrapped as `{ value updatedAt }`; soc/timeRemaining/currentPrice
// vary by deployment so we request value records where the python clients do
// and bare scalars otherwise.
func liveSessionQuery(vehicleID string) string {
	const vr = "{ value updatedAt }"
	fields := "" +
		"__typename " +
		"vehicleChargerState " + vr + " " +
		"power " + vr + " " +
		"current " + vr + " " +
		"kilometersChargedPerHour " + vr + " " +
		"rangeAddedThisSession " + vr + " " +
		"soc " + vr + " " +
		"timeRemaining " + vr + " " +
		"totalChargedEnergy " + vr + " " +
		"timeElapsed " +
		"currentPrice " +
		"currentCurrency "

	query := "query getLiveSessionData($vehicleId: ID!) { getLiveSessionData(vehicleId: $vehicleId) { " + fields + "} }"

	return marshalGraphQL("getLiveSessionData", query, map[string]any{"vehicleId": vehicleID})
}
