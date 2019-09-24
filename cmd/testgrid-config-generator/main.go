package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/GoogleCloudPlatform/testgrid/config"
	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"
)

type options struct {
	releaseConfigDir  string
	testGridConfigDir string
}

func (o *options) Validate() error {
	if o.releaseConfigDir == "" {
		return errors.New("--release-config is required")
	}
	if o.testGridConfigDir == "" {
		return errors.New("--testgrid-config is required")
	}
	return nil
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.releaseConfigDir, "release-config", "", "Path to Release Controller configuration directory.")
	fs.StringVar(&o.testGridConfigDir, "testgrid-config", "", "Path to TestGrid configuration directory.")
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("could not parse input")
	}
	return o
}

// dashboard contains the release/version/type specific data for jobs
type dashboard struct {
	*config.Dashboard
	testGroups []*config.TestGroup
}

func dashboardFor(product, version, role string) dashboard {
	return dashboard{
		Dashboard: &config.Dashboard{
			Name:         fmt.Sprintf("redhat-openshift-%s-release-%s-%s", product, version, role),
			DashboardTab: []*config.DashboardTab{},
		},
		testGroups: []*config.TestGroup{},
	}
}

// dashboardTabFor builds a dashboard tab with default values injected
func dashboardTabFor(name string) *config.DashboardTab {
	return &config.DashboardTab{
		Name:             name,
		TestGroupName:    name,
		BaseOptions:      "width=10",
		OpenTestTemplate: &config.LinkTemplate{Url: "https://prow.svc.ci.openshift.org/view/gcs/<gcs_prefix>/<changelist>"},
		FileBugTemplate: &config.LinkTemplate{
			Url: "https://github.com/openshift/origin/issues/new",
			Options: []*config.LinkOptionsTemplate{
				{Key: "title", Value: "E2E: <test-name>"},
				{Key: "body", Value: "<test-url>"},
			},
		},
		OpenBugTemplate:       &config.LinkTemplate{Url: "https://github.com/openshift/origin/issues/"},
		ResultsUrlTemplate:    &config.LinkTemplate{Url: "https://prow.svc.ci.openshift.org/job-history/<gcs_prefix>"},
		CodeSearchPath:        "https://github.com/openshift/origin/search",
		CodeSearchUrlTemplate: &config.LinkTemplate{Url: "https://github.com/openshift/origin/compare/<start-custom-0>...<end-custom-0>"},
	}
}

func testGroupFor(name string) *config.TestGroup {
	return &config.TestGroup{
		Name:      name,
		GcsPrefix: fmt.Sprintf("origin-ci-test/logs/%s", name),
	}
}

func (d *dashboard) add(name string) {
	d.Dashboard.DashboardTab = append(d.Dashboard.DashboardTab, dashboardTabFor(name))
	d.testGroups = append(d.testGroups, testGroupFor(name))
}

// release is a subset of fields from the release controller's config
type release struct {
	Publish struct {
		Mirror struct {
			ImageStreamRef struct {
				Name string `json:"name"`
			} `json:"'imageStreamRef'"`
		} `json:"mirror-to-origin"`
		Tag struct {
			TagRef struct {
				Name string `json:"name"`
			} `json:"tagRef"`
		} `json:"tag"`
	} `json:"publish"`
	Verify map[string]struct {
		Optional bool `json:"optional"`
		ProwJob  struct {
			Name string `json:"name"`
		} `json:"prowJob"`
	} `json:"verify"`
}

// This tool is intended to make the process of maintaining TestGrid dashboards for
// release-gating and release-informing tests simple.
//
// We read the release controller's configuration for all of the release candidates
// being tested and auto-generate TestGrid configuration for the jobs involved,
// partitioning them by which release they are using (OKD or OCP), which version they
// run for and whether or not they are informing or blocking.
func main() {
	o := gatherOptions()
	if err := o.Validate(); err != nil {
		logrus.Fatalf("Invalid options: %v", err)
	}

	var dashboards []dashboard
	if err := filepath.Walk(o.releaseConfigDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := ioutil.ReadFile(path)
		if err != nil {
			return fmt.Errorf("could not read release controller config at %s: %v", path, err)
		}

		var releaseConfig release
		if err := json.Unmarshal(data, &releaseConfig); err != nil {
			return fmt.Errorf("could not unmarshal release controller config at %s: %v", path, err)
		}

		var product, version string
		if releaseConfig.Publish.Mirror.ImageStreamRef.Name != "" {
			product = "okd"
			version = releaseConfig.Publish.Mirror.ImageStreamRef.Name
		} else if releaseConfig.Publish.Tag.TagRef.Name != "" {
			product = "ocp"
			version = releaseConfig.Publish.Tag.TagRef.Name
		} else {
			logrus.Infof("could not determine publish destination for config at %v", path)
			return nil
		}

		blocking := dashboardFor(product, version, "blocking")
		informing := dashboardFor(product, version, "informing")
		for _, job := range releaseConfig.Verify {
			if job.ProwJob.Name == "release-openshift-origin-installer-e2e-aws-upgrade" {
				// this job is not sharded by version ... why? who knows
				continue
			}
			if job.Optional {
				informing.add(job.ProwJob.Name)
			} else {
				blocking.add(job.ProwJob.Name)
			}
		}
		if len(blocking.testGroups) > 0 {
			dashboards = append(dashboards, blocking)
		}
		if len(informing.testGroups) > 0 {
			dashboards = append(dashboards, informing)
		}
		return nil
	}); err != nil {
		logrus.WithError(err).Fatal("Could not process input configurations.")
	}

	// first, update the overall list of dashboards that exist for the redhat group
	dashboardNames := sets.NewString()
	for _, dash := range dashboards {
		dashboardNames.Insert(dash.Name)
	}

	groupFile := path.Join(o.testGridConfigDir, "groups.yaml")
	data, err := ioutil.ReadFile(groupFile)
	if err != nil {
		logrus.WithError(err).Fatal("Could not read TestGrid group config")
	}

	var groups config.Configuration
	if err := yaml.Unmarshal(data, &groups); err != nil {
		logrus.WithError(err).Fatal("Could not unmarshal TestGrid group config")
	}

	for _, dashGroup := range groups.DashboardGroups {
		if dashGroup.Name == "redhat" {
			dashboardNames.Insert(dashGroup.DashboardNames...)
			dashGroup.DashboardNames = dashboardNames.List() // sorted implicitly
		}
	}

	data, err = yaml.Marshal(&groups)
	if err != nil {
		logrus.WithError(err).Fatal("Could not marshal TestGrid group config")
	}

	if err := ioutil.WriteFile(groupFile, data, 0664); err != nil {
		logrus.WithError(err).Fatal("Could not write TestGrid group config")
	}

	// then, rewrite any dashboard configs we are generating
	for _, dash := range dashboards {
		partialConfig := config.Configuration{
			TestGroups: dash.testGroups,
			Dashboards: []*config.Dashboard{dash.Dashboard},
		}
		sort.Slice(partialConfig.TestGroups, func(i, j int) bool {
			return partialConfig.TestGroups[i].Name < partialConfig.TestGroups[j].Name
		})
		sort.Slice(partialConfig.Dashboards, func(i, j int) bool {
			return partialConfig.Dashboards[i].Name < partialConfig.Dashboards[j].Name
		})
		for k := range partialConfig.Dashboards {
			sort.Slice(partialConfig.Dashboards[k].DashboardTab, func(i, j int) bool {
				return partialConfig.Dashboards[k].DashboardTab[i].Name < partialConfig.Dashboards[k].DashboardTab[j].Name
			})
		}
		data, err = yaml.Marshal(&partialConfig)
		if err != nil {
			logrus.WithError(err).Fatalf("Could not marshal TestGrid config for %s", dash.Name)
		}

		if err := ioutil.WriteFile(path.Join(o.testGridConfigDir, fmt.Sprintf("%s.yaml", dash.Name)), data, 0664); err != nil {
			logrus.WithError(err).Fatalf("Could not write TestGrid config for %s", dash.Name)
		}
	}
	logrus.Info("Finished generating TestGrid dashboards.")
}
