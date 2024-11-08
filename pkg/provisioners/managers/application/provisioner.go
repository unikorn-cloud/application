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

func varName(applicationID string, version *unikornv1core.SemanticVersion) string {
	return applicationID + "=" + version.String()
}

// SolveApplicationSet walks the graph Dykstra style loading in referenced dependencies.
// Where things get interesting is if a dependency is pulled in multiple times
// but the originally selected version conflicts with other dependency constraints
// then we have a conflict, and have to backtrack and try again with another version.
// Unlike typical SAT solver problems, choosing a different version can have the fun
// effect of changing its dependencies!
//
//nolint:cyclop,gocognit
func SolveApplicationSet(ctx context.Context, client client.Client, namespace string, applicationset *unikornv1.ApplicationSet) error {
	applications, err := getApplications(ctx, client, namespace)
	if err != nil {
		return err
	}

	solver := sat.NewCDCLSolver()

	// Pass 1 is going to do an exhaustive walk of the dependency graph gathering
	// all application/version tuples as variables, and also create any clauses along the way.
	queue := Queue[string]{}

	// Populate the work queue with any application IDs that are requested by the
	// user.
	for _, ref := range applicationset.Spec.Applications {
		application, ok := applications[ref.Application.Name]
		if !ok {
			return fmt.Errorf("%w: requested application %s not in catalog", ErrResourceDependency, ref.Application.Name)
		}

		queue.Push(ref.Application.Name)

		// Add a unit clause if an application version is specified.
		if ref.Version != nil {
			// Non existent version asked for.
			if _, err := application.GetVersion(*ref.Version); err != nil {
				return err
			}

			solver.Clause(solver.Literal(varName(ref.Application.Name, ref.Version)))

			continue
		}

		// Otherise we must install at least one version.
		versions := slices.Collect(application.Versions())
		if len(versions) == 0 {
			return fmt.Errorf("%w: requested application %s has no versions", ErrResourceDependency, application.Name)
		}

		l := make([]*sat.Literal, 0, len(application.Spec.Versions))

		for _, version := range slices.Backward(slices.Collect(application.Versions())) {
			l = append(l, solver.Literal(varName(application.Name, &version.Version)))
		}

		solver.Clause(l...)
	}

	visited := map[string]bool{}

	for !queue.Empty() {
		id := queue.Pop()

		if _, ok := visited[id]; ok {
			continue
		}

		visited[id] = true

		application, ok := applications[id]
		if !ok {
			return fmt.Errorf("%w: unable to locate application %s", ErrResourceDependency, id)
		}

		for i, version := range application.Spec.Versions {
			name := varName(application.Name, &version.Version)

			// Only one version of the application may be installed at any time...
			// We basically do a permute of all possible combinations and if both
			// are true, we want a false, so ^(A ^ B) or ^A v ^B.
			for _, other := range application.Spec.Versions[i+1:] {
				solver.Clause(solver.NegatedLiteral(name), solver.NegatedLiteral(varName(application.Name, &other.Version)))
			}

			// Next if an application version is installed, we need to ensure any
			// dependent applications are also installed, but constrained to the allowed
			// set for this application.  So A => B v C v D becomes ^A v B v C v D.
			// This also has the property that if a version has no satisfiable deps e.g.
			// A => , then it will add a unit clause ^A to ensure it cannot be installed.
			for _, dependency := range version.Dependencies {
				dependantApplication := applications[dependency.Name]

				l := []*sat.Literal{
					solver.NegatedLiteral(name),
				}

				for _, depVersion := range slices.Backward(slices.Collect(dependantApplication.Versions())) {
					if dependency.Constraints == nil || dependency.Constraints.Check(&depVersion.Version) {
						l = append(l, solver.Literal(varName(dependency.Name, &depVersion.Version)))
					}
				}

				solver.Clause(l...)

				queue.Push(dependency.Name)
			}
		}
	}

	// Pass 2 does the actual solving...
	if !solver.Solve(sat.DefaultChooser) {
		return fmt.Errorf("%w: unsolvable", ErrConstraint)
	}

	return nil
}

func GenerateProvisioner(ctx context.Context, client client.Client, namespace string, applicationset *unikornv1.ApplicationSet) (provisioners.Provisioner, error) {
	err := SolveApplicationSet(ctx, client, namespace, applicationset)
	if err != nil {
		return nil, err
	}

	//nolint:nilnil
	return nil, nil
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
