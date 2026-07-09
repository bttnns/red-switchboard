package v1

import "strings"

const (
	RivianBasePath         = "https://rivian.com/api/gql"
	RivianGatewayPath      = RivianBasePath + "/gateway/graphql"
	RivianChargingPath     = RivianBasePath + "/chrg/user/graphql"
	RivianOrdersPath       = RivianBasePath + "/orders/graphql"
	RivianContentPath      = RivianBasePath + "/content/graphql"
	RivianTransactionsPath = RivianBasePath + "/t2d/graphql"
	RivianAPIPath          = "https://api.rivian.com"
)

// apiBase normalizes a configured base API root (e.g. https://rivian.com/api/gql
// in production, or http://mock:5050/api/gql in local dev). An empty value
// falls back to the production root.
func apiBase(base string) string {
	if base == "" {
		return RivianBasePath
	}
	return strings.TrimRight(base, "/")
}

// GatewayURL and ChargingURL derive the per-endpoint GraphQL URLs from a base API
// root, so the whole client (data + auth) can be retargeted at a fake Rivian for
// local development by changing one config value (rivian.base_url).
func GatewayURL(base string) string  { return apiBase(base) + "/gateway/graphql" }
func ChargingURL(base string) string { return apiBase(base) + "/chrg/user/graphql" }

var DefaultHeaders = map[string]string{
	"User-Agent":                "RivianApp/1304 CFNetwork/1404.0.5 Darwin/22.3.0",
	"Accept":                    "application/json",
	"Content-Type":              "application/json",
	"Accept-Language":           "en-US",
	"Accept-Encoding":           "gzip, deflate, br",
	"Apollographql-Client-Name": "com.rivian.ios.consumer-apollo-ios",
}
