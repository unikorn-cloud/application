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
}

// newApplicationBuilder createa a new application builder.
func newApplicationBuilder() *applicationBuilder {
	return &applicationBuilder{
		dependencies: map[string][]unikornv1core.HelmApplicationDependency{},
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

// TestProvisionSingle tests a single app is solved.
func TestProvisionSingle(t *testing.T) {
	t.Parallel()

	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).get()
	applicationset := newApplicationSet().withApplication(app.Name, getSemver(t, "1.0.0")).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app).Build()

	_, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)
}

// TestProvisionSingleMostRecent tests a single app is solved with the most recent version
// when it isn't specified.
func TestProvisionSingleMostRecent(t *testing.T) {
	t.Parallel()

	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withVersion(getSemver(t, "2.0.0")).get()
	applicationset := newApplicationSet().withApplication(app.Name, nil).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app).Build()

	_, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)
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

	_, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)
}

// TestProvisionSingleWithDependencyNoMatch tests a single application with an unmet dependency.
func TestProvisionSingleWithDependencyNoMatch(t *testing.T) {
	t.Parallel()

	dep := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).get()
	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withDependency(dep.Name, getConstraints(t, "^2")).get()
	applicationset := newApplicationSet().withApplication(app.Name, getSemver(t, "1.0.0")).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app, dep).Build()

	_, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.Error(t, err)
}

// TestProvisionMultipleWithDependencyConflict tests that two apps with conflicting dependency
// information, but a solvable outcome succeeds.  This test checks that constraints are
// correctly accumulatd and applied when selecting a dependency with multiple consumers.
func TestProvisionMultipleWithDependencyConflict(t *testing.T) {
	t.Parallel()

	dep := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withVersion(getSemver(t, "2.0.0")).get()

	// NOTE: here we say app1 (which should be processed first) can accespt any version >=1.0.0,
	// so 2.0.0 should be selected.  But, app2 overrides that by allowing only >=1.0.0 and <2.0.0.
	app1 := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withDependency(dep.Name, getConstraints(t, ">=1.0.0")).get()
	app2 := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withDependency(dep.Name, getConstraints(t, "~1")).get()
	applicationset := newApplicationSet().withApplication(app1.Name, getSemver(t, "1.0.0")).withApplication(app2.Name, getSemver(t, "1.0.0")).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app1, app2, dep).Build()

	_, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)
}

// TestProvisionSingleWithConflictingTransitveDependency tests that two apps with conflicting dependency
// information, but a solvable outcome succeeds.  This test checks that a dependency that has already
// been added, but is include again later with an incompatible constraint undoes the guess, remebering
// the conflicting constraint when retrying.
func TestProvisionSingleWithConflictingTransitveDependency(t *testing.T) {
	t.Parallel()

	dep := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withVersion(getSemver(t, "2.0.0")).get()
	intermediateDep := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withDependency(dep.Name, getConstraints(t, "~1")).get()

	// NOTE: what will happen here is dep will be processed before intermediateDep, so that will select
	// 2.0.0 as there are no constraints.  When intermediateDep is processed it will note that dep already
	// exists, but with a version incompatible with its contraint of >=1.0.0 >2.0.0.  At this
	// point it needs to roll back to the epoch where we guessed the version of dep, but with the
	// extra new constraint in place.
	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withDependency(dep.Name, nil).withDependency(intermediateDep.Name, nil).get()
	applicationset := newApplicationSet().withApplication(app.Name, getSemver(t, "1.0.0")).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app, dep, intermediateDep).Build()

	_, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)
}

// TestProvisionSingleWithChoice makes sure where multiple choices are available
// and the desirable outcome has a conflict, we satisfy the problem.
func TestProvisionSingleWithChoice(t *testing.T) {
	t.Parallel()

	dep := newApplicationBuilder().
		withVersion(getSemver(t, "1.0.0")).
		withVersion(getSemver(t, "2.0.0")).
		get()
	idep1 := newApplicationBuilder().
		withVersion(getSemver(t, "1.0.0")).
		withDependency(dep.Name, getConstraints(t, "=1.0.0")).
		withVersion(getSemver(t, "2.0.0")).
		withDependency(dep.Name, getConstraints(t, "=2.0.0")).
		get()
	idep2 := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).withDependency(dep.Name, getConstraints(t, "=1.0.0")).get()

	// NOTE: the solver will be forced to chosse 2.0.0 first as it's the latest version,
	// this will pull in idep1 with version 2.0.0, but that will conflict with idep2, so
	// we need to backtrack and start again.
	app := newApplicationBuilder().
		withVersion(getSemver(t, "1.0.0")).
		withDependency(idep1.Name, getConstraints(t, "=1.0.0")).
		withDependency(idep2.Name, getConstraints(t, "=1.0.0")).
		withVersion(getSemver(t, "2.0.0")).
		withDependency(idep1.Name, getConstraints(t, "=2.0.0")).
		withDependency(idep2.Name, getConstraints(t, "=1.0.0")).
		get()

	applicationset := newApplicationSet().withApplication(app.Name, nil).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app, dep, idep1, idep2).Build()

	solution, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)

	order, err := application.Schedule(context.Background(), client, namespace, solution)
	require.NoError(t, err)

	// TODO: the order of the middle two doesn't really matter as they can be done in
	// parallel, but it's non-deterministic, so we need a better way of checking this.
	require.Len(t, order, 4)
	require.Equal(t, application.NewAppVersion(dep.Name, getSemver(t, "1.0.0")), order[0])
	require.Equal(t, application.NewAppVersion(app.Name, getSemver(t, "1.0.0")), order[3])
}

// TestProvisionSingleWithChoiceAndConditionalDependency checks for the "phanom package"
// problem, where a dependency occurs on a specific version.  The selection heuristic should
// not install it if it's not explicitly depended upon.
func TestProvisionSingleWithChoiceAndConditionalDependency(t *testing.T) {
	t.Parallel()

	dep := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).get()
	app := newApplicationBuilder().
		withVersion(getSemver(t, "1.0.0")).
		withDependency(dep.Name, getConstraints(t, "=1.0.0")).
		withVersion(getSemver(t, "2.0.0")).
		get()

	applicationset := newApplicationSet().withApplication(app.Name, nil).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app, dep).Build()
	solution, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)

	order, err := application.Schedule(context.Background(), client, namespace, solution)
	require.NoError(t, err)

	expected := []application.AppVersion{
		application.NewAppVersion(app.Name, getSemver(t, "2.0.0")),
	}

	require.Equal(t, expected, order)
}

// TestProvisionSingleWithRecommendation tests recommendations are correctly picked up
// and applied.
func TestProvisionSingleWithRecommendation(t *testing.T) {
	t.Parallel()

	app := newApplicationBuilder().withVersion(getSemver(t, "1.0.0")).get()

	rec := newApplicationBuilder().
		withVersion(getSemver(t, "1.0.0")).
		withDependency(app.Name, getConstraints(t, "=1.0.0")).
		get()

	app.Spec.Versions[0].Recommends = []unikornv1core.HelmApplicationRecommendation{
		{
			Name: rec.Name,
		},
	}

	applicationset := newApplicationSet().withApplication(app.Name, nil).get()

	client := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(app, rec).Build()
	solution, err := application.SolveApplicationSet(context.Background(), client, namespace, applicationset)
	require.NoError(t, err)

	order, err := application.Schedule(context.Background(), client, namespace, solution)
	require.NoError(t, err)

	expected := []application.AppVersion{
		application.NewAppVersion(app.Name, getSemver(t, "1.0.0")),
		application.NewAppVersion(rec.Name, getSemver(t, "1.0.0")),
	}

	require.Equal(t, expected, order)
}
