package httpx_test

import (
	"net/http"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/Toshik1978/firefly-jar/internal/httpx"
)

// ClientSuite covers httpx.NewClient (T019, R8, R12): a bounded per-request timeout and no
// automatic redirect handling, so a bearer token configured for one host is never replayed to
// another after a 3xx.
type ClientSuite struct {
	suite.Suite
}

func (s *ClientSuite) TestNewClientSetsThirtySecondTimeout() {
	client := httpx.NewClient(http.DefaultTransport)

	s.Equal(30*time.Second, client.Timeout)
}

func (s *ClientSuite) TestNewClientNeverFollowsARedirect() {
	client := httpx.NewClient(http.DefaultTransport)
	s.Require().NotNil(client.CheckRedirect, "a nil CheckRedirect would follow redirects automatically")

	err := client.CheckRedirect(&http.Request{}, nil)

	s.Require().ErrorIs(err, http.ErrUseLastResponse)
}
