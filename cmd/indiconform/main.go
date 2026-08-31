// Command indiconform connects to an INDI server and reports whether each device
// conforms to the protocol and the standard property contracts, exiting non-zero if
// any check fails.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mikefsq/goindi/client"
	"github.com/mikefsq/goindi/conform"
)

func main() {
	addr := flag.String("addr", "localhost:7624", "INDI server host:port")
	device := flag.String("device", "", "limit to one device (default: all discovered)")
	mutate := flag.Bool("mutate", false, "run state-changing checks (connect, guide pulse, test exposure); devices conform connects are disconnected afterwards")
	timeout := flag.Duration("timeout", 30*time.Second, "overall run timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	c, err := client.Dial(ctx, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "indiconform:", err)
		os.Exit(2)
	}
	defer c.Close()

	results := conform.Run(ctx, c, conform.Options{Device: *device, Mutate: *mutate})

	for _, r := range results {
		line := fmt.Sprintf("[%-4s] %s", r.Status, r.Check)
		if r.Device != "" {
			line = fmt.Sprintf("[%-4s] %s: %s", r.Status, r.Device, r.Check)
		}
		if r.Detail != "" {
			line += " — " + r.Detail
		}
		fmt.Println(line)
	}

	pass, fail, warn, info := conform.Summarize(results)
	fmt.Printf("\n%d checks: %d pass, %d fail, %d warn, %d info\n", len(results), pass, fail, warn, info)
	if fail > 0 {
		os.Exit(1)
	}
}
