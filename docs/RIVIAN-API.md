# The Rivian API (as redswitchboard uses it)

A reference for the slice of Rivian's undocumented GraphQL API that Layer 1 (`internal/rivian`)
speaks. System design: [ARCHITECTURE.md](../ARCHITECTURE.md). Operations: [FAQ.md](../FAQ.md).

> Undocumented and reverse-engineered from the iOS app and the community Python/Home Assistant
> clients. It can change at any time. The field sets below mirror those clients because that is what
> is known to work over HTTP polling.

Implemented in `internal/protocol/rivian/graphql/poll/v1/{constants,queries,headers,transport,session,parse}.go`.
The Rivian sink (`redswitchboard mock --protocol rivian-graphql-poll-v1`) reproduces these exact shapes, so the real parser round-trips the mock's output.

## Endpoints

The base is configurable (`rivian.base_url`, default `https://rivian.com/api/gql`); per-endpoint URLs
derive from it via `GatewayURL(base)` / `ChargingURL(base)`, so the whole client retargets at a fake
by changing one value.

| Endpoint | Path | Operations |
|---|---|---|
| **gateway** | `/api/gql/gateway/graphql` | `CreateCSRFToken`, `Login`, `LoginWithOTP`, `getUserInfo`, `GetVehicleState` |
| **charging** | `/api/gql/chrg/user/graphql` | `getLiveSessionData` |

Every call is an HTTP `POST` of a JSON body (`{operationName, query, variables}`); only body and
headers vary. A 30s client timeout backstops the per-request context. (Rivian's `orders`/`content`/`t2d`
roots are defined in `constants.go` but unused.)

## Authentication

No bearer tokens; auth is carried in custom session headers. A login mints three:

- **A-Sess** (app session) and **Csrf-Token**, from `createCsrfToken`
- **U-Sess** (user session), from `login` (or `loginWithOTP` for MFA)

Sequence: `CreateCSRFToken` -> `Login(email, password)` (returns the session tokens, or an `otpToken`
for MFA) -> `LoginWithOTP(email, code, otpToken)` if MFA -> `getUserInfo` (the vehicle list: `id`
GUID, `name`, `vin`).

There is **no refresh mutation**: redswitchboard refreshes the short-lived CSRF/app-session tokens at
runtime (once automatically on `UNAUTHENTICATED`), but a fully expired user session can only be replaced
by a fresh login. That login (the full `CreateCSRFToken -> Login -> LoginWithOTP -> getUserInfo`
sequence, writing a base64 JSON creds file at `0600`) is performed by the separate
[`rivian_auth`](https://github.com/bttnns/rivian_auth) tool.

## Headers

`DefaultHeaders` (in `constants.go`) go on every request:

```
User-Agent:                RivianApp/1304 CFNetwork/1404.0.5 Darwin/22.3.0
Accept:                    application/json
Content-Type:              application/json
Accept-Language:           en-US
Accept-Encoding:           gzip, deflate, br
Apollographql-Client-Name: com.rivian.ios.consumer-apollo-ios
```

Each call type adds session headers (`headers.go`); `Dc-Cid` is a fresh per-request id `m-ios-<uuid>`.
There is no `Authorization` header anywhere.

| Call | Added headers |
|---|---|
| gateway data (`GetVehicleState`) | `A-Sess` + `U-Sess` + `Dc-Cid` |
| charging data (`getLiveSessionData`) | `U-Sess` + `Dc-Cid` |
| login (`Login`, `LoginWithOTP`) | `Csrf-Token` + `A-Sess` + `Dc-Cid` |
| `getUserInfo` | `Csrf-Token` + `A-Sess` + `U-Sess` + `Dc-Cid` |

**HTTP 200 with `errors[]`.** Rivian returns GraphQL errors as HTTP 200 with a populated `errors[]`,
not a 4xx. The transport parses `errors[]` first, then the status; the first error's
`extensions.code` is what callers branch on (`IsUnauthenticated`/`IsRateLimited` in `errors.go`):
`UNAUTHENTICATED` (or HTTP 401) -> re-auth; `RATE_LIMIT` -> back off; `BAD_USER_INPUT` -> bad request.

## Gateway `GetVehicleState`

Every scalar is wrapped `{ timeStamp value }` (value typed String/Int/Float/Bool). `gnssLocation` is
`{ latitude longitude timeStamp }`, `cloudConnection` is `{ isOnline lastSync }`.

The query requests only the **poll-safe (sans-TPMS)** field set known to work over polling. Numeric
`tirePressure*`, `chargingTripTarget*`, `activeDriverName`, and other `@subscriptionOnly` fields are
omitted (they error over polling; mirrors HA's `VEHICLE_STATE_SANS_TPMS_API_FIELDS`). Tire **status
strings** (e.g. `OK`) are fetched; numeric pressures are not. Fields fetched (`queries.go`): cloud
connection and GPS; speed/bearing/altitude; mileage, range; battery level/limit/capacity; power,
gear, drive mode; charger state/status, charge port, time-to-charge; cabin climate, seat/steering
heat, preconditioning, defrost; gear-guard; tire-status strings; door/window/closure sensors; `ota*`.

**Sentinels.** A field can report `{fault, signal_not_available, undefined}` (case-insensitive,
mirrors HA's `INVALID_SENSOR_STATES`) to mean "no reading". The parser treats those, and a `null`,
as **absent** and leaves the Go field zero, so the translator holds last-known instead of propagating
junk.

The parser flattens to `rivian.VehicleState` (`types.go`). Units (verified vs the Python/HA clients):

| Domain field | Unit |
|---|---|
| `VehicleMileage` | meters (int) |
| `DistanceToEmpty` | kilometers (int) |
| `GnssSpeed` | meters/second |
| `GnssBearing` | degrees |
| `GnssAltitude` | meters |
| `BatteryLevel`, `BatteryLimit` | percent 0-100 |
| `BatteryCapacity` | kWh gross (dynamic) |
| `CabinInteriorTemp` | degrees Celsius |
| `TimeToEndOfCharge` | minutes |
| `OtaInstallProgress` / `OtaDownloadProgress` | percent 0-100 |

Enum strings stay raw/lowercase: `powerState` {go, ready, sleep, standby, vehicle_reset};
`gearStatus` {drive, neutral, park, reverse}; `chargerState` {charging_active, charging_connecting,
charging_ready}; locks {locked, unlocked}; doors/windows/closures {open, closed}. `LastUpdate` is the
newest timeStamp across the flattened scalars (the snapshot time when `gnssLocation` is absent).

Example (`POST /api/gql/gateway/graphql`, headers `A-Sess`/`U-Sess`/`Dc-Cid` + defaults):

```json
{
  "operationName": "GetVehicleState",
  "query": "query GetVehicleState($vehicleID: String!) { vehicleState(id: $vehicleID) { cloudConnection { isOnline lastSync } gnssLocation { latitude longitude timeStamp } batteryLevel { timeStamp value } powerState { timeStamp value } ... } }",
  "variables": { "vehicleID": "01-REDACTED-GUID" }
}
```

```json
{
  "data": { "vehicleState": {
    "cloudConnection": { "isOnline": true, "lastSync": "2026-06-15T18:04:02.000Z" },
    "gnssLocation": { "latitude": 37.0, "longitude": -122.0, "timeStamp": "2026-06-15T18:04:01.000Z" },
    "batteryLevel": { "timeStamp": "2026-06-15T18:03:50.000Z", "value": 74 },
    "powerState":   { "timeStamp": "2026-06-15T18:03:50.000Z", "value": "ready" },
    "gearStatus":   { "timeStamp": "2026-06-15T18:03:50.000Z", "value": "park" },
    "vehicleMileage": { "timeStamp": "2026-06-15T18:03:50.000Z", "value": 18452000 }
  } }
}
```

## Charging `getLiveSessionData`

Value records are wrapped `{ value updatedAt }` (note: not the gateway's `{ timeStamp value }`); a few
fields (`timeElapsed`, `currentPrice`, `currentCurrency`) are bare scalars. The session is **nullable**:
`null` means no active session (callers fall back to the gateway's `chargerState`/`chargerStatus`). To
avoid wasted calls, redswitchboard only hits this endpoint when the gateway snapshot says plugged in.
`currentPrice`/`timeElapsed` are `Float` in the schema but sometimes arrive as quoted strings, so the
parser uses a `flexFloat` that accepts either.

`rivian.LiveSessionData` (`types.go`):

| Domain field | Unit |
|---|---|
| `Power` | kW |
| `Current` | amps |
| `KilometersChargedPerHr` | km/h |
| `RangeAddedThisSession` | km |
| `Soc` | percent 0-100 |
| `TimeRemaining` | seconds (API sends a String) |
| `TotalChargedEnergy` | kWh, cumulative for the session |
| `CurrentPrice` | money in `CurrentCurrency` |
| `TimeElapsed` | seconds |

Example (`POST /api/gql/chrg/user/graphql`, headers `U-Sess`/`Dc-Cid` + defaults), active session:

```json
{
  "data": { "getLiveSessionData": {
    "__typename": "LiveSessionData",
    "vehicleChargerState": { "value": "charging_active", "updatedAt": "2026-06-15T18:04:00.000Z" },
    "power":              { "value": 48.2, "updatedAt": "2026-06-15T18:04:00.000Z" },
    "totalChargedEnergy": { "value": 12.4, "updatedAt": "2026-06-15T18:04:00.000Z" },
    "timeRemaining":      { "value": "1800", "updatedAt": "2026-06-15T18:04:00.000Z" },
    "soc":                { "value": 74, "updatedAt": "2026-06-15T18:04:00.000Z" },
    "timeElapsed": 900, "currentPrice": "4.21", "currentCurrency": "USD"
  } }
}
```

No active session: `{ "data": { "getLiveSessionData": null } }`.

## Auth operation shapes

- `CreateCSRFToken`: `mutation { createCsrfToken { csrfToken appSessionToken } }`
- `Login`: `mutation Login($email,$password) { login(...) { ... on MobileLoginResponse { accessToken refreshToken userSessionToken } ... on MobileMFALoginResponse { otpToken } } }`
- `LoginWithOTP`: `mutation LoginWithOTP($email,$otpCode,$otpToken) { loginWithOTP(...) { accessToken refreshToken userSessionToken } }`
- `getUserInfo`: `query { currentUser { vehicles { id name vin } } }`

The Rivian sink, fronted by the mock simulator (`redswitchboard mock --protocol rivian-graphql-poll-v1`), serves all
of the above at the same paths (the sink encode is the inverse of `parse.go`); point
`sources.rivian-graphql-poll-v1.base_url` at it for car-free dev.
