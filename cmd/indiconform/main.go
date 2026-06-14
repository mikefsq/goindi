// Command indiconform is the ConformU analogue for INDI: it connects to an INDI
// server and reports whether each device conforms to the protocol and the
// standard property contracts. Exit status is non-zero if any check fails.
//
//	indiconform -addr localhost:7624
//	indiconform -addr localhost:7624 -device "10Micron" -mutate=false
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
	mutate := flag.Bool("mutate", true, "run state-changing checks (connect, guide pulse)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "indiconform:", err)
		os.Exit(2)
	}
	defer c.Close()

	results := conform.Run(c, conform.Options{Device: *device, Mutate: *mutate})

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
