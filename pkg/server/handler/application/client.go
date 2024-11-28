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
	"slices"

	"github.com/unikorn-cloud/application/pkg/openapi"
	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"
	coreopenapi "github.com/unikorn-cloud/core/pkg/openapi"
	"github.com/unikorn-cloud/core/pkg/server/conversion"
	"github.com/unikorn-cloud/core/pkg/server/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Client wraps up application bundle related management handling.
type Client struct {
	// client allows Kubernetes API access.
	client client.Client
	// namespace is the namespace we are running in to scope application access.
	namespace string
}

// NewClient returns a new client with required parameters.
func NewClient(client client.Client, namespace string) *Client {
	return &Client{
		client:    client,
		namespace: namespace,
	}
}

func convert(in *unikornv1core.HelmApplication) *openapi.ApplicationRead {
	versions := make(openapi.ApplicationVersions, 0, len(in.Spec.Versions))

	for _, version := range in.Spec.Versions {
		v := openapi.ApplicationVersion{
			Version: version.Version.Original(),
		}

		if len(version.Dependencies) != 0 {
			deps := make(openapi.ApplicationDependencies, 0, len(version.Dependencies))

			for _, dependency := range version.Dependencies {
				deps = append(deps, openapi.ApplicationDependency{
					Name: dependency.Name,
				})
			}

			v.Dependencies = &deps
		}

		if len(version.Recommends) != 0 {
			recommends := make(openapi.ApplicationRecommends, 0, len(version.Recommends))

			for _, recommend := range version.Recommends {
				recommends = append(recommends, openapi.ApplicationDependency{
					Name: recommend.Name,
				})
			}

			v.Recommends = &recommends
		}

		versions = append(versions, v)
	}

	out := &openapi.ApplicationRead{
		Metadata: conversion.ResourceReadMetadata(in, in.Spec.Tags, coreopenapi.ResourceProvisioningStatusProvisioned),
		Spec: openapi.ApplicationSpec{
			Documentation: *in.Spec.Documentation,
			License:       *in.Spec.License,
			Icon:          in.Spec.Icon,
			Versions:      versions,
		},
	}

	return out
}

func convertList(in []unikornv1core.HelmApplication) []*openapi.ApplicationRead {
	out := make([]*openapi.ApplicationRead, len(in))

	for i := range in {
		out[i] = convert(&in[i])
	}

	return out
}

func (c *Client) List(ctx context.Context) ([]*openapi.ApplicationRead, error) {
	options := &client.ListOptions{
		Namespace: c.namespace,
	}

	result := &unikornv1core.HelmApplicationList{}

	if err := c.client.List(ctx, result, options); err != nil {
		return nil, errors.OAuth2ServerError("failed to list applications").WithError(err)
	}

	slices.SortStableFunc(result.Items, unikornv1core.CompareHelmApplication)

	return convertList(result.Items), nil
}
