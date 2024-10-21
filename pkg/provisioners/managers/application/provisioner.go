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
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
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

// Graph wraps up our graph of applications, and versions, in
// a way that is usable by a simple SAT solver.
type Graph struct {
	// epochs is a time ordered list of points in time where we have made
	// "arbitrary" decisions about a package version to include.
	epochs []*Epoch
	// epoch is the current epoch index.
	epoch int
}

// Epoch is a point in time where we have made a decision about
// a package version to use.  If we get to a point where the graph cannot be
// satisfied this gives us a convenient place to backtrack to an try again with
// a new version.
type Epoch struct {
	// applications defines a set of applications in the graph.
	applications map[string]*unikornv1core.HelmApplication
	// dependencies is a count of dependencies an application has.  Those
	// with zero are considered the roots, thus those to process first.
	dependencies map[string]int
	// children records the inverse relationship of application dependencies.
	// This allows us to walk the graph from root nodes (with no dependencies).
	children map[string][]string
	// constraints are any constraints we have attached to an application.
	// These may either come from the user selecting a specific package version,
	// a dependency, or a "guess" made by the solver when no prior constraints
	// were present.  These need to be checked when adding a node to select a
	// valid version, and when adding new dependency constraints as the node
	// may have already been added to the graph and conflict with what's newly
	// required.
	constraints map[string][]*unikornv1core.SemanticVersionConstraints
	// versions remembers the selected version of an application.
	versions map[string]*unikornv1core.HelmApplicationVersion
}

func NewGraph() *Graph {
	return &Graph{
		epochs: []*Epoch{
			{
				applications: map[string]*unikornv1core.HelmApplication{},
				dependencies: map[string]int{},
				children:     map[string][]string{},
				constraints:  map[string][]*unikornv1core.SemanticVersionConstraints{},
				versions:     map[string]*unikornv1core.HelmApplicationVersion{},
			},
		},
		epoch: 0,
	}
}

// Epoch is a convenience method to access the lastest epoch.
func (g *Graph) Epoch() *Epoch {
	return g.epochs[g.epoch]
}

// Contains checks if the most recent epoch already contains an application.
func (g *Graph) Contains(id string) bool {
	_, ok := g.Epoch().applications[id]
	return ok
}

// checkConstraints takes a semantic version and checks it against a list of
// constraints, returning an error if any are violated.
func checkConstraints(version unikornv1core.SemanticVersion, constraints []*unikornv1core.SemanticVersionConstraints) error {
	for _, constraint := range constraints {
		if !constraint.Check(&version) {
			return fmt.Errorf("%w: version %v does not match constraint %s", ErrConstraint, version.Version, constraint.String())
		}
	}

	return nil
}

type ConstraintsErrors []error

func (e ConstraintsErrors) Error() string {
	s := make([]string, len(e))

	for i, t := range e {
		s[i] = t.Error()
	}

	return strings.Join(s, ", ")
}

// cmpHelmApplicationVersion provides a compare function to compare Helm application
// versions based on their semantic versions.
func cmpHelmApplicationVersion(a, b *unikornv1core.HelmApplicationVersion) int {
	return a.Version.Compare(&b.Version)
}

func (g *Graph) Add(a *unikornv1core.HelmApplication) ([]string, error) {
	if g.Contains(a.Name) {
		return nil, fmt.Errorf("%w: already encountered %s", ErrGraph, a.Name)
	}

	var version *unikornv1core.HelmApplicationVersion

	constraints := g.Epoch().constraints[a.Name]

	var errors ConstraintsErrors

	for _, v := range slices.Backward(slices.SortedFunc(a.Versions(), cmpHelmApplicationVersion)) {
		if err := checkConstraints(v.Version, constraints); err != nil {
			errors = append(errors, err)
			continue
		}

		version = v

		break
	}

	if version == nil {
		return nil, errors
	}

	// For every dependency, add a reverse relationship.
	dependencies := make([]string, len(version.Dependencies))

	for i, dependency := range version.Dependencies {
		dependencies[i] = dependency.Name

		if _, ok := g.Epoch().children[dependency.Name]; !ok {
			g.Epoch().children[dependency.Name] = []string{}
		}

		g.Epoch().children[dependency.Name] = append(g.Epoch().children[dependency.Name], a.Name)
	}

	g.Epoch().applications[a.Name] = a
	g.Epoch().versions[a.Name] = version
	g.Epoch().dependencies[a.Name] = len(version.Dependencies)

	return dependencies, nil
}

func (g *Graph) AddConstraints(id string, constraints *unikornv1core.SemanticVersionConstraints) {
	if constraints == nil {
		return
	}

	if _, ok := g.Epoch().constraints[id]; !ok {
		g.Epoch().constraints[id] = []*unikornv1core.SemanticVersionConstraints{constraints}

		return
	}

	g.Epoch().constraints[id] = append(g.Epoch().constraints[id], constraints)
}

func (g *Graph) All() iter.Seq2[*unikornv1core.HelmApplication, *unikornv1core.HelmApplicationVersion] {
	return func(yield func(*unikornv1core.HelmApplication, *unikornv1core.HelmApplicationVersion) bool) {
		roots := maps.Clone(g.Epoch().dependencies)
		maps.DeleteFunc(roots, func(k string, v int) bool { return v != 0 })

		queue := slices.Collect(maps.Keys(roots))
		seen := map[string]interface{}{}

		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]

			if _, ok := seen[id]; ok {
				continue
			}

			seen[id] = nil

			if !yield(g.Epoch().applications[id], g.Epoch().versions[id]) {
				return
			}

			queue = append(queue, g.Epoch().children[id]...)
		}
	}
}

// SolveApplicationSet walks the graph Dykstra style loading in referenced dependencies.
// Where things get interesting is if a dependency is pulled in multiple times
// but the originally selected version conflicts with other dependency constraints
// then we have a conflict, and have to backtrack and try again with another version.
// Unlike typical SAT solver problems, choosing a different version can have the fun
// effect of changing its dependencies!
func SolveApplicationSet(ctx context.Context, client client.Client, namespace string, applicationset *unikornv1.ApplicationSet) (*Graph, error) {
	applications, err := getApplications(ctx, client, namespace)
	if err != nil {
		return nil, err
	}

	queue := make([]string, 0, len(applicationset.Spec.Applications))

	g := NewGraph()

	// Populate the work queue with any application IDs that are requested by the
	// user, we will use them as starting points to populate the rest of the dependencies.
	// If the user specified a specific version of an application, then add that as a
	// constraint.
	for _, ref := range applicationset.Spec.Applications {
		queue = append(queue, ref.Application.Name)

		if ref.Version != nil {
			constraints, err := semver.NewConstraint("=" + ref.Version.String())
			if err != nil {
				return nil, err
			}

			g.AddConstraints(ref.Application.Name, &unikornv1core.SemanticVersionConstraints{
				Constraints: *constraints,
			})
		}
	}

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		if g.Contains(id) {
			continue
		}

		application, ok := applications[id]
		if !ok {
			return nil, fmt.Errorf("%w: unable to locate application %s", ErrResourceDependency, id)
		}

		dependencies, err := g.Add(application)
		if err != nil {
			return nil, err
		}

		queue = append(queue, dependencies...)
	}

	return g, nil
}

func GenerateProvisioner(ctx context.Context, client client.Client, namespace string, applicationset *unikornv1.ApplicationSet) (provisioners.Provisioner, error) {
	g, err := SolveApplicationSet(ctx, client, namespace, applicationset)
	if err != nil {
		return nil, err
	}

	for a := range g.All() {
		fmt.Println(a)
	}

	/*
		// Our root nodes for scheduling are those with no dependencies...
		roots := maps.Clone(dependencies)
		maps.DeleteFunc(roots, func(k string, v int) bool { return v != 0 })

		// Back to Dykstra again, we're going to simply walk the graph from the roots
		// to the leaves, remembering the shortest depth.  We can then process all nodes
		// at the same depth in parallel, and join those up in series.
		// NOTE: this is sub-optimal, but good enough for an MVP.
		queue = slices.Collect(maps.Keys(roots))

		seen := map[string]interface{}{}
		depths := map[string]int{}

		largestDepth := 0;

		for root := range roots {
			depths[root] = 0
		}

		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]

			// Node visited already...
			if _, ok := seen[id]; ok {
				continue
			}

			seen[id] = true

			for _, child := range children[id] {
				depth := depths[id] + 1
				largestDepth = max(largestDepth, depth)

				if _, ok := depths[child]; !ok {
					depths[child] = depth
				}

				queue = append(queue, child)
			}
		}

		p := make([]provisioners.Provisioner, largestDepth+1)

		for i := range largestDepth + 1 {
			ids := []string{}

			for id, depth := range depths {
				if depth != i {
					continue
				}

				ids = append(ids, id)
			}

			apps := make([]provisioners.Provisioner, 0, len(ids))

			for _, id := range ids {
				ref := &unikornv1core.ApplicationReference{
					Name: &applications[id].Name,
					Version: versions[id],
				}

				getter := func(_ context.Context) (*unikornv1core.ApplicationReference, error) {
					return ref, nil
				}

				apps = append(apps, application.New(getter))
			}

			p[i] = concurrent.New(fmt.Sprintf("concurrent%d", i), apps...)
		}

		return serial.New("serial0", p...), nil
	*/

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
