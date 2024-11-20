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

	"github.com/spf13/pflag"
	sat "github.com/spjmurray/go-sat"

	unikornv1 "github.com/unikorn-cloud/application/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/application/pkg/constants"
	"github.com/unikorn-cloud/application/pkg/solver"
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

type schedulerVistor struct {
	applications map[string]*unikornv1core.HelmApplication
	appVersions  map[string]solver.AppVersion
	dependers    map[string][]string
	seen         sat.Set[string]
	order        []solver.AppVersion
}

func (v *schedulerVistor) Visit(av solver.AppVersion, enqueue func(solver.AppVersion)) error {
	v.seen.Add(av.ID)

	v.order = append(v.order, av)

	// Doea anyone depdend on me?
	if dependers, ok := v.dependers[av.ID]; ok {
		for _, depender := range dependers {
			dependerAppVersion := v.appVersions[depender]

			application := v.applications[depender]

			version, err := application.GetVersion(dependerAppVersion.Version)
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

func Schedule(ctx context.Context, client client.Client, namespace string, solution sat.Set[solver.AppVersion]) ([]solver.AppVersion, error) {
	applications, err := getApplications(ctx, client, namespace)
	if err != nil {
		return nil, err
	}

	// Okay, we need to build up a reverse map of dependencies and also
	// record the packages with no dependencies as those will be installed
	// first.
	dependers := map[string][]string{}
	appVersions := map[string]solver.AppVersion{}

	var roots []solver.AppVersion

	for av := range solution.All() {
		appVersions[av.ID] = av

		application := applications[av.ID]

		version, err := application.GetVersion(av.Version)
		if err != nil {
			return nil, err
		}

		if len(version.Dependencies) == 0 {
			roots = append(roots, av)

			continue
		}

		for _, dep := range version.Dependencies {
			dependers[dep.Name] = append(dependers[dep.Name], av.ID)
		}
	}

	// Then we need to walk the graph from the roots to the leaves, but only
	// processing nodes once all their dependencies are satisfied.
	graph := solver.NewGraphWalker[solver.AppVersion]()

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
