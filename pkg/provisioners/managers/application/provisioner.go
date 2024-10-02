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

	unikornv1 "github.com/unikorn-cloud/application/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/application/pkg/constants"
	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"
	coreclient "github.com/unikorn-cloud/core/pkg/client"
	"github.com/unikorn-cloud/core/pkg/manager"
	"github.com/unikorn-cloud/core/pkg/provisioners"
	identityclient "github.com/unikorn-cloud/identity/pkg/client"
	kubernetesclient "github.com/unikorn-cloud/kubernetes/pkg/client"
	kubernetesapi "github.com/unikorn-cloud/kubernetes/pkg/openapi"
)

var (
	ErrAnnotation = errors.New("required annotation missing")

	ErrResourceDependency = errors.New("resource deplendedncy error")
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
func (p *Provisioner) getKubernetesClient(ctx context.Context, traceName string) (context.Context, kubernetesapi.ClientWithResponsesInterface, error) {
	cli, err := coreclient.ProvisionerClientFromContext(ctx)
	if err != nil {
		return nil, nil, err
	}

	tokenIssuer := identityclient.NewTokenIssuer(cli, p.options.kubernetesOptions, &p.options.clientOptions, constants.Application, constants.Version)

	ctx, err = tokenIssuer.Context(ctx, traceName)
	if err != nil {
		return nil, nil, err
	}

	getter := kubernetesclient.New(cli, p.options.kubernetesOptions, &p.options.clientOptions)

	client, err := getter.Client(ctx)
	if err != nil {
		return nil, nil, err
	}

	return ctx, client, nil
}

// Provision implements the Provision interface.
func (p *Provisioner) Provision(ctx context.Context) error {
	_, _, err := p.getKubernetesClient(ctx, "provision")
	if err != nil {
		return err
	}

	// TODO: do something!
	return nil
}

// Deprovision implements the Provision interface.
func (p *Provisioner) Deprovision(ctx context.Context) error {
	// TODO: do something!
	_, _, err := p.getKubernetesClient(ctx, "deprovision")
	if err != nil {
		return err
	}

	return nil
}
