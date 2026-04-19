//go:build integration_multipass

// Package multipass holds integration tests that drive the real `claws
// apply` flow against local Multipass VMs. Unlike the container tier in
// ../docker, these tests do NOT install any stubs — systemd, cloud-init,
// the multipass CLI itself, and the hosting.Provider plumbing are all
// exercised for real.
//
// See docs/multipass-integration-plan.md for the full design. Run with:
//
//	go test -tags=integration_multipass ./test/integration/multipass/...
//
// Requires the `multipass` CLI on PATH. Tests skip (not fail) in preflight
// with a clear "install Multipass" message when it's missing, so
// developers without the tool can still run adjacent packages.
//
// Tests in this package:
//
//   - TestProvisioningSmoke (multipass_provisioning_test.go): the minimum
//     viable exercise — builds an in-memory manifest with a single
//     `type: multipass` machine, runs ONLY the provisioning phase of the
//     apply plan, and asserts the resulting VM is Running with an IPv4.
//     Tears down via the same provider's DeleteInstance path so the
//     destroy flow is covered too.
//
// Other tests (see the corresponding file for full header docs):
// TestSecuritySmoke, TestMeshSmoke, TestGatewaySmoke, TestChannelsSmoke,
// TestNodeSmoke, TestAgentsSmoke, TestCronAgentWithNodeExec. The last one
// is the fullest-stack exercise in the tier: provisioning + security +
// mesh + gateway + node + agents, then post-apply SFTP-uploads
// test/infra/fake-ollama.py to the gateway VM and starts it under
// systemd --user as the LLM backend, registers an every-5s cron job, and
// asserts at least two scheduler runs fire with status=ok — proving the
// end-to-end cron → isolated agent → ollama-plugin → stub → exec-over-
// tailnet-ws → tool result → final text pipeline.
//
// Why fake-ollama instead of a real ollama install + model pull: the
// scheduler / agent-session / exec-over-node coverage this tier targets
// is independent of LLM weights. The earlier iteration pulled a 1.3 GiB
// linux-arm64 ollama tarball + the qwen2.5:0.5b model weights, which on
// a slow home ISP dominated the test (~60-90 min) for zero additional
// coverage. The docker tier's identical test bakes qwen2.5:0.5b into
// oc-ollama-test so real-ollama coverage lives there; on Multipass the
// ~5 min fake variant is the right tradeoff.
package multipass
