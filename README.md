# Unikorn Application Service

![Unikorn Logo](https://raw.githubusercontent.com/unikorn-cloud/assets/main/images/logos/light-on-dark/logo.svg#gh-dark-mode-only)
![Unikorn Logo](https://raw.githubusercontent.com/unikorn-cloud/assets/main/images/logos/dark-on-light/logo.svg#gh-light-mode-only)

## Overview

The application service is a layer that sits on top of the [Kubernetes service](https://github.com/unikorn-cloud/kubernetes).
Its role is to decouple application management from the actual cluster, and provide independent application management services from the applications defined for the Kubernetes service to operate.

The application service lays the foundations for an abstract range of offerings that offer platform-as-a-service (PaaS).
For example, where users don't care about the underlying infrastructure and just want to consume a platform e.g. Jupyter Notebooks.

## Installation

### Prerequisites

To use the Application service you first need to install:

* [The identity service](https://github.com/unikorn-cloud/identity) to provide API authentication and authorization.
* [The Kubernetes service](https://github.com/unikorn-cloud/kubernetes) to provide Kubernetes cluster monitoring.

### Installing the Service

#### Installing Prerequisites

The Unikorn application server component has a couple prerequisites that are required for correct functionality.
If not installing the server component, skip to the next section.

You'll need to install:

* cert-manager (used to generate keying material for ingress TLS)
* nginx-ingress (to perform routing, avoiding CORS, and TLS termination)

#### Installing the Application Service

<details>
<summary>Helm</summary>

Create a `values.yaml` for the server component:
A typical `values.yaml` that uses cert-manager and ACME, and external DNS might look like:

```yaml
global:
  identity:
    host: https://identity.unikorn-cloud.org
  kubernetes:
    host: https://kubernetes.unikorn-cloud.org
  application:
    host: https://application.unikorn-cloud.org
```

```shell
helm install unikorn-application charts/application --namespace unikorn-application --create-namespace --values values.yaml
```

</details>

<details>
<summary>ArgoCD</summary>

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: unikorn
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://unikorn-cloud.github.io/application
    chart: application
    targetRevision: v0.1.0
  destination:
    namespace: unikorn
    server: https://kubernetes.default.svc
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
    - CreateNamespace=true
```

</details>

### Configuring Service Authentication and Authorization

The [Unikorn Identity Service](https://github.com/unikorn-cloud/identity) describes how to configure a service organization, groups and role mappings for services that require them.

This service requires asynchronous access to the Unikorn Region API in order to poll cloud identity and physical network status during cluster creation, and delete those resources on cluster deletion.

This service defines the `unikorn-application` user that will need to be added to a group in the service organization.
