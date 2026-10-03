package sandbox

import (
	"os"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
)

// Every image users can pick is pre-pulled on every node (D19, D46): the
// DaemonSet's init containers use each one with the default tag, so adding
// an image to Images without adding it there fails here. The manifest is
// parsed, so an image mentioned only in a comment doesn't count.
func TestPrepullListsEveryImage(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/dev/prepull.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	if err := yaml.UnmarshalStrict(raw, &ds); err != nil {
		t.Fatal(err)
	}
	var pulled []string
	for _, c := range ds.Spec.Template.Spec.InitContainers {
		pulled = append(pulled, c.Image)
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
		if !slices.Contains(pulled, ref) {
			t.Errorf("prepull.yaml's init containers don't pull %s (they pull %v)", ref, pulled)
		}
	}
}
