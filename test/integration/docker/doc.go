//go:build integration_docker

// Package docker holds the container-based integration tests. They run
// against oc-test containers built from `test/infra/Dockerfile`, which
// installs stubbed systemctl/headscale/tailscale wrappers
// (`test/infra/stub-*.sh`) so the apply plan can complete end-to-end
// without real init systems.
//
// What this tier is good for: plan graph, SSH pool, cache semantics,
// CLI surface, config mutations, channels, agents, cron — anything
// whose correctness does not depend on a real systemd.
//
// What this tier explicitly cannot catch: activation bugs of the
// issue-04 class (wrong unit enabled) and anything that interacts
// with the user-level systemd bus, linger, or cloud-init ordering.
// Those live in the sibling ../multipass tier.
//
// Run with:
//
//	go test -tags=integration_docker ./test/integration/docker/...
package docker
