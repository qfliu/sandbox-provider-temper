package sandbox

import (
	"strings"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
)

// The runtime a request asks for (D65): the default when empty, refused when
// unknown or not installed on this cluster.
func TestRuntimeChoice(t *testing.T) {
	gvisorOnly := &Service{cfg: config.Sandbox{Runtimes: []string{"gvisor"}}}
	both := &Service{cfg: config.Sandbox{Runtimes: []string{"gvisor", "kata"}}}
	unset := &Service{}
	for _, c := range []struct {
		s       *Service
		in      string
		want    string
		wantErr string
	}{
		{gvisorOnly, "", "gvisor", ""},
		{gvisorOnly, "gvisor", "gvisor", ""},
		{gvisorOnly, "kata", "", "isn't available on this cluster (available: gvisor)"},
		{gvisorOnly, "runc", "", "runtime must be one of gvisor, kata"},
		{gvisorOnly, "GVISOR", "", "runtime must be one of"},
		{both, "", "gvisor", ""},
		{both, "kata", "kata", ""},
		{unset, "", "gvisor", ""},
	} {
		got, err := c.s.runtime(c.in)
		if c.wantErr != "" {
			var ve *ValidationError
			if err == nil || !asValidation(err, &ve) || !strings.Contains(ve.Msg, c.wantErr) {
				t.Errorf("runtime(%q) error = %v, want validation error containing %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("runtime(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func asValidation(err error, ve **ValidationError) bool {
	v, ok := err.(*ValidationError)
	*ve = v
	return ok
}
