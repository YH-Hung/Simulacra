// Package admin implements the Simulacra control plane: ConnectRPC handlers for
// the simulacra.admin.v1 API, served over h2c alongside a /healthz endpoint.
//
// This phase defines only the generated contract the package will serve; the
// handlers arrive with the admin server itself.
package admin
