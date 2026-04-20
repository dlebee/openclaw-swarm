package common

import "fmt"

// OpenclawNodeCompileCacheDir is the path openclaw recommends for Node's V8
// module compile cache. Matches gateway/node systemd drop-ins and openclaw
// doctor guidance (/var/tmp survives reboots better than /tmp).
const OpenclawNodeCompileCacheDir = "/var/tmp/openclaw-compile-cache"

// OpenclawCLIPreamble returns a bash snippet: export NODE_COMPILE_CACHE and
// OPENCLAW_NO_RESPAWN, then mkdir -p on the cache dir. Prefix any remote
// script that invokes the openclaw CLI outside systemd (apply probe, helpers)
// so repeated invocations warm V8 bytecode and skip the respawn fast path.
func OpenclawCLIPreamble() string {
	return fmt.Sprintf(
		"export NODE_COMPILE_CACHE=%q\nexport OPENCLAW_NO_RESPAWN=1\nmkdir -p \"$NODE_COMPILE_CACHE\"\n",
		OpenclawNodeCompileCacheDir,
	)
}
