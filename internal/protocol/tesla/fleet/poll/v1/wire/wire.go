// Package wire defines the Tesla Fleet API response types that the Tesla sink
// serves to stock TeslaMate. The structs mirror the shape TeslaMate decodes in
// lib/tesla_api/vehicle/state.ex; pointer fields marshal as JSON null when unset
// so we never fabricate values TeslaMate would otherwise treat as real.
package wire

// Response is the standard Tesla Fleet API envelope: {"response": <data>}.
type Response struct {
	Response interface{} `json:"response"`
}

// Product is an entry in GET /api/1/products. TeslaMate filters products to
// those that carry a vehicle_id, then treats them as cars.
type Product struct {
	ID          int64  `json:"id"`
	VehicleID   int64  `json:"vehicle_id"`
	VIN         string `json:"vin"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"`
}

// Summary is the lightweight vehicle object returned by GET /api/1/vehicles and
// GET /api/1/vehicles/{id} (the cheap state check used while a car is asleep).
type Summary struct {
	ID          int64  `json:"id"`
	VehicleID   int64  `json:"vehicle_id"`
	VIN         string `json:"vin"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"`
}

// VehicleData is the full payload returned by GET /api/1/vehicles/{id}/vehicle_data.
// All five sub-objects must be non-null or TeslaMate discards the snapshot.
type VehicleData struct {
	ID          int64   `json:"id"`
	VehicleID   int64   `json:"vehicle_id"`
	VIN         string  `json:"vin"`
	DisplayName string  `json:"display_name"`
	State       string  `json:"state"`
	OptionCodes *string `json:"option_codes"`
	APIVersion  *int    `json:"api_version"`
	InService   *bool   `json:"in_service"`

	ChargeState   *ChargeState   `json:"charge_state"`
	ClimateState  *ClimateState  `json:"climate_state"`
	DriveState    *DriveState    `json:"drive_state"`
	VehicleConfig *VehicleConfig `json:"vehicle_config"`
	VehicleState  *VehicleState  `json:"vehicle_state"`
}

// ChargeState mirrors TeslaApi.Vehicle.State.Charge. charge_energy_added and
// ideal_battery_range must be non-null or every charge row is silently dropped.
type ChargeState struct {
	ChargeMilesAddedRated       *float64 `json:"charge_miles_added_rated"`
	ChargeCurrentRequest        *int     `json:"charge_current_request"`
	ChargerPower                *float64 `json:"charger_power"`
	ManagedChargingStartTime    *int64   `json:"managed_charging_start_time"`
	ChargerPhases               *int     `json:"charger_phases"`
	ChargeEnergyAdded           *float64 `json:"charge_energy_added"`
	ChargerVoltage              *int     `json:"charger_voltage"`
	FastChargerType             *string  `json:"fast_charger_type"`
	TimeToFullCharge            *float64 `json:"time_to_full_charge"`
	IdealBatteryRange           *float64 `json:"ideal_battery_range"`
	UsableBatteryLevel          *int     `json:"usable_battery_level"`
	ScheduledChargingPending    *bool    `json:"scheduled_charging_pending"`
	ChargerActualCurrent        *int     `json:"charger_actual_current"`
	EstBatteryRange             *float64 `json:"est_battery_range"`
	ChargeLimitSocMin           *int     `json:"charge_limit_soc_min"`
	ChargePortDoorOpen          *bool    `json:"charge_port_door_open"`
	ManagedChargingActive       *bool    `json:"managed_charging_active"`
	ChargeLimitSocMax           *int     `json:"charge_limit_soc_max"`
	FastChargerPresent          *bool    `json:"fast_charger_present"`
	FastChargerBrand            *string  `json:"fast_charger_brand"`
	ScheduledChargingStartTime  *int64   `json:"scheduled_charging_start_time"`
	ConnChargeCable             *string  `json:"conn_charge_cable"`
	Timestamp                   *int64   `json:"timestamp"`
	UserChargeEnableRequest     *bool    `json:"user_charge_enable_request"`
	ChargePortColdWeatherMode   *bool    `json:"charge_port_cold_weather_mode"`
	ChargeToMaxRange            *bool    `json:"charge_to_max_range"`
	MaxRangeChargeCounter       *int     `json:"max_range_charge_counter"`
	ChargeLimitSocStd           *int     `json:"charge_limit_soc_std"`
	ChargePortLatch             *string  `json:"charge_port_latch"`
	ManagedChargingUserCanceled *bool    `json:"managed_charging_user_canceled"`
	ChargerPilotCurrent         *int     `json:"charger_pilot_current"`
	TripCharging                *bool    `json:"trip_charging"`
	BatteryRange                *float64 `json:"battery_range"`
	ChargingState               *string  `json:"charging_state"`
	ChargeRate                  *float64 `json:"charge_rate"`
	NotEnoughPowerToHeat        *bool    `json:"not_enough_power_to_heat"`
	ChargeLimitSoc              *int     `json:"charge_limit_soc"`
	ChargeEnableRequest         *bool    `json:"charge_enable_request"`
	ChargeCurrentRequestMax     *int     `json:"charge_current_request_max"`
	BatteryLevel                *int     `json:"battery_level"`
	ChargeMilesAddedIdeal       *float64 `json:"charge_miles_added_ideal"`
	BatteryHeaterOn             *bool    `json:"battery_heater_on"`
}

// ClimateState mirrors TeslaApi.Vehicle.State.Climate.
type ClimateState struct {
	BatteryHeater              *bool    `json:"battery_heater"`
	BatteryHeaterNoPower       *bool    `json:"battery_heater_no_power"`
	ClimateKeeperMode          *string  `json:"climate_keeper_mode"`
	DefrostMode                *int     `json:"defrost_mode"`
	DriverTempSetting          *float64 `json:"driver_temp_setting"`
	FanStatus                  *int     `json:"fan_status"`
	InsideTemp                 *float64 `json:"inside_temp"`
	IsAutoConditioningOn       *bool    `json:"is_auto_conditioning_on"`
	IsClimateOn                *bool    `json:"is_climate_on"`
	IsFrontDefrosterOn         *bool    `json:"is_front_defroster_on"`
	IsPreconditioning          *bool    `json:"is_preconditioning"`
	IsRearDefrosterOn          *bool    `json:"is_rear_defroster_on"`
	LeftTempDirection          *int     `json:"left_temp_direction"`
	MaxAvailTemp               *float64 `json:"max_avail_temp"`
	MinAvailTemp               *float64 `json:"min_avail_temp"`
	OutsideTemp                *float64 `json:"outside_temp"`
	PassengerTempSetting       *float64 `json:"passenger_temp_setting"`
	RemoteHeaterControlEnabled *bool    `json:"remote_heater_control_enabled"`
	RightTempDirection         *int     `json:"right_temp_direction"`
	SeatHeaterLeft             *int     `json:"seat_heater_left"`
	SeatHeaterRearCenter       *int     `json:"seat_heater_rear_center"`
	SeatHeaterRearLeft         *int     `json:"seat_heater_rear_left"`
	SeatHeaterRearRight        *int     `json:"seat_heater_rear_right"`
	SeatHeaterRearLeftBack     *int     `json:"seat_heater_rear_left_back"`
	SeatHeaterRearRightBack    *int     `json:"seat_heater_rear_right_back"`
	SeatHeaterRight            *int     `json:"seat_heater_right"`
	SideMirrorHeaters          *bool    `json:"side_mirror_heaters"`
	SteeringWheelHeater        *bool    `json:"steering_wheel_heater"`
	SmartPreconditioning       *bool    `json:"smart_preconditioning"`
	Timestamp                  *int64   `json:"timestamp"`
	WiperBladeHeater           *bool    `json:"wiper_blade_heater"`
}

// DriveState mirrors TeslaApi.Vehicle.State.Drive. timestamp/latitude/longitude
// must be numeric or positions are dropped; shift_state drives the FSM.
type DriveState struct {
	GpsAsOf                        *int64   `json:"gps_as_of"`
	Heading                        *int     `json:"heading"`
	Latitude                       *float64 `json:"latitude"`
	Longitude                      *float64 `json:"longitude"`
	NativeLatitude                 *float64 `json:"native_latitude"`
	NativeLocationSupported        *int     `json:"native_location_supported"`
	NativeLongitude                *float64 `json:"native_longitude"`
	NativeType                     *string  `json:"native_type"`
	Power                          *float64 `json:"power"`
	ShiftState                     *string  `json:"shift_state"`
	Speed                          *float64 `json:"speed"`
	Timestamp                      *int64   `json:"timestamp"`
	ActiveRouteDestination         *string  `json:"active_route_destination"`
	ActiveRouteEnergyAtArrival     *float64 `json:"active_route_energy_at_arrival"`
	ActiveRouteLatitude            *float64 `json:"active_route_latitude"`
	ActiveRouteLongitude           *float64 `json:"active_route_longitude"`
	ActiveRouteMilesToArrival      *float64 `json:"active_route_miles_to_arrival"`
	ActiveRouteMinutesToArrival    *float64 `json:"active_route_minutes_to_arrival"`
	ActiveRouteTrafficMinutesDelay *float64 `json:"active_route_traffic_minutes_delay"`
}

// VehicleConfig mirrors TeslaApi.Vehicle.State.VehicleConfig. It must be a
// non-null object (car_type at minimum) or identify/1 crashes the FSM.
type VehicleConfig struct {
	CanAcceptNavigationRequests *bool   `json:"can_accept_navigation_requests"`
	CanActuateTrunks            *bool   `json:"can_actuate_trunks"`
	CarSpecialType              *string `json:"car_special_type"`
	CarType                     *string `json:"car_type"`
	ChargePortType              *string `json:"charge_port_type"`
	EuVehicle                   *bool   `json:"eu_vehicle"`
	ExteriorColor               *string `json:"exterior_color"`
	HasAirSuspension            *bool   `json:"has_air_suspension"`
	HasLudicrousMode            *bool   `json:"has_ludicrous_mode"`
	KeyVersion                  *int    `json:"key_version"`
	MotorizedChargePort         *bool   `json:"motorized_charge_port"`
	PerfConfig                  *string `json:"perf_config"`
	Plg                         *bool   `json:"plg"`
	RearSeatHeaters             *int    `json:"rear_seat_heaters"`
	RearSeatType                *int    `json:"rear_seat_type"`
	Rhd                         *bool   `json:"rhd"`
	RoofColor                   *string `json:"roof_color"`
	SeatType                    *int    `json:"seat_type"`
	SpoilerType                 *string `json:"spoiler_type"`
	SunRoofInstalled            *int    `json:"sun_roof_installed"`
	ThirdRowSeats               *string `json:"third_row_seats"`
	Timestamp                   *int64  `json:"timestamp"`
	TrimBadging                 *string `json:"trim_badging"`
	UseRangeBadging             *bool   `json:"use_range_badging"`
	WheelType                   *string `json:"wheel_type"`
}

// SoftwareUpdate mirrors TeslaApi.Vehicle.State.VehicleState.SoftwareUpdate.
type SoftwareUpdate struct {
	DownloadPerc        *int    `json:"download_perc"`
	ExpectedDurationSec *int    `json:"expected_duration_sec"`
	InstallPerc         *int    `json:"install_perc"`
	ScheduledTimeMs     *int64  `json:"scheduled_time_ms"`
	Status              *string `json:"status"`
	Version             *string `json:"version"`
}

// VehicleState mirrors TeslaApi.Vehicle.State.VehicleState. odometer must be
// numeric; software_update is always present (status "" when none).
type VehicleState struct {
	APIVersion               *int            `json:"api_version"`
	AutoparkStateV3          *string         `json:"autopark_state_v3"`
	AutoparkStyle            *string         `json:"autopark_style"`
	CalendarSupported        *bool           `json:"calendar_supported"`
	CarVersion               *string         `json:"car_version"`
	CenterDisplayState       *int            `json:"center_display_state"`
	Df                       *int            `json:"df"`
	Dr                       *int            `json:"dr"`
	Ft                       *int            `json:"ft"`
	HomelinkDeviceCount      *int            `json:"homelink_device_count"`
	HomelinkNearby           *bool           `json:"homelink_nearby"`
	IsUserPresent            *bool           `json:"is_user_present"`
	LastAutoparkError        *string         `json:"last_autopark_error"`
	Locked                   *bool           `json:"locked"`
	NotificationsSupported   *bool           `json:"notifications_supported"`
	Odometer                 *float64        `json:"odometer"`
	ParsedCalendarSupported  *bool           `json:"parsed_calendar_supported"`
	Pf                       *int            `json:"pf"`
	Pr                       *int            `json:"pr"`
	RemoteStart              *bool           `json:"remote_start"`
	RemoteStartEnabled       *bool           `json:"remote_start_enabled"`
	RemoteStartSupported     *bool           `json:"remote_start_supported"`
	Rt                       *int            `json:"rt"`
	FdWindow                 *int            `json:"fd_window"`
	FpWindow                 *int            `json:"fp_window"`
	RdWindow                 *int            `json:"rd_window"`
	RpWindow                 *int            `json:"rp_window"`
	SentryMode               *bool           `json:"sentry_mode"`
	SentryModeAvailable      *bool           `json:"sentry_mode_available"`
	SmartSummonAvailable     *bool           `json:"smart_summon_available"`
	SoftwareUpdate           *SoftwareUpdate `json:"software_update"`
	SummonStandbyModeEnabled *bool           `json:"summon_standby_mode_enabled"`
	SunRoofPercentOpen       *int            `json:"sun_roof_percent_open"`
	SunRoofState             *string         `json:"sun_roof_state"`
	Timestamp                *int64          `json:"timestamp"`
	ValetMode                *bool           `json:"valet_mode"`
	ValetPinNeeded           *bool           `json:"valet_pin_needed"`
	VehicleName              *string         `json:"vehicle_name"`
	TpmsPressureFl           *float64        `json:"tpms_pressure_fl"`
	TpmsPressureFr           *float64        `json:"tpms_pressure_fr"`
	TpmsPressureRl           *float64        `json:"tpms_pressure_rl"`
	TpmsPressureRr           *float64        `json:"tpms_pressure_rr"`
	TpmsSoftWarningFl        *bool           `json:"tpms_soft_warning_fl"`
	TpmsSoftWarningFr        *bool           `json:"tpms_soft_warning_fr"`
	TpmsSoftWarningRl        *bool           `json:"tpms_soft_warning_rl"`
	TpmsSoftWarningRr        *bool           `json:"tpms_soft_warning_rr"`
}
