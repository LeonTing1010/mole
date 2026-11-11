// Package nodeassets holds the server assets shared by CLI and standalone deployments.
package nodeassets

import _ "embed"

// BrutalKeeper is also installed verbatim by cloud-init and Docker.
//
//go:embed brutal-keeper.py
var BrutalKeeper string

// InstallFunctions contains the pinned, bounded engine installers.
//
//go:embed install-functions.sh
var InstallFunctions string
