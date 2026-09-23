//go:build live

package app_test

import (
	"bytes"
	"os"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/app"
)

// envLiveConfig names the environment variable that points at a real config file. The suite skips
// whenever it is unset, so a normal `go test -tags live` run without live credentials stays a no-op
// rather than failing.
const envLiveConfig = "FIREFLY_JAR_CONFIG"

// LiveSuite runs `check --stdout` against the real services named by a config the caller supplies.
// It is excluded from `task check` by the live build tag and never runs against a made-up config,
// so it never invents a call to a bank or to Firefly III on its own.
type LiveSuite struct {
	suite.Suite
}

// TestCheckStdout exercises the full check pipeline end to end against live services. It only
// asserts the contract every exit code of contracts/cli.md guarantees a check run: 0 (clean) or 1
// (missing transactions found). Any other code, including the parse-error 2, means the run itself
// failed and the captured stderr is surfaced to explain why.
func (s *LiveSuite) TestCheckStdout() {
	configPath := os.Getenv(envLiveConfig)
	if configPath == "" {
		s.T().Skip("set " + envLiveConfig + " to a real config file to run the live smoke test")
	}

	var stdout, stderr bytes.Buffer

	code := app.Run([]string{"check", "--stdout", "--config", configPath}, os.Stdin, &stdout, &stderr)

	s.Containsf([]int{0, 1}, code, "check exited %d, stderr:\n%s", code, stderr.String())
}
