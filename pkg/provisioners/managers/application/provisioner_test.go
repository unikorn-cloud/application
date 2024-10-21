/*
Copyright 2024 the Unikorn Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package application_test

import (
	"context"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	unikornv1 "github.com/unikorn-cloud/application/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/application/pkg/provisioners/managers/application"
	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, unikornv1.AddToScheme(s))
	require.NoError(t, unikornv1core.AddToScheme(s))

	return s
}

const (
	namespace = "chicken"
)

// getSemver provides a terse semver creation function.
func getSemver(t *testing.T, version string) *unikornv1core.SemanticVersion {
	t.Helper()

	v, err := semver.NewVersion(version)
	if err != nil {
		t.Fatal(err)
	}

	return &unikornv1core.SemanticVersion{
		Version: *v,
	}
}

// getConstraints provides a terse constraints creation function.
func getConstraints(t *testing.T, constraints string) *unikornv1core.SemanticVersionConstraints {
	t.Helper()

	c, err := semver.NewConstraint(constraints)
	if err != nil {
		t.Fatal(err)
	}

	return &unikornv1core.SemanticVersionConstraints{
		Constraints: *c,
	}
}

// applicationBuilder provides a builder pattern for flexible application creation.
type applicationBuilder struct {
	// currVersion is a pointer to the current version string.
	currVersion string
	// versions is an ordered array of versions.
	// NOTE: no error detection of version resuse is performed.
	versions []*unikornv1core.SemanticVersion
	// dependencies defines a per-version list of package dependencies.
	dependencies map[string][]unikornv1core.HelmApplicationDependency
	// recommendations defines a per-version list of package recommendations.
	recommendations map[string][]unikornv1core.HelmApplicationRecommendation
}

// newApplicationBuilder createa a new application builder.
func newApplicationBuilder() *applicationBuilder {
	return &applicationBuilder{
		dependencies:    map[string][]unikornv1core.HelmApplicationDependency{},
		recommendations: map[string][]unikornv1core.HelmApplicationRecommendation{},
	}
}

// withVersion creates a new version and updates the version pointer.
func (b *applicationBuilder) withVersion(version *unikornv1core.SemanticVersion) *applicationBuilder {
	b.currVersion = version.Original()
	b.versions = append(b.versions, version)

	return b
}

// withDependency adds a dependency to the current version.
func (b *applicationBuilder) withDependency(id string, constraints *unikornv1core.SemanticVersionConstraints) *applicationBuilder {
	b.dependencies[b.currVersion] = append(b.dependencies[b.currVersion], unikornv1core.HelmApplicationDependency{
		Name:        id,
		Constraints: constraints,
	})

	return b
}

// withRecommendation adds a reccomendation to the current version.
//
//nolint:unused
func (b *applicationBuilder) withRecommendation(id string) *applicationBuilder {
	b.recommendations[b.currVersion] = append(b.recommendations[b.currVersion], unikornv1core.HelmApplicationRecommendation{
		Name: id,
	})

	return b
}

// get builds and returns the application resource.
func (b *applicationBuilder) get() *unikornv1core.HelmApplication {
	app := &unikornv1core.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      uuid.New().String(),
		},
		Spec: unikornv1core.HelmApplicationSpec{
			Versions: make([]unikornv1core.HelmApplicationVersion, len(b.versions)),
		},
	}

	for i, version := range b.versions {
		v := unikornv1core.HelmApplicationVersion{
			Version: *version,
		}

		if t, ok := b.dependencies[version.Original()]; ok {
			v.Dependencies = t
		}

		if t, ok := b.recommendations[version.Original()]; ok {
			v.Recommends = t
		}

		app.Spec.Versions[i] = v
	}

	return app
}

// applicationSetBuilder provides a builder pattern for application sets.
type applicationSetBuilder struct {
	// applications are an ordered list of applications to install.
	applications []unikornv1.ApplicationSpec
}

// newApplicationSet creates a new application set.
func newApplicationSet() *applicationSetBuilder {
	return &applicationSetBuilder{}
}

// withApplication adds a new application to the application set.
func (b *applicationSetBuilder) withApplication(id string, version *unikornv1core.SemanticVersion) *applicationSetBuilder {
	spec := unikornv1.ApplicationSpec{
		Application: corev1.TypedObjectReference{
			Kind: unikornv1core.HelmApplicationKind,
			Name: id,
		},
	}

	if version != nil {
		spec.Version = version
	}

	b.applications = append(b.applications, spec)

	return b
}

// get returns the application set resource.
func (b *applicationSetBuilder) get() *unikornv1.ApplicationSet {
	return &unikornv1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: uuid.New().String(),
		},
		Spec: unikornv1.ApplicationSetSpec{
			Applications: b.applications,
		},
	}
}

// expectedAppVersion is a simple tuple of app ID and version.
type expectedAppVersion struct {
	id      string
	version string
}

// expectedGraph provides validation of a application solution.
type expectedGraph struct {
	apps []expectedAppVersion
}

// newExpectedGraph creates a new solution validator.
func newExpectedGraph() *expectedGraph {
	return &expectedGraph{}
}

// withAppVersion adds an ordered application version.
func (e *expectedGraph) withAppVersion(id, version string) *expectedGraph {
	e.apps = append(e.apps, expectedAppVersion{
		id:      id,
		version: version,
	})

	return e
}

// check iterates over the graph, checking applications are in the expected
// order and have the correct version.
// TODO: The iteration is guaranteed to be a BFS, where as the expectation
// is an ordered list.  These don't align and will cause problems if the
// algorithms change.
func (e *expectedGraph) check(t *testing.T, graph *application.Graph) {
	t.Helper()

	i := 0

	for a, v := range graph.All() {
		// TODO: overflow detection.
		expected := e.apps[i]

		require.Equal(t, expected.id, a.Name)
		require.Equal(t, expected.version, v.Version.Original())

		i++
	}
}

// TestProvisionSingle tests a single app is solved.
func TestProvisionSingle(t *testing.T) {
	t.Parallel()

	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).get()
	applicationset := newApplicationSet().withApplication(app.Name, getSemver(t, "1.0.0")).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app).Build()

	graph, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)

	newExpectedGraph().withAppVersion(app.Name, "1.0.0").check(t, graph)
}

// TestProvisionSingleMostRecent tests a single app is solved with the most recent version
// when it isn't specified.
func TestProvisionSingleMostRecent(t *testing.T) {
	t.Parallel()

	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withVersion(getSemver(t, "2.0.0")).get()
	applicationset := newApplicationSet().withApplication(app.Name, nil).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app).Build()

	graph, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)

	newExpectedGraph().withAppVersion(app.Name, "2.0.0").check(t, graph)
}

// TestProvisionSingleNoMatch tests single app failure when a version constraint doesn't exist.
func TestProvisionSingleNoMatch(t *testing.T) {
	t.Parallel()

	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).get()
	applicationset := newApplicationSet().withApplication(app.Name, getSemver(t, "2.0.0")).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app).Build()

	_, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.Error(t, err)
}

// TestProvisionSingleWithDependency tests a single application with a met dependency.
func TestProvisionSingleWithDependency(t *testing.T) {
	t.Parallel()

	dep := newApplicationBuilder().withVersion(getSemver(t, "2.0.0")).get()
	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withDependency(dep.Name, nil).get()
	applicationset := newApplicationSet().withApplication(app.Name, getSemver(t, "1.0.0")).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app, dep).Build()

	graph, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)

	newExpectedGraph().withAppVersion(dep.Name, "2.0.0").withAppVersion(app.Name, "1.0.0").check(t, graph)
}

// TestProvisionSingleWithDependencyNoMatch tests a single application with an unmet dependency.
func TestProvisionSingleWithDependencyNoMatch(t *testing.T) {
	t.Parallel()

	dep := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).get()
	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withDependency(dep.Name, getConstraints(t, "^2")).get()
	applicationset := newApplicationSet().withApplication(app.Name, getSemver(t, "1.0.0")).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app, dep).Build()

	_, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)
}
