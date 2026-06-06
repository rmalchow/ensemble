// Command ensemble is a peer-to-peer, sample-synchronized multi-room audio player.
//
// Subcommands:
//
//	ensemble run [flags]   run the node daemon (default --data ./data)
//	ensemble version       print the version
//	ensemble help          print usage
package main

import (
	"fmt"
	"io"
	"os"
)

// version is stamped at build time via -ldflags "-X main.version=..." (doc 11 §2).
var version = "0.0.0-dev"

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stderr))
}

// dispatch is the subcommand router, factored out of main so it is testable
// without os.Exit (doc 01 §5.1 B1, P0.3 §7 T2). It returns the process exit
// code and writes the version / usage / error text to out (main passes
// os.Stderr; the run daemon prints its own banner to stdout). The actual run
// daemon (cmdRun) blocks on signals, so dispatch is only unit-tested over the
// non-run subcommands.
func dispatch(args []string, out io.Writer) int {
	if len(args) < 1 {
		usage(out)
		return 2
	}
	switch args[0] {
	case "run":
		if err := cmdRun(args[1:]); err != nil {
			fmt.Fprintln(out, "ensemble:", err)
			return 1
		}
		return 0
	case "version", "-v", "--version":
		fmt.Fprintln(out, version)
		return 0
	case "help", "-h", "--help":
		usage(out)
		return 0
	default:
		fmt.Fprintf(out, "ensemble: unknown command %q\n\n", args[0])
		usage(out)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ensemble - synchronized multi-room audio player

usage:
  ensemble run [flags]        run the node daemon (default --data ./data)
  ensemble version            print version
  ensemble help               print this help

run flags:
  --data DIR        data directory (config + identity + certs + media)  (default ./data)
  --config PATH     explicit config.yaml (default <data>/config.yaml)
  --name NAME       node friendly name (overrides config / identity)
  --web-port N      control-plane HTTPS base port                       (default 8443)
  --clock-port N    clock-plane UDP port                                (default 9000)
  --audio-port N    audio-plane UDP port                                (default 9100)
  --bind-port N     memberlist gossip port                              (default 7946)
  --join HOST:PORT  explicit gossip seed (repeatable)
  --no-mdns         disable mDNS announce/browse
  --device DEV      audio sink device (e.g. ALSA hw:0)
  -v                verbose cluster/engine logs
`)
}
