package v1

import (
	"fmt"

	"github.com/google/uuid"
)

// dcCid builds a fresh per-request Dc-Cid value (m-ios-<uuid>). Rivian expects
// a unique device-correlation id on each request.
func dcCid() string {
	return fmt.Sprintf("m-ios-%s", uuid.New().String())
}

// gatewayHeaders are the auth headers for gateway data calls: A-Sess + U-Sess
// (NOT Bearer), and a per-request Dc-Cid. The Apollo client name comes from
// DefaultHeaders.
func gatewayHeaders(a *AuthData) map[string]string {
	return map[string]string{
		"A-Sess": a.AppSessionToken,
		"U-Sess": a.UserSessionToken,
		"Dc-Cid": dcCid(),
	}
}

// chargingHeaders are the auth headers for the charging endpoint: U-Sess only.
func chargingHeaders(a *AuthData) map[string]string {
	return map[string]string{
		"U-Sess": a.UserSessionToken,
		"Dc-Cid": dcCid(),
	}
}
