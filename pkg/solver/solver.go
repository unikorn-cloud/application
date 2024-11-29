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

package solver

import (
	"context"
	"errors"
	"fmt"

	"github.com/spjmurray/go-sat/pkg/cdcl"
	"github.com/spjmurray/go-util/pkg/queue"
	"github.com/spjmurray/go-util/pkg/set"

	unikornv1 "github.com/unikorn-cloud/application/pkg/apis/unikorn/v1alpha1"
	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/core/pkg/constants"
)

var (
	ErrAnnotation = errors.New("required annotation missing")

	ErrResourceDependency = errors.New("resource dependedncy error")

	ErrGraph = errors.New("graph error")

	ErrConstraint = errors.New("constraint error")
)

// GraphVisitor is used to visit a node in the graph.
type GraphVisitor[T comparable] interface {
	// Visit is called when a new node is encountered, it accepts
	// the node itself and an enqueue function.
	Visit(node T, enqueue func(T)) error
}

type GraphWalker[T comparable] struct {
	queue *queue.Queue[T]
	seen  set.Set[T]
}

func NewGraphWalker[T comparable]() *GraphWalker[T] {
	return &GraphWalker[T]{
		queue: queue.New[T](),
		seen:  set.New[T](),
	}
}

func (g *GraphWalker[T]) Enqueue(t T) {
	g.queue.Push(t)
}

func (g *GraphWalker[T]) Walk(visitor GraphVisitor[T]) error {
	for !g.queue.Empty() {
		t, err := g.queue.Pop()
		if err != nil {
			return err
		}

		if g.seen.Contains(t) {
			continue
		}

		g.seen.Add(t)

		if err := visitor.Visit(t, g.Enqueue); err != nil {
			return err
		}
	}

	return nil
}

// AppVersion wraps up applicationID and version tuples in a comparable
// and easy to use form when interacting with the SAT solver.
type AppVersion struct {
	Name    string
	Version unikornv1core.SemanticVersion
}

func NewAppVersion(name string, version unikornv1core.SemanticVersion) AppVersion {
	return AppVersion{
		Name:    name,
		Version: version,
	}
}

type ApplicationIndex map[string]*unikornv1core.HelmApplication

func NewApplicationIndex(applications ...*unikornv1core.HelmApplication) (ApplicationIndex, error) {
	index := ApplicationIndex{}

	for _, application := range applications {
		if application.Labels == nil {
			return nil, fmt.Errorf("%w: application ID %s has no labels", ErrResourceDependency, application.Name)
		}

		name, ok := application.Labels[constants.NameLabel]
		if !ok {
			return nil, fmt.Errorf("%w: application ID %s has no name", ErrResourceDependency, application.Name)
		}

		index[name] = application
	}

	return index, nil
}

func (i ApplicationIndex) Get(name string) (*unikornv1core.HelmApplication, error) {
	application, ok := i[name]
	if !ok {
		return nil, fmt.Errorf("%w: unable to locate application %s", ErrResourceDependency, name)
	}

	return application, nil
}

type solverVisitor struct {
	applications ApplicationIndex
	model        *cdcl.Model[AppVersion]
}

//nolint:cyclop
func (v *solverVisitor) Visit(name string, enqueue func(string)) error {
	application, err := v.applications.Get(name)
	if err != nil {
		return err
	}

	appVersions := make([]AppVersion, 0, len(application.Spec.Versions))

	for version := range application.Versions() {
		appVersions = append(appVersions, NewAppVersion(name, version.Version))
	}

	// Only one version of the application may be installed at any time...
	v.model.AtMostOneOf(appVersions...)

	// Next if an application version is installed, we need to ensure any
	// dependent applications are also installed, but constrained to the allowed
	// set for this application. This also has the property that if a version has
	// no satisfiable deps e.g. then it will add a unary clause that prevents the
	// version from being used.  If a version is installed, it also implies any
	// recommended packages should be installed too.
	for version := range application.Versions() {
		av := NewAppVersion(name, version.Version)

		for _, dependency := range version.Dependencies {
			dependantApplication, err := v.applications.Get(dependency.Name)
			if err != nil {
				return err
			}

			depVersions := make([]AppVersion, 0, len(dependantApplication.Spec.Versions))

			for depVersion := range dependantApplication.Versions() {
				if dependency.Constraints == nil || dependency.Constraints.Check(&depVersion.Version) {
					depVersions = append(depVersions, NewAppVersion(dependency.Name, depVersion.Version))
				}
			}

			v.model.ImpliesAtLeastOneOf(av, depVersions...)

			enqueue(dependency.Name)
		}

		for _, recommendation := range version.Recommends {
			recommendedApplication, err := v.applications.Get(recommendation.Name)
			if err != nil {
				return err
			}

			recVersions := make([]AppVersion, 0, len(recommendedApplication.Spec.Versions))

			for recVersion := range recommendedApplication.Versions() {
				recVersions = append(recVersions, NewAppVersion(recommendation.Name, recVersion.Version))
			}

			v.model.ImpliesAtLeastOneOf(av, recVersions...)

			enqueue(recommendation.Name)
		}
	}

	return nil
}

// SolveApplicationSet walks the graph Dykstra style loading in referenced dependencies.
// Where things get interesting is if a dependency is pulled in multiple times
// but the originally selected version conflicts with other dependency constraints
// then we have a conflict, and have to backtrack and try again with another version.
// Unlike typical SAT solver problems, choosing a different version can have the fun
// effect of changing its dependencies!
func SolveApplicationSet(ctx context.Context, applications ApplicationIndex, applicationset *unikornv1.ApplicationSet) (set.Set[AppVersion], error) {
	// We're going to do an exhaustive walk of the dependency graph gathering
	// all application/version tuples as variables, and also create any clauses along the way.
	graph := NewGraphWalker[string]()

	model := cdcl.NewModel[AppVersion]()

	// Populate the work queue with any application IDs that are requested by the
	// user and any clauses relevant to the solver.
	for _, ref := range applicationset.Spec.Applications {
		application, err := applications.Get(ref.Name)
		if err != nil {
			return nil, err
		}

		graph.Enqueue(ref.Name)

		// Add a unit clause if an application version is specified.
		if ref.Version != nil {
			// Non existent version asked for.
			if _, err := application.GetVersion(*ref.Version); err != nil {
				return nil, err
			}

			model.Unary(NewAppVersion(ref.Name, *ref.Version))

			continue
		}

		l := make([]AppVersion, 0, len(application.Spec.Versions))

		for version := range application.Versions() {
			l = append(l, NewAppVersion(ref.Name, version.Version))
		}

		model.AtLeastOneOf(l...)
	}

	// Do the walk.
	visitor := &solverVisitor{
		applications: applications,
		model:        model,
	}

	if err := graph.Walk(visitor); err != nil {
		return nil, err
	}

	// Solve the problem.
	if err := cdcl.New().Solve(model, cdcl.DefaultChooser); err != nil {
		return nil, err
	}

	// Get the result.
	result := set.Set[AppVersion]{}

	for av, b := range model.Variables() {
		if b.Value() {
			result.Add(av)
		}
	}

	return result, nil
}
