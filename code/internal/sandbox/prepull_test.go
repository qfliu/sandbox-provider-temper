package sandbox

import (
	"os"
	"strings"
	"testing"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
)

// Every image users can pick is pre-pulled on every node (D19, step 5.3):
// the DaemonSet manifest lists each one with the default tag, so adding an
// image to Images without adding it there fails here.
func TestPrepullListsEveryImage(t *testing.T) {
	manifest, err := os.ReadFile("../../deploy/dev/prepull.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SANDBOX_INTERNAL_KEY_FILE", "unused")
	t.Setenv("DATABASE_URL", "postgres://unused")
	t.Setenv("SANDBOX_IMAGE_REPO", "") // the defaults the manifest uses
	t.Setenv("SANDBOX_IMAGE_TAG", "")
	cfg, err := config.LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range Images {
		ref := cfg.Sandbox.ImageRepo + img + ":" + cfg.Sandbox.ImageTag
		if !strings.Contains(string(manifest), "image: "+ref+"\n") {
			t.Errorf("prepull.yaml doesn't pre-pull %s", ref)
		}
	}
}
