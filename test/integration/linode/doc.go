//go:build integration_linode

// Package linode holds integration tests that drive the real `claws
// apply` flow against actual Linode instances. Unlike the Multipass tier
// (../multipass), these tests spend real money — pennies per run, but
// non-zero — and exercise the code paths that only exist when the
// gateway has a real public IP: sslip.io resolution, Caddy + ACME
// (Let's Encrypt), Headscale over HTTPS.
//
// Run with:
//
//	go test -tags=integration_linode ./test/integration/linode/...
//
// Requires a Linode API token. The harness looks for it in this order:
//
//  1. `LINODE_TOKEN` in the process environment.
//  2. A `LINODE_TOKEN=…` entry in `<repo>/manifests/.env` (four levels
//     up from the package directory — matches the default location the
//     CLI reads from).
//
// If neither yields a token, tests skip (not fail) with a clear message
// so developers without Linode credentials can still run adjacent
// packages.
//
// Cost envelope (g6-standard-1, us-east, ~April 2026 pricing):
//
//   - provisioning: 2 VMs × ~5 min = ~$0.003
//   - security:     2 VMs × ~8 min = ~$0.005
//   - mesh:         3 VMs × ~12 min = ~$0.010
//
// Leaked VMs are the real cost risk — every test registers a cleanup
// hook BEFORE calling Execute so a panic or timeout still tears down
// what was created. The harness also runs an end-of-test DeleteInstance
// sweep in the green path so DeleteInstance is exercised too.
//
// Tests in this package:
//
//   - TestProvisioningSmoke (linode_provisioning_test.go): two bare
//     linode machines, only the provisioning phase. Mirror of the
//     Multipass tier's smallest-meaningful test.
//   - TestSecuritySmoke (linode_security_test.go): provisioning +
//     security. Same outside-in SSH assertions as the Multipass tier
//     (agent user exists, security packages installed, UFW active,
//     fail2ban up, unattended-upgrades enabled).
//   - TestMeshSmoke (linode_mesh_test.go): provisioning + security +
//     mesh-gateway + mesh-join on three instances. Uses the default
//     sslip.io public_hostname strategy — so this test also covers
//     install-caddy (Let's Encrypt ACME) and headscale-over-HTTPS,
//     which the Multipass tier can't exercise (no public IP, no
//     certificate).
package linode
