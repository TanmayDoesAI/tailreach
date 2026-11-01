package scripts

import _ "embed"

// SigningIdentity is signing-identity.sh, which `tailreach setup` runs.
//
//go:embed signing-identity.sh
var SigningIdentity string
