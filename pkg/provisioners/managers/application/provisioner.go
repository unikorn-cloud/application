/*
Copyright 2024-2025 the Unikorn Authors.

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

	"github.com/spf13/pflag"
	"github.com/spjmurray/go-util/pkg/graph"
	"github.com/spjmurray/go-util/pkg/set"

	unikornv1 "github.com/unikorn-cloud/application/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/application/pkg/solver"
	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/core/pkg/cd"
	coreclient "github.com/unikorn-cloud/core/pkg/client"
	"github.com/unikorn-cloud/core/pkg/manager"
	"github.com/unikorn-cloud/core/pkg/provisioners"
	"github.com/unikorn-cloud/core/pkg/provisioners/application"
	"github.com/unikorn-cloud/core/pkg/provisioners/remotecluster"
	"github.com/unikorn-cloud/core/pkg/provisioners/serial"
	unikornv1kubernetes "github.com/unikorn-cloud/kubernetes/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/kubernetes/pkg/provisioners/helmapplications/clusteropenstack"
	"github.com/unikorn-cloud/kubernetes/pkg/provisioners/helmapplications/vcluster"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrAnnotation = errors.New("required annotation missing")

	ErrResourceDependency = errors.New("resource dependedncy error")

	ErrGraph = errors.New("graph error")

	ErrConstraint = errors.New("constraint error")

	ErrClusterManager = errors.New("cluster manager lookup failed")
)

// Options allows access to CLI options in the provisioner.
type Options struct {
}

func (o *Options) AddFlags(f *pflag.FlagSet) {
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

// getApplicationIndex loads up all applications we have defined.
func getApplicationIndex(ctx context.Context, cli client.Client, namespace string) (solver.ApplicationIndex, error) {
	var applications unikornv1core.HelmApplicationList

	options := &client.ListOptions{
		Namespace: namespace,
	}

	if err := cli.List(ctx, &applications, options); err != nil {
		return nil, err
	}

	l := make([]*unikornv1core.HelmApplication, len(applications.Items))

	for i := range applications.Items {
		l[i] = &applications.Items[i]
	}

	return solver.NewApplicationIndex(l...)
}

// schedulerVistor is used to walk the graph of application versions from the SAT solution.
// It builds up an order in which to install applications so that all dependencies are
// satisfied before an application is installed.
type schedulerVistor struct {
	applications solver.ApplicationIndex
	appVersions  map[string]solver.AppVersion
	dependers    map[string][]string
	seen         set.Set[string]
	order        []solver.AppVersion
}

func (v *schedulerVistor) Visit(av solver.AppVersion, enqueue func(solver.AppVersion)) error {
	v.seen.Add(av.Name)

	v.order = append(v.order, av)

	// Doea anyone depdend on me?
	if dependers, ok := v.dependers[av.Name]; ok {
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
				if !v.seen.Contains(dep.Name) {
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

// Schedule takes a SAT solution and creates an installation order for the applications.
func Schedule(ctx context.Context, applications solver.ApplicationIndex, solution set.Set[solver.AppVersion]) ([]solver.AppVersion, error) {
	// Okay, we need to build up a reverse map of dependencies and also
	// record the packages with no dependencies as those will be installed
	// first.
	dependers := map[string][]string{}
	appVersions := map[string]solver.AppVersion{}

	var roots []solver.AppVersion

	for av := range solution.All() {
		appVersions[av.Name] = av

		application := applications[av.Name]

		version, err := application.GetVersion(av.Version)
		if err != nil {
			return nil, err
		}

		if len(version.Dependencies) == 0 {
			roots = append(roots, av)

			continue
		}

		for _, dep := range version.Dependencies {
			dependers[dep.Name] = append(dependers[dep.Name], av.Name)
		}
	}

	// Then we need to walk the graph from the roots to the leaves, but only
	// processing nodes once all their dependencies are satisfied.
	graph := graph.NewWalker[solver.AppVersion]()

	for _, root := range roots {
		graph.Push(root)
	}

	visitor := &schedulerVistor{
		applications: applications,
		appVersions:  appVersions,
		dependers:    dependers,
		seen:         set.Set[string]{},
	}

	if err := graph.Visit(visitor); err != nil {
		return nil, err
	}

	return visitor.order, nil
}

// getClusterManager gets the control plane object that owns this cluster.
func (p *Provisioner) getClusterManager(ctx context.Context, cluster *unikornv1kubernetes.KubernetesCluster) (*unikornv1kubernetes.ClusterManager, error) {
	cli, err := coreclient.ProvisionerClientFromContext(ctx)
	if err != nil {
		return nil, err
	}

	key := client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      cluster.Spec.ClusterManagerID,
	}

	var clusterManager unikornv1kubernetes.ClusterManager

	if err := cli.Get(ctx, key, &clusterManager); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrClusterManager, err.Error())
	}

	return &clusterManager, nil
}

// removeOrphanedApplications does exactly that, we can only see what the user currently
// wants installed, so we need to inspect what the CD driver can see on the system and
// manually prune anything that's installed by shouldn't be.
func (p *Provisioner) removeOrphanedApplications(ctx context.Context, required set.Set[solver.AppVersion]) error {
	// List all applications that exist for this resource.
	labels, err := p.applicationset.ResourceLabels()
	if err != nil {
		return err
	}

	id := &cd.ResourceIdentifier{
		Labels: make([]cd.ResourceIdentifierLabel, 0, len(labels)),
	}

	for k, v := range labels {
		id.Labels = append(id.Labels, cd.ResourceIdentifierLabel{
			Name:  k,
			Value: v,
		})
	}

	applications, err := cd.FromContext(ctx).ListHelmApplications(ctx, id)
	if err != nil {
		return err
	}

	// Perform a boolean subtraction to find the ones that are orhpaned.
	// Use only the name, ignore versions as that whittles down to an upgrade
	// or downgrade.
	for id := range applications {
		found := false

		for r := range required.All() {
			if id.Name == r.Name {
				found = true

				break
			}
		}

		if !found {
			if err := cd.FromContext(ctx).DeleteHelmApplication(ctx, id, true); err != nil {
				return err
			}
		}
	}

	return nil
}

// getProvisioner creates the provisioner.
func (p *Provisioner) getProvisioner(ctx context.Context) (provisioners.Provisioner, error) {
	cli, err := coreclient.ProvisionerClientFromContext(ctx)
	if err != nil {
		return nil, err
	}

	namespace, err := coreclient.NamespaceFromContext(ctx)
	if err != nil {
		return nil, err
	}

	index, err := getApplicationIndex(ctx, cli, namespace)
	if err != nil {
		return nil, err
	}

	solution, err := solver.SolveApplicationSet(ctx, index, &p.applicationset)
	if err != nil {
		return nil, err
	}

	if err := p.removeOrphanedApplications(ctx, solution); err != nil {
		return nil, err
	}

	order, err := Schedule(ctx, index, solution)
	if err != nil {
		return nil, err
	}

	apps := make([]provisioners.Provisioner, len(order))

	for i, av := range order {
		applicationGetter := func(context.Context) (*unikornv1core.HelmApplication, *unikornv1core.SemanticVersion, error) {
			return index[av.Name], &av.Version, nil
		}

		apps[i] = application.New(applicationGetter)
	}

	cluster := &unikornv1kubernetes.KubernetesCluster{}

	if err := cli.Get(ctx, client.ObjectKey{Namespace: p.applicationset.Namespace, Name: p.applicationset.Name}, cluster); err != nil {
		return nil, err
	}

	clusterManager, err := p.getClusterManager(ctx, cluster)
	if err != nil {
		return nil, err
	}

	remoteClusterManager := remotecluster.New(vcluster.NewRemoteCluster(cluster.Namespace, clusterManager.Name, clusterManager), false)

	remoteCluster := remotecluster.New(clusteropenstack.NewRemoteCluster(cluster), false)

	provisioner := remoteClusterManager.ProvisionOn(
		remoteCluster.ProvisionOn(
			serial.New("application-set", apps...),
			remotecluster.BackgroundDeletion,
		),
	)

	return provisioner, nil
}

// Provision implements the Provision interface.
func (p *Provisioner) Provision(ctx context.Context) error {
	provisioner, err := p.getProvisioner(ctx)
	if err != nil {
		return err
	}

	return provisioner.Provision(ctx)
}

// Deprovision implements the Provision interface.
func (p *Provisioner) Deprovision(ctx context.Context) error {
	provisioner, err := p.getProvisioner(ctx)
	if err != nil {
		return err
	}

	return provisioner.Deprovision(ctx)
}
