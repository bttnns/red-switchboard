// Command redswitchboard impersonates an external vehicle API so ecosystem apps
// (TeslaMate and any Tesla API consumer today) can run against a different make's
// car. It is a hub: an input SOURCE plugin reads a vendor API and maps it to one
// neutral model; an output SINK plugin serves that model in some external API's
// shape. Each protocol package (rivian-graphql-poll-v1, tesla-fleet-poll-v1, tesla-owner-poll-v1)
// registers both a source and a sink, so any source can feed any sink.
//
// This binary blank-imports the protocol plugins it ships so they self-register,
// then hands off to the command tree in internal/cli (serve / mock / status /
// stats / cache / sources / sinks / config / version).
package main

import (
	"github.com/bttnns/red-switchboard/internal/cli"

	// Blank-import the plugins this binary ships so their init() registers them.
	// Each protocol package registers both a source and a sink.
	_ "github.com/bttnns/red-switchboard/internal/protocol/rivian/graphql/poll/v1"
	_ "github.com/bttnns/red-switchboard/internal/protocol/tesla/command/v1"
	_ "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/poll/v1"
	_ "github.com/bttnns/red-switchboard/internal/protocol/tesla/fleet/stream/v1"
	_ "github.com/bttnns/red-switchboard/internal/protocol/tesla/owner/poll/v1"
	_ "github.com/bttnns/red-switchboard/internal/protocol/tesla/owner/stream/v1"
	_ "github.com/bttnns/red-switchboard/internal/protocol/tesla/stream/v1"
	_ "github.com/bttnns/red-switchboard/internal/protocol/teslafi/csv/v1"
)

func main() {
	cli.Execute()
}
