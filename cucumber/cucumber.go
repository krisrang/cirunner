package cucumber

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/krisrang/cirunner/Godeps/_workspace/src/github.com/cucumber/gherkin-go"
)

type FeatureFile struct {
	Feature *gherkin.Feature
	Path    string
	Weight  int
}

type ByWeight []FeatureFile

func (a ByWeight) Len() int           { return len(a) }
func (a ByWeight) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByWeight) Less(i, j int) bool { return a[i].Weight < a[j].Weight }

type Tags struct {
	SelectTags []string
	RejectTags []string
	SlowTags   []string
}

func (t *Tags) Include(feature *gherkin.Feature) bool {
	if len(t.RejectTags) > 0 {
		for _, tag := range feature.Tags {
			if contains(t.RejectTags, tag.Name) {
				return false
			}
		}
	}

	if len(t.SelectTags) > 0 {
		found := false

		for _, tag := range feature.Tags {
			if contains(t.SelectTags, tag.Name) {
				found = true
			}
		}

		return found
	}

	return true
}

func Select(tags Tags) ([]FeatureFile, error) {
	features := make([]FeatureFile, 0)
	files := make([]string, 0)

	visit := func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if strings.HasSuffix(path, ".feature") {
			files = append(files, path)
		}

		return nil
	}

	err := filepath.Walk("features", visit)
	if err != nil {
		return nil, err
	}

	for _, f := range files {
		parsedFeature, err := ParseFeature(f)
		if err != nil {
			return nil, err
		}

		if tags.Include(parsedFeature) {
			weight := 0

			for _, d := range parsedFeature.ScenarioDefinitions {
				if scenario, ok := d.(*gherkin.Scenario); ok {
					weight += len(scenario.Steps)
				}

				if outline, ok := d.(*gherkin.ScenarioOutline); ok {
					examples := 0

					for _, e := range outline.Examples {
						examples += len(e.TableBody)
					}

					weight += len(outline.Steps) * examples
				}
			}

			for _, t := range parsedFeature.Tags {
				if contains(tags.SlowTags, t.Name) {
					weight *= 2
				}
			}

			features = append(features, FeatureFile{
				Feature: parsedFeature,
				Path:    f,
				Weight:  weight,
			})
		}
	}

	return features, nil
}

func ParseTags(tags []string, slow []string) Tags {
	parsedTags := Tags{
		SelectTags: make([]string, 0),
		RejectTags: make([]string, 0),
		SlowTags:   slow,
	}

	for _, t := range tags {
		if strings.HasPrefix(t, "~") {
			parsedTags.RejectTags = append(parsedTags.RejectTags, t[1:])
		} else {
			parsedTags.SelectTags = append(parsedTags.SelectTags, t)
		}
	}

	return parsedTags
}

func ParseFeature(path string) (*gherkin.Feature, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	feature, err := gherkin.ParseFeature(file)
	if err != nil {
		return nil, err
	}

	return feature, err
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}
