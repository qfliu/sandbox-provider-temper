package sandbox

import (
	"os"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/qfliu/sandbox-provider-temper/code/internal/config"
)

// Every image users can pick is pre-pulled on every node (D19, D46): the
// Helm chart's sandboxImages values drive both the API's settings and the
// pre-pull DaemonSet, so they must list exactly Images, with the API's
// default repo and tag. Adding an image to Images without adding it to the
// chart fails here.
func TestChartListsEveryImage(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/helm/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		SandboxImages struct {
			Repository string   `json:"repository"`
			Tag        string   `json:"tag"`
			Names      []string `json:"names"`
		} `json:"sandboxImages"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SANDBOX_INTERNAL_KEY_FILE", "unused")
	t.Setenv("DATABASE_URL", "postgres://unused")
	t.Setenv("SANDBOX_IMAGE_REPO", "") // the API's defaults
	t.Setenv("SANDBOX_IMAGE_TAG", "")
	cfg, err := config.LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	got := values.SandboxImages
	if got.Repository != cfg.Sandbox.ImageRepo || got.Tag != cfg.Sandbox.ImageTag {
		t.Errorf("chart sandboxImages %q:%q, API defaults %q:%q", got.Repository, got.Tag, cfg.Sandbox.ImageRepo, cfg.Sandbox.ImageTag)
	}
	want := slices.Sorted(slices.Values(Images))
	if names := slices.Sorted(slices.Values(got.Names)); !slices.Equal(names, want) {
		t.Errorf("chart sandboxImages.names %v, want sandbox.Images %v", got.Names, want)
	}
}
