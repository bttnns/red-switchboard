// Package units holds the pure unit conversions shared by protocol mappers.
// The canonical model (internal/vehicle) is SI/metric throughout; a vendor whose
// API is imperial (Tesla reports miles, mph) converts on the way in (its source
// decode: imperial -> canonical) and on the way out (its sink encode: canonical
// -> imperial). Keeping both directions here means a make's source and sink, and
// two protocols of the same make, never reimplement (or disagree on) a factor.
//
// Temperatures are intentionally absent: both the canonical model and the Tesla
// API use Celsius, so no temperature conversion is needed today.
package units

// Conversion factors (exact).
const (
	metersPerMile = 1609.344
	kmPerMile     = 1.609344
	mpsPerMph     = 0.44704
)

// MetersToMiles converts a meters reading (e.g. an odometer) to miles.
func MetersToMiles(m float64) float64 { return m / metersPerMile }

// MilesToMeters converts a miles reading to meters.
func MilesToMeters(mi float64) float64 { return mi * metersPerMile }

// KmToMiles converts kilometers to miles.
func KmToMiles(km float64) float64 { return km / kmPerMile }

// MilesToKm converts miles to kilometers.
func MilesToKm(mi float64) float64 { return mi * kmPerMile }

// MpsToMph converts meters/second to miles/hour.
func MpsToMph(mps float64) float64 { return mps / mpsPerMph }

// MphToMps converts miles/hour to meters/second.
func MphToMps(mph float64) float64 { return mph * mpsPerMph }
