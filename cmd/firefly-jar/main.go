// Command firefly-jar is a one-shot CLI that reminds its owner about bank transactions missing from
// Firefly III. Cron runs it on a schedule; the command itself never schedules or daemonizes.
package main

import (
	"os"

	"github.com/Toshik1978/firefly-jar/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
