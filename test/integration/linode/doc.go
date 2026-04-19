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
//   - provisioning: 2 VMs × ~5 min  = ~$0.003
//   - security:     2 VMs × ~8 min  = ~$0.005
//   - gateway:      1 VM  × ~10 min = ~$0.003
//   - channels:     1 VM  × ~10 min = ~$0.003
//   - node:         2 VMs × ~15 min = ~$0.008
//   - agents:       1 VM  × ~12 min = ~$0.003
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
//   - TestGatewaySmoke (linode_gateway_test.go): provisioning +
//     security + gateway on one instance. Loopback-only bind (no
//     networking block) — mirror of the Multipass tier's
//     TestGatewaySmoke, but on real cloud infrastructure it also
//     proves `npm install -g openclaw` against the public registry,
//     user-mode systemd + linger survival, and (critically) that the
//     gateway does NOT inadvertently bind 0.0.0.0 on a public-IP VM.
//   - TestChannelsSmoke (linode_channels_test.go): provisioning +
//     security + gateway + channels on one instance with two
//     telegram channels (the second one proves ensure-default-
//     account honors ch.Default rather than falling back to first-
//     seen). Fake bot tokens injected via t.Setenv — the channels
//     phase writes config without hitting the Telegram API, the
//     daemon 401s in the background. SFTP-pulls the remote
//     ~/.openclaw/openclaw.json to assert unredacted token values
//     against the multi-bot schema path. Re-asserts loopback bind
//     after channels apply on a public-IP VM, which is the unique
//     value-add over the Multipass tier.
//   - TestNodeSmoke (linode_node_test.go): provisioning + security +
//     mesh-gateway + mesh-join + gateway + node on two instances
//     (one gateway, one node) running the production shape:
//     headscale mode with sslip public hostnames. The first test in
//     this tier that combines mesh + gateway + node in a single
//     apply — the same pipeline army.yml / david-army.yml drive in
//     production. Asserts the node's openclaw-node.service unit
//     references the gateway's TAILNET IP (recorded by install-
//     tailscale into the plan cache, picked up by NodeTarget.
//     GatewayInternalHost's mesh-IP fallback), NOT the public IP —
//     a regression in either step would show up here directly.
//     Exercises the install-caddy + Let's Encrypt path which the
//     Multipass tier can't. Fixture omits exec_policy to cover the
//     ExecPolicyStep.Applicable=false skip path.
//   - TestAgentsSmoke (linode_agents_test.go): provisioning +
//     security + gateway + channels + agents on one instance with
//     one telegram channel and one agent (tools.elevated + identity
//     + bindings, no tools.exec). Exercises every sub-step of the
//     agents phase: add-agent, ensure-model, configure-workspace
//     (SOUL.md / AGENTS.md / IDENTITY.md with managed-section
//     markers + `agents set-identity`), configure-tools
//     (tools.elevated.enabled + allowFrom), configure-bindings
//     (`agents bind telegram:telegram-main`). Re-asserts loopback
//     bind after agents apply on a public-IP VM — the unique
//     value-add over the Multipass tier is catching any agents-
//     phase regression that inadvertently flips gateway.bind on
//     real cloud infrastructure before it reaches production.
//     Fake bot token injected via t.Setenv; daemon 401s in the
//     background without affecting gateway health.
//   - TestMeshSmoke (linode_mesh_test.go): provisioning + security +
//     mesh-gateway + mesh-join on three instances. Uses the default
//     sslip.io public_hostname strategy — so this test also covers
//     install-caddy (Let's Encrypt ACME) and headscale-over-HTTPS,
//     which the Multipass tier can't exercise (no public IP, no
//     certificate).
//   - TestCronAgentWithNodeExec (linode_cron_test.go): full
//     production-shape cron pipeline on two instances (g6-standard-
//     4 gateway + g6-standard-1 scraper-node). Runs provisioning +
//     security + mesh (Caddy + Let's Encrypt) + gateway + node +
//     agents, then post-apply installs Ollama on the gateway VM,
//     pulls qwen2.5:0.5b, points models.providers.ollama.baseUrl
//     at 127.0.0.1:11434, registers an every-5s cron job, and
//     asserts the scheduler fires at least two runs with status=ok.
//     The cron-triggered isolated agent dispatches exec tool-calls
//     to scraper-node over the tailnet ws — the only test on this
//     tier that proves mesh + gateway + node + agents + scheduler
//     all cooperate under real traffic (not just "config landed").
//     Fixture sets node.exec_policy={security:full, ask:off} to
//     cover the Applicable=true branch TestNodeSmoke deliberately
//     omits. Cost envelope: ~25-40 min ≈ $0.09 per run.
package linode
