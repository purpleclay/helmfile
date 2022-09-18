package helmfile

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/variantdev/chartify/helmtesting"
	"gopkg.in/yaml.v3"
)

type ociChart struct {
	name    string
	version string
	digest  string
}

func TestHelmfileTemplateWithBuildCommand(t *testing.T) {
	type Config struct {
		LocalDockerRegistry struct {
			Enabled bool `yaml:"enabled"`
			Port    int  `yaml:"port"`
		} `yaml:"localDockerRegistry"`
		LocalChartRepoServer struct {
			Enabled bool `yaml:"enabled"`
			Port    int  `yaml:"port"`
		} `yaml:"localChartRepoServer"`
		ChartifyTempDir string   `yaml:"chartifyTempDir"`
		HelmfileArgs    []string `yaml:"helmfileArgs"`
	}

	_, filename, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(filename), "..", "..", "..", "..")
	helmfileBin := filepath.Join(projectRoot, "helmfile")
	testdataDir := "testdata/snapshot"
	chartsDir := "testdata/charts"

	entries, err := os.ReadDir(testdataDir)
	require.NoError(t, err)

	for _, e := range entries {
		if !e.IsDir() {
			t.Fatalf("Unexpected type of entry at %s", e.Name())
		}

		name := e.Name()

		wd, err := os.Getwd()
		require.NoError(t, err)

		// We read the config from `testdata/snapshot/$CASE_NAME/config.yaml`.
		// It's optional so the test won't fail even if the config file does not exist.

		var config Config

		configFile := filepath.Join(testdataDir, name, "config.yaml")
		if configData, err := os.ReadFile(configFile); err == nil {
			if err := yaml.Unmarshal(configData, &config); err != nil {
				t.Fatalf("Unable to load %s: %v", configFile, err)
			}
		}

		// We run `helmfile build` by default.
		// If you want to test `helmfile template`, set the following in the config.yaml:
		//
		// helmfileArgs:
		// - template
		helmfileArgs := config.HelmfileArgs
		if len(helmfileArgs) == 0 {
			helmfileArgs = append(helmfileArgs, "build")
		}

		t.Run(name, func(t *testing.T) {
			// Use the specific chartify tempdir for easy debugging and the test reproducibility.
			// We do snapshot testing in this test. The default chartify tempdir is a random directory created within the os temp dir.
			// Without making it a static path, it's unnecessarily hard to snapshot test it, as the dir path embedded in the output changes
			// on each test run.
			chartifyTempDir := config.ChartifyTempDir
			if chartifyTempDir == "" {
				chartifyTempDir = "chartify_temp"
			}

			// We set the envvar provided by chartify, CHARTIFY_TEMPDIR, to make the tempdir static.
			chartifyTempDir = filepath.Join(wd, chartifyTempDir)
			t.Setenv("CHARTIFY_TEMPDIR", chartifyTempDir)
			// Ensure there's no dangling and remaining tempdir from the previous run
			if err := os.RemoveAll(chartifyTempDir); err != nil {
				t.Fatalf("unable to remove chartify temp dir %q: %v", chartifyTempDir, err)
			}
			// Ensure it's removed on test completion
			t.Cleanup(func() {
				if err := os.RemoveAll(chartifyTempDir); err != nil {
					t.Fatalf("unable to remove chartify temp dir %q: %v", chartifyTempDir, err)
				}
			})

			// ociCharts holds a list of chart name, version and digest distributed by local oci registry.
			ociCharts := []ociChart{}
			// If localDockerRegistry.enabled is set to `true`,
			// run the docker registry v2 and push the test charts to the registry
			// so that it can be accessed by helm and helmfile as a oci registry based chart repository.
			if config.LocalDockerRegistry.Enabled {
				containerName := "helmfile_docker_registry"

				hostPort := config.LocalDockerRegistry.Port
				if hostPort <= 0 {
					hostPort = 5000
				}

				execDocker(t, "run", "--rm", "-d", "-p", fmt.Sprintf("%d:5000", hostPort), "--name", containerName, "registry:2")
				t.Cleanup(func() {
					execDocker(t, "stop", containerName)
				})

				// FIXME: this is a hack to wait for registry to be up and running
				// please replace with proper wait for registry
				time.Sleep(5 * time.Second)

				// We helm-package and helm-push every test chart saved in the ./testdata/charts directory
				// to the local registry, so that they can be accessed by helmfile and helm invoked while testing.
				charts, err := os.ReadDir(chartsDir)
				require.NoError(t, err)

				for _, c := range charts {
					chartPath := filepath.Join(chartsDir, c.Name())
					if !c.IsDir() {
						t.Fatalf("%s is not a directory", c)
					}
					tgzFile := execHelmPackage(t, chartPath)

					// Extract chart version from the name of chart package archival
					chartName := c.Name()
					chartNameWithVersion := strings.TrimSuffix(filepath.Base(tgzFile), filepath.Ext(tgzFile))
					chartVersion := strings.TrimPrefix(chartNameWithVersion, fmt.Sprintf("%s-", chartName))

					chartDigest, err := execHelmPush(t, tgzFile, fmt.Sprintf("oci://localhost:%d/myrepo", hostPort))
					require.NoError(t, err, "Unable to run helm push to local registry: %v", err)

					ociCharts = append(ociCharts, ociChart{
						name:    chartName,
						version: chartVersion,
						digest:  chartDigest,
					})
				}
			}

			if config.LocalChartRepoServer.Enabled {
				helmtesting.StartChartRepoServer(t, helmtesting.ChartRepoServerConfig{
					Port:      config.LocalChartRepoServer.Port,
					ChartsDir: chartsDir,
				})
			}

			inputFile := filepath.Join(testdataDir, name, "input.yaml")
			outputFile := filepath.Join(testdataDir, name, "output.yaml")

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			args := []string{"-f", inputFile}
			args = append(args, helmfileArgs...)
			cmd := exec.CommandContext(ctx, helmfileBin, args...)
			got, err := cmd.CombinedOutput()
			if err != nil {
				t.Logf("Output from %v: %s", args, string(got))
			}

			require.NoError(t, err, "Unable to run helmfile with args %v", args)

			gotStr := string(got)
			gotStr = strings.ReplaceAll(gotStr, fmt.Sprintf("chart=%s", wd), "chart=$WD")
			// OCI based helm charts are pulled and exported under temporary directory.
			// We are not sure the exact name of the temporary directory generated by helmfile,
			// so redact its base directory name with $TMP.
			if config.LocalDockerRegistry.Enabled {
				var releaseName, chartPath string
				sc := bufio.NewScanner(strings.NewReader(gotStr))
				for sc.Scan() {
					if !strings.HasPrefix(sc.Text(), "Templating ") {
						continue
					}
					releaseChartStr := strings.TrimPrefix(sc.Text(), "Templating ")
					releaseChartParts := strings.Split(releaseChartStr, ", ")
					if len(releaseChartParts) != 2 {
						t.Fatal("Found unexpected log output of templating oci based helm chart, want=\"Templating release=<release_name>, chart=<chart_name>\"")
					}
					releaseNamePart, chartPathPart := releaseChartParts[0], releaseChartParts[1]
					releaseName = strings.TrimPrefix(releaseNamePart, "release=")
					chartPath = chartPathPart
				}
				for _, ociChart := range ociCharts {
					chartPathWithoutTempDirBase := fmt.Sprintf("/%s/%s/%s/%s", releaseName, ociChart.name, ociChart.version, ociChart.name)
					var chartPathBase string
					if strings.HasSuffix(chartPath, chartPathWithoutTempDirBase) {
						chartPathBase = strings.TrimSuffix(chartPath, chartPathWithoutTempDirBase)
					}
					if len(chartPathBase) != 0 {
						gotStr = strings.ReplaceAll(gotStr, chartPathBase, "chart=$TMP")
					}
					gotStr = strings.ReplaceAll(gotStr, fmt.Sprintf("Digest: %s", ociChart.digest), "Digest: $DIGEST")
				}
			}

			if stat, _ := os.Stat(outputFile); stat != nil {
				want, err := os.ReadFile(outputFile)
				require.NoError(t, err)
				require.Equal(t, string(want), gotStr)
			} else {
				// To update the test golden image(output.yaml), just remove it and rerun this test.
				// We automatically capture the output to `output.yaml` in the test case directory
				// when the output.yaml doesn't exist.
				t.Log("generate output.yaml file and write captured output to it")
				require.NoError(t, os.WriteFile(outputFile, []byte(gotStr), 0664))
			}
		})
	}
}

func execDocker(t *testing.T, args ...string) {
	t.Helper()

	docker := exec.Command("docker", args...)
	out, err := docker.CombinedOutput()
	if err != nil {
		t.Logf("Docker output: %s", string(out))
		t.Fatalf("Unable to run docker: %v", err)
	}
}

func execHelmPackage(t *testing.T, localChart string) string {
	t.Helper()

	out := execHelm(t, "package", localChart)
	msg := strings.Split(out, " ")
	tgzAbsPath := msg[len(msg)-1]
	return strings.TrimSpace(tgzAbsPath)
}

// execHelmPush pushes helm package to oci based helm repository,
// then returns its digest.
func execHelmPush(t *testing.T, tgzPath, remoteUrl string) (string, error) {
	t.Helper()

	out := execHelm(t, "push", tgzPath, remoteUrl)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "Digest:") {
			return strings.TrimPrefix(sc.Text(), "Digest: "), nil
		}
	}
	return "", fmt.Errorf("Unable to find chart digest from output string of helm push")
}

func execHelm(t *testing.T, args ...string) string {
	t.Helper()

	cmd := []string{"helm"}
	cmd = append(cmd, args...)
	c := strings.Join(cmd, " ")
	helm := exec.Command("helm", args...)
	out, err := helm.CombinedOutput()
	if err != nil {
		t.Logf("%s: %s", c, string(out))
		t.Fatalf("Unable to run %s: %v", c, err)
	}

	return string(out)
}
