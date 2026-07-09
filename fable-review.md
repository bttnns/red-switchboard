# /code-review xhigh, whole repo at HEAD (e5bebcd) — PAUSED by user

Status: Phase 1 COMPLETE (all 10 finders done, results below). Paused before
Phase 2. Next: dedup candidates (same line/mechanism), spawn 1 verifier agent
per surviving candidate (CONFIRMED/PLAUSIBLE/REFUTED, keep non-REFUTED), then
Phase 3 one sweep finder with verified list, then ReportFindings (<=15, xhigh).
Known cross-angle dups to merge: idmap persist (C7=E4), stale_after ignored
(D3=E5), broadcaster race (D4=E7), replaybuffer under lock (C2=eff1),
triggerNow drop (A1=A7-adjacent=altitude4=C1 related but C1 is the INVERSE
claim: C1 says triggers bypass backoff, A1 says triggers dropped when too
soon; both real, keep separate), merge.go:163 freshness (A3=C4),
config decode swallow (A8=D7), settled-offline poll-only gap (B2=altitude1),
gps_as_of zero time (B5=D5), owner-stream power sign (B1 vs C3: C3 says 0-power
drop kills chargeEnded, B1 says sign inverted entirely; related, keep both).
`go vet` clean, full `go test ./...` passes (via harv).
Excluded from scope: untracked phase1-candidates.md (stale notes).

When resuming: collect remaining finder results (task notifications), then
Phase 2 dedup + 1-vote verify agents (CONFIRMED/PLAUSIBLE/REFUTED), Phase 3
one sweep finder with the verified list, then ReportFindings (≤15, level xhigh).

## Finder status
- DONE conventions (6 candidates)
- DONE reuse (7 candidates)
- DONE efficiency (5 candidates)
- DONE simplification (8 candidates)
- DONE altitude (7 candidates)
- DONE E wrapper correctness (8 candidates)
- DONE C cross-file tracer (8 candidates)
- DONE D Go pitfalls (8 candidates)
- DONE B spec-vs-code (8 candidates)
- DONE A core-hub line scan (8 candidates)
ALL FINDERS DONE.

## Candidates so far

### Conventions
1. go.mod:7 gopkg.in/yaml.v3 explicitly REJECTED in ~/Dev/Docs/go-modules.md (mandates go.yaml.in/yaml/v3); imported by 12+ files incl internal/config/config.go:16
2. go.mod:14 14 of 15 direct deps unrecorded in ~/Dev/Docs/go-modules.md (only spf13/cobra vetted)
3. internal/cache/merge.go:287 7-line comment block vs "one short line max" (repo-wide pattern: 139 blocks of 3+ comment lines)
4. internal/cli/serve.go:228 6-line what-comment block (command gating)
5. internal/cli/serve.go:178 6-line comment block (/metrics wiring)
6. internal/cache/service.go:174 6-line comment block (MergeStreamDetect)

### Reuse (several have correctness drift)
1. internal/protocol/teslafi/csv/v1/sink_mapping.go:89 topState duplicates teslafleet.TopState and DRIFTED: PowerUnknown -> "online" here vs "offline" in fleet/stream sinks
2. internal/protocol/tesla/owner/poll/v1/source.go:154 getWithRefresh verbatim copy of fleet source.go:262 (401-refresh retry wrapper); fleet comment promises shared fix
3. internal/protocol/teslafi/csv/v1/sink_mapping.go:100 shiftState exact dup of teslafleet.ShiftState
4. internal/protocol/teslafi/csv/v1/source_mapping.go:67 gearFromShift dups teslafleet.GearFromTesla, DRIFTED default: "" -> GearPark here vs GearUnknown shared
5. internal/cli/mockstream.go:150 runOwnerStreamServer re-implements mock.StreamServer (conn tracking + broadcast)
6. internal/mock/stream_consumer.go:70 hardcoded 12-column CSV list dup of mock.SubscribeValue (stream_server.go:131)
7. internal/mock/stream_server.go:55 raw gws.NewUpgrader instead of wssutil.NewUpgrader (5s vs 10s handshake timeout fork; also cli/mockstream.go:157)

### Efficiency
1. internal/cache/replaybuffer.go:98 Append marshals whole multi-vehicle ring + sync disk write under global b.mu on every frame/poll; move to background flusher
2. internal/cache/asofstore.go:79 Advance persist() (MarshalIndent+WriteFile+rename) inline under per-vehicle Merger mutex + global store mutex from Merger.commit
3. internal/protocol/tesla/fleet/stream/v1/source_mapping.go:158 payloadOf double proto.Unmarshal per frame; telemetry v0.9.1 Record.GetProtoMessage() exists (comment claiming no getter is stale; verified against pinned lib)
4. internal/protocol/tesla/fleet/stream/v1/source.go:327 KnownIDs() full map copy per frame to test one VIN; capture once per connection or add Known(id) bool
5. internal/protocol/teslafi/csv/v1/source.go:166 Poll re-parses every row Date per poll (~43k time.Parse per poll); parse once at load + binary search

### Simplification
1. internal/cache/trigger.go:43 SessionBoundary/WakeBoundary/sessionBoundaryCrossed/undebounced driveBoundary DEAD (superseded by Merger.MergeStreamDetect; grep + deadcode confirmed); merge.go comments still cite SessionBoundary
2. internal/plugin/source/source.go:120 Register/Open/Names registry boilerplate copy-pasted 5x (source, sink, streamsource, streamsink, commander); generic registry[T] would collapse ~200 to ~40 lines
3. internal/poll/poll.go:156 Poller keeps 8 resolved-tunable fields duplicating p.intervals; p.maxBackoff write-only
4. internal/poll/poll.go:694 deriveState and intervalFor mirrored switches over same classification; derive state once, switch on it
5. internal/protocol/rivian/graphql/poll/v1/types.go:39 5 flattened wire fields decoded but never read (GnssAltitude, DriveMode, GearGuardLocked, KilometersChargedPerHr, RangeAddedThisSession)
6. internal/protocol/rivian/graphql/poll/v1/plugin.go:199 pluggedIn re-implements plugToCanonical vocabulary; line 207 not_connected recheck unreachable
7. internal/plugin/source/source.go:69 IsAccountDisabled + accountDisabled interface + AccountDisabled() methods: zero production callers, hypothetical-future abstraction
8. internal/cli/serve.go:59 serve --debug flag parsed, threaded, discarded at line 77 (_ = debug); dead knob in --help

### Altitude (several carry correctness claims worth verifying)
1. internal/cache/merge.go:207 'settled offline = asleep' rule lives in protocol-agnostic Merger (classifyPollLiveness) + duplicated undebounced in fleet sink adapter; CLAIM: default poll-only config (no stream block) never creates cache.Service so e5bebcd's fix never runs on poll-only deployments (TeslaMate 11h stuck-charging bug persists there)
2. internal/cache/merge.go:158 MergePoll adopts poll State wholesale (SummaryToSnapshot zeroes SOC/range/odo/gear/location); per-field junk-zero rescue guards accumulate per sink; CLAIM: data:update sink pushes blank odo/soc/location frames after asleep summary poll; teslafi/rivian sinks emit battery_level 0 rows; deeper fix: poll presence bits mirroring StreamPresent
3. internal/cache/service.go:212 disconnect-poll gate re-implements stream-freshness with different zero-value semantics than poll.Poller.streamFresh (0 = gate-disabled vs fallback intervals.Driving); CLAIM: poll_overrides stream_fresh_within: 0 disables gate, re-introduces the 1d61648 cost bug while backoff stays on
4. internal/poll/poll.go:427 triggerNow DROPS suppressed trigger while upstream debouncers are edge-triggered and latch; CLAIM: drive-start frame within minInterval of last poll is lost, drive detection delayed up to 5m (or 15m for wake)
5. internal/protocol/tesla/fleet/poll/v1/sink_mapping.go:347 DC/AC threshold configurable in poll layer (dc_threshold_kw) but hardcoded 25.0 in sink encode; operator setting 40 gets split classification
6. internal/protocol/tesla/fleet/poll/v1/adapter.go:218 'moving gear' predicate in 4 copies at 4 altitudes; CLAIM: staleLiveness (wire shift_state, hold-last-known) vs currentState (canonical gear, zeroed by summary poll) already disagree for a stale mid-drive car (asleep vs offline)
7. internal/cache/replaybuffer.go:137 asofStore + replayBuffer duplicate persistence skeleton (95e651c applied same fixes to both); one persisted-JSON store helper

### E wrapper correctness (all correctness bugs)
1. internal/protocol/tesla/fleet/poll/v1/adapter.go:266 translate() reads prov.Latest BEFORE taking e.mu; concurrent vehicle_data requests can commit an OLDER snapshot over a newer one (cached/prev/cachedAsOf regress; served timestamp regression = TeslaMate refetch spin)
2. internal/cache/service.go:149 replay ring stores PRE-commit snapshot; commit() stamps AsOf only on local copy (merge.go:386); replayed frames carry prev frame's AsOf or bypass asofStore floor (timestamp regression on replay)
3. internal/protocol/tesla/owner/stream/v1/source.go:243 owner-stream dialer never calls StreamDisconnectNotifier on connection loss (fleet does at fleet/stream/v1/source.go:287); mid-drive WS drop = no immediate poll, phantom drive up to a DrivingStreaming tick
4. internal/plugin/sink/idmap/idmap.go:102 persist uses direct os.WriteFile (no temp+rename); crash mid-write leaves truncated file, New() rejects it, serve refuses to boot
5. internal/cli/serve.go:144 global cfg.Poll.StaleAfter passed to cache.NewService + teslasink.SetStaleAfter while intervals use cfg.PollFor(source); poll_overrides.<source>.stale_after silently ignored by both staleness gates
6. internal/cache/service.go:181 replay.Append called AFTER releasing Merger lock; two concurrent frames (connection-recycle overlap) can append out of order; replay emits newer-then-older
7. internal/protocol/tesla/stream/v1/sink.go:256 re-subscribe on same WS cancels old broadcaster async + starts new one sharing connState: two broadcasters race on st.lastPush, duplicate data:update frames
8. internal/transport/wssutil/ws.go:77 Sender.Send done-check + enqueue not atomic: message enqueued as writeLoop exits reported sent(true) but never delivered

### C cross-file tracer (all correctness)
1. internal/poll/poll.go:429 triggerNow gates only on time-since-last-SUCCESS; stream-triggered PollNow bypasses error/rate-limit/quota backoff (403 quota block still hammered on every boundary/wake/disconnect trigger)
2. internal/cache/replaybuffer.go:97 Append sync whole-map marshal+WriteFile under GLOBAL mutex on per-frame hot path; violates streamsource.RecordSink.Put documented bound (overlaps efficiency#1)
3. internal/protocol/tesla/owner/stream/v1/source_mapping.go:138 owner-stream decode drops 0 power column (kw > 0 gate clears StreamChargePower presence bit); chargeEnded never fires terminal PollNow on owner-stream path; stale non-zero charge kW pushed
4. internal/cache/merge.go:163 MergePoll freshness guard compares prev.StreamFields vs pollSnap.FetchedAt (receive time); poll content older than FetchedAt so recent stream frames get overwritten; backwards-jumping data:update mid-drive on every drive poll
5. internal/cli/show.go:98 show --server unconditionally broken for Tesla sinks: sink opened over emptyProvider{}, teslaID finds no entry even with --vehicle; always ErrVehicleNotFound
6. internal/protocol/rivian/graphql/poll/v1/errors.go:57 HTTP-level 429 => APIError{HTTPStatus:429, Code:""}; RateLimited() only checks Code=="RATE_LIMIT"; 429 never classified rate-limited; LiveSession 429 swallowed, poll reports success, backoff reset
7. internal/plugin/sink/idmap/idmap.go:102 (dup of E#4) non-atomic persist
8. internal/protocol/tesla/fleet/stream/v1/source.go:198 http.Server.Shutdown never closes hijacked WS conns; ReadLoop goroutines keep merging frames after FlushAsOf/FlushReplay during shutdown; post-flush AsOf advances lost

### D Go pitfalls (all correctness)
1. internal/cache/integrity.go:139 NaN passes gateStream (NaN<0 and NaN>100 both false); NaN poisons snapshot: json.Marshal fails (errors discarded, truncated 200 body, replay ring stops persisting), stream_encode Itoa(int(Round(NaN))) emits -9223372036854775808
2. internal/protocol/tesla/fleet/stream/v1/source.go:178 public mTLS listener http.Server has no ReadHeaderTimeout/IdleTimeout; pre-upgrade slow-header conns hold capped slots forever (32 slots exhaustible)
3. internal/cli/serve.go:144 (dup of E#5) per-source stale_after ignored
4. internal/protocol/tesla/stream/v1/sink.go:256 (dup of E#7) re-subscribe broadcaster race on connState.lastPush
5. internal/protocol/tesla/fleet/poll/v1/sink_mapping.go:217 gps_as_of = Location.TimeStamp.UnixMilli()/1000 without zero-time guard: emits -62135596800 when fix has no timestamp
6. internal/protocol/tesla/stream/v1/sink.go:166 MaxPushHz doc says '0 disables throttling' but applySinkDefaults forces 0 -> 1.0 Hz; only undocumented negative disables
7. internal/config/config.go:214 PollFor swallows poll_overrides decode error (_ = n.Decode(&p)); malformed override block silently ignored/half-applied
8. internal/mock/engine.go:362 charging countdown subtracts int(dt), truncates fractional simulated seconds to 0; TimeRemainingSec frozen at time-scale < 1

### B spec-vs-code (all correctness)
1. internal/protocol/tesla/owner/stream/v1/source_mapping.go:138 owner-stream power SIGN INVERTED vs real Tesla streaming API: positive power (drive discharge) decoded as charging session (phantom charge, charging_dc cadence at >=25kW drive draw); negative power (real charging per docs/teslamate-states power<0) dropped by kw>0 gate
2. internal/cache/merge.go:207 settled-offline->asleep reclassification (e5bebcd) lives only in streaming-mode Merger; default poll-only path (no stream block) never reclassifies; adapter isStale never trips (FetchedAt advances); TeslaMate charge stays open forever (= altitude#1)
3. internal/protocol/tesla/owner/stream/v1/source_mapping.go:143 blank shift_state column treated as 'absent, hold last-known' but real Tesla stream blanks shift_state when PARKED; park never reaches cache, Gear=D held, drive stays open until car sleeps
4. internal/protocol/tesla/stream/v1/stream_encode.go:96 data:update sink emits charge power POSITIVE but TeslaMate stream detection expects NEGATIVE power = charging; TeslaMate never short-circuits to charging fetch
5. internal/protocol/tesla/fleet/poll/v1/source_mapping.go:90 DecodeVehicleData sets Location.TimeStamp = Go zero when gps_as_of nil/0 despite MAPPING.md invariant 'never Go-zero'; sink emits gps_as_of -62135596800 (= D5); Rivian parse.go:233 same hole
6. internal/protocol/tesla/stream/v1/stream_encode.go:68 dataColumnsFor gates every column on != 0: genuine zero (speed 0 at light, heading 0 due north, SOC 0) emitted blank; paired decoder holds stale last-known (65mph while stationary)
7. config/README.md:78 docs promise stale_after degrades to 'offline' but code degrades settled car to 'asleep'; README says stream client_ca '(required)' but code treats optional (identity-only cert check, no chain)
8. internal/protocol/teslafi/csv/v1/sink_mapping.go:20 toRow never populates Row.Power (literal 0 in every exported CSV row); outsideTempOrDefault fabricates 20.0C when ambient unreported; violates MAPPING.md no-fake-0 rule

### A core-hub line scan (all correctness)
1. internal/poll/poll.go:431 triggerNow silently drops boundary/wake PollNow when last success < minInterval; upstream detectors edge-triggered one-shots so trigger lost permanently: awake streaming car served asleep up to 15m; park frame drop holds driving gear up to 2m (= altitude#4)
2. internal/metrics/metrics.go:297 HTTP middleware uses raw (only id-normalized) path as Prometheus label: scanner paths (/wp-login.php, /.env, ...) mint unbounded label series -> OOM
3. internal/cache/merge.go:163 (= C4) MergePoll guard vs FetchedAt: mid-drive poll overwrites streamed loc/odo/SOC with lagged cloud values AND re-anchors gpsAt/odoAt so gateStream falsely rejects next 3-4s of genuine frames (gps_teleport/odometer_regress)
4. internal/cache/trigger.go:136 driveDebouncer.class only updated on stream path; MergePoll never feeds gear transitions: after stream stall + poll-observed park, next stream drive-start matches stale class, boundary missed, no PollNow, drives_opened skewed
5. internal/cache/merge.go:168 MergePoll replaces fresh poll Live wholesale with prev.Live when stream fresher: poll-owned session fields (TimeRemainingSec, FastCharger) from just-billed fetch discarded, stale until next poll wins race
6. internal/cache/trigger.go:105 wakeDebouncer frame run never expires: 3 stray frames spread over hours satisfy >=3-frames-spanning-3s test, spurious wake poll; guard defeated
7. internal/poll/poll.go:417 pollNow accepted while poll in flight consumed immediately after completion: back-to-back source calls ~2s apart violate minInterval floor (extra billed call)
8. internal/config/config.go:214 (= D7) PollFor swallows override decode error
