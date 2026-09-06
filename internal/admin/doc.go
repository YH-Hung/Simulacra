// Package admin implements the Simulacra control plane: ConnectRPC handlers for
// the simulacra.admin.v1 API, served over h2c alongside a /healthz endpoint.
//
// The package owns what is served — routes, health endpoint, and protocol
// configuration. When it runs belongs to the server package, which supplies
// Deps and drives the lifecycle.
package admin
