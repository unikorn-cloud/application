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

package application

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/pflag"
	sat "github.com/spjmurray/go-sat"

	unikornv1 "github.com/unikorn-cloud/application/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/application/pkg/constants"
	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"
	coreclient "github.com/unikorn-cloud/core/pkg/client"
	"github.com/unikorn-cloud/core/pkg/manager"
	"github.com/unikorn-cloud/core/pkg/provisioners"
	identityclient "github.com/unikorn-cloud/identity/pkg/client"
	kubernetesclient "github.com/unikorn-cloud/kubernetes/pkg/client"
	kubernetesapi "github.com/unikorn-cloud/kubernetes/pkg/openapi"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrAnnotation = errors.New("required annotation missing")

	ErrResourceDependency = errors.New("resource dependedncy error")

	ErrGraph = errors.New("graph error")

	ErrConstraint = errors.New("constraint error")
)

// Options allows access to CLI options in the provisioner.
type Options struct {
	// kubernetesOptions allows the kubernetes host and CA to be set.
	kubernetesOptions *kubernetesclient.Options
	// clientOptions give access to client certificate information as
	// we need to talk to kubernetes to get a token, and then to kubernetes
	// to ensure cloud identities and networks are provisioned, as well
	// as deptovisioning them.
	clientOptions coreclient.HTTPClientOptions
}

func (o *Options) AddFlags(f *pflag.FlagSet) {
	if o.kubernetesOptions == nil {
		o.kubernetesOptions = kubernetesclient.NewOptions()
	}

	o.kubernetesOptions.AddFlags(f)
	o.clientOptions.AddFlags(f)
}

// Provisioner encapsulates control plane provisioning.
type Provisioner struct {
	provisioners.Metadata

	// applicationset is the application set we're provisioning.
	applicationset unikornv1.ApplicationSet

	// options are documented for the type.
	options *Options
}

// New returns a new initialized provisioner object.
func New(options manager.ControllerOptions) provisioners.ManagerProvisioner {
	o, _ := options.(*Options)

	return &Provisioner{
		options: o,
	}
}

// Ensure the ManagerProvisioner interface is implemented.
var _ provisioners.ManagerProvisioner = &Provisioner{}

func (p *Provisioner) Object() unikornv1core.ManagableResourceInterface {
	return &p.applicationset
}

// getKubernetesClient returns an authenticated context with a client credentials access token
// and a client.  The context must be used by subseqent API calls in order to extract
// the access token.
//
//nolint:unparam
func (p *Provisioner) getKubernetesClient(ctx context.Context, traceName string) (context.Context, kubernetesapi.ClientWithResponsesInterface, error) {
	cli, err := coreclient.ProvisionerClientFromContext(ctx)
	if err != nil {
		return nil, nil, err
	}

	tokenIssuer := identityclient.NewTokenIssuer(cli, p.options.kubernetesOptions, &p.options.clientOptions, constants.Application, constants.Version)

	token, err := tokenIssuer.Issue(ctx, traceName)
	if err != nil {
		return nil, nil, err
	}

	getter := kubernetesclient.New(cli, p.options.kubernetesOptions, &p.options.clientOptions)

	client, err := getter.Client(ctx, token)
	if err != nil {
		return nil, nil, err
	}

	return ctx, client, nil
}

func getApplications(ctx context.Context, cli client.Client, namespace string) (map[string]*unikornv1core.HelmApplication, error) {
	var applications unikornv1core.HelmApplicationList

	options := &client.ListOptions{
		Namespace: namespace,
	}

	if err := cli.List(ctx, &applications, options); err != nil {
		return nil, err
	}

	applicationMap := map[string]*unikornv1core.HelmApplication{}

	for i, application := range applications.Items {
		applicationMap[application.Name] = &applications.Items[i]
	}

	return applicationMap, nil
}

// HelmApplicationVersionIterator simplifies iteration over application
// versions and returns an ordered list (newest to oldest), of semantic
// versions.
type HelmApplicationVersionIterator struct {
	application *unikornv1core.HelmApplication
}

func NewHelmApplicationVersionIterator(application *unikornv1core.HelmApplication) *HelmApplicationVersionIterator {
	return &HelmApplicationVersionIterator{
		application: application,
	}
}

type Queue[T any] struct {
	items []T
}

func (q *Queue[T]) Empty() bool {
	return len(q.items) == 0
}

func (q *Queue[T]) Push(value T) {
	q.items = append(q.items, value)
}

func (q *Queue[T]) Pop() T {
	value := q.items[0]
	q.items = q.items[1:]

	return value
}

// GraphVisitor is used to visit a node in the graph.
type GraphVisitor[T comparable] interface {
	// Visit is called when a new node is encountered, it accepts
	// the node itself and an enqueue function.
	Visit(node T, enqueue func(T)) error
}

type GraphWalker[T comparable] struct {
	queue Queue[T]
	seen  sat.Set[T]
}

func NewGraphWalker[T comparable]() *GraphWalker[T] {
	return &GraphWalker[T]{
		seen: sat.Set[T]{},
	}
}

func (g *GraphWalker[T]) Enqueue(t T) {
	g.queue.Push(t)
}

func (g *GraphWalker[T]) Walk(visitor GraphVisitor[T]) error {
	for !g.queue.Empty() {
		t := g.queue.Pop()

		if g.seen.Has(t) {
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
	id      string
	version unikornv1core.SemanticVersion
}

func NewAppVersion(id string, version *unikornv1core.SemanticVersion) AppVersion {
	return AppVersion{
		id:      id,
		version: *version,
	}
}

type solverVisitor struct {
	applications map[string]*unikornv1core.HelmApplication
	model        *sat.Model[AppVersion]
}

func (v *solverVisitor) Visit(id string, enqueue func(string)) error {
	application, ok := v.applications[id]
	if !ok {
		return fmt.Errorf("%w: unable to locate application %s", ErrResourceDependency, id)
	}

	appVersions := make([]AppVersion, 0, len(application.Spec.Versions))

	for version := range application.Versions() {
		appVersions = append(appVersions, AppVersion{application.Name, version.Version})
	}

	// Only one version of the application may be installed at any time...
	v.model.AtMostOneOf(appVersions...)

	// Next if an application version is installed, we need to ensure any
	// dependent applications are also installed, but constrained to the allowed
	// set for this application. This also has the property that if a version has
	// no satisfiable deps e.g. then it will add a unary clause that prevents the
	// version from being used.
	for version := range application.Versions() {
		av := AppVersion{application.Name, version.Version}

		for _, dependency := range version.Dependencies {
			dependantApplication, ok := v.applications[dependency.Name]
			if !ok {
				return fmt.Errorf("%w: requested application %s not in catalog", ErrResourceDependency, dependency.Name)
			}

			depVersions := make([]AppVersion, 0, len(dependantApplication.Spec.Versions))

			for _, depVersion := range slices.Backward(slices.Collect(dependantApplication.Versions())) {
				if dependency.Constraints == nil || dependency.Constraints.Check(&depVersion.Version) {
					depVersions = append(depVersions, AppVersion{dependency.Name, depVersion.Version})
				}
			}

			v.model.ImpliesAtLeastOneOf(av, depVersions...)

			enqueue(dependency.Name)
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
//
//nolint:cyclop
func SolveApplicationSet(ctx context.Context, client client.Client, namespace string, applicationset *unikornv1.ApplicationSet) (sat.Set[AppVersion], error) {
	applications, err := getApplications(ctx, client, namespace)
	if err != nil {
		return nil, err
	}

	// We're going to do an exhaustive walk of the dependency graph gathering
	// all application/version tuples as variables, and also create any clauses along the way.
	graph := NewGraphWalker[string]()

	model := sat.NewModel[AppVersion]()

	// Populate the work queue with any application IDs that are requested by the
	// user and any clauses relevant to the solver.
	for _, ref := range applicationset.Spec.Applications {
		application, ok := applications[ref.Application.Name]
		if !ok {
			return nil, fmt.Errorf("%w: requested application %s not in catalog", ErrResourceDependency, ref.Application.Name)
		}

		graph.Enqueue(ref.Application.Name)

		// Add a unit clause if an application version is specified.
		if ref.Version != nil {
			// Non existent version asked for.
			if _, err := application.GetVersion(*ref.Version); err != nil {
				return nil, err
			}

			model.Unary(AppVersion{ref.Application.Name, *ref.Version})

			continue
		}

		// Otherise we must install at least one version.
		// NOTE: we cheat a bit here, when making a choice the solver will pick
		// the first undefined variable and set it to true, so we implicitly
		// choose the most recent version by adding them in a descending order.
		versions := slices.Collect(application.Versions())
		if len(versions) == 0 {
			return nil, fmt.Errorf("%w: requested application %s has no versions", ErrResourceDependency, application.Name)
		}

		l := make([]AppVersion, len(versions))

		for i, version := range slices.Backward(versions) {
			l[i] = AppVersion{application.Name, version.Version}
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
	if err := sat.NewCDCLSolver().Solve(model, sat.DefaultChooser); err != nil {
		return nil, err
	}

	// Get the result.
	result := sat.Set[AppVersion]{}

	for av, b := range model.Variables() {
		if b.Value() {
			result.Add(av)
		}
	}

	return result, nil
}

type schedulerVistor struct {
	applications map[string]*unikornv1core.HelmApplication
	appVersions  map[string]AppVersion
	dependers    map[string][]string
	seen         sat.Set[string]
	order        []AppVersion
}

func (v *schedulerVistor) Visit(av AppVersion, enqueue func(AppVersion)) error {
	v.seen.Add(av.id)

	v.order = append(v.order, av)

	// Doea anyone depdend on me?
	if dependers, ok := v.dependers[av.id]; ok {
		for _, depender := range dependers {
			dependerAppVersion := v.appVersions[depender]

			application := v.applications[depender]

			version, err := application.GetVersion(dependerAppVersion.version)
			if err != nil {
				return err
			}

			// If all their dependencies are fulfilled, add to the queue.
			satisfied := true

			for _, dep := range version.Dependencies {
				if !v.seen.Has(dep.Name) {
					satisfied = false

					break
				}
			}

			if satisfied {
				enqueue(dependerAppVersion)
			}
		}
	}

	return nil
}

func Schedule(ctx context.Context, client client.Client, namespace string, solution sat.Set[AppVersion]) ([]AppVersion, error) {
	applications, err := getApplications(ctx, client, namespace)
	if err != nil {
		return nil, err
	}

	// Okay, we need to build up a reverse map of dependencies and also
	// record the packages with no dependencies as those will be installed
	// first.
	dependers := map[string][]string{}
	appVersions := map[string]AppVersion{}

	var roots []AppVersion

	for av := range solution.All() {
		appVersions[av.id] = av

		application := applications[av.id]

		version, err := application.GetVersion(av.version)
		if err != nil {
			return nil, err
		}

		if len(version.Dependencies) == 0 {
			roots = append(roots, av)

			continue
		}

		for _, dep := range version.Dependencies {
			dependers[dep.Name] = append(dependers[dep.Name], av.id)
		}
	}

	// Then we need to walk the graph from the roots to the leaves, but only
	// processing nodes once all their dependencies are satisfied.
	graph := NewGraphWalker[AppVersion]()

	for _, root := range roots {
		graph.Enqueue(root)
	}

	visitor := &schedulerVistor{
		applications: applications,
		appVersions:  appVersions,
		dependers:    dependers,
		seen:         sat.Set[string]{},
	}

	if err := graph.Walk(visitor); err != nil {
		return nil, err
	}

	return visitor.order, nil
}

// Provision implements the Provision interface.
func (p *Provisioner) Provision(ctx context.Context) error {
	clientContext, client, err := p.getKubernetesClient(ctx, "provision")
	if err != nil {
		return err
	}

	// TODO: do something!
	_, _ = clientContext, client

	return nil
}

// Deprovision implements the Provision interface.
func (p *Provisioner) Deprovision(ctx context.Context) error {
	clientContext, client, err := p.getKubernetesClient(ctx, "deprovision")
	if err != nil {
		return err
	}

	// TODO: do something!
	_, _ = clientContext, client

	return nil
}
