# Shadow Service DNS Controller

**Enable User Defined Network (UDN) connectivity for Operator-managed workloads without fighting the Operator.**

## Simple Overview
The **Shadow Service DNS Controller** is a Kubernetes operator designed to expose Pods on User Defined Networks (UDNs) via standard DNS discovery. It allows you to create a "Shadow" Service that mirrors an existing Service but resolves to the Pods' UDN IPs (secondary or primary non-default networks). This is critical for clustered applications like Keycloak, PostgreSQL, or Hazelcast that need to form clusters over a high-performance or isolated UDN network while still being managed by their standard Operators.

## Description
In modern Kubernetes environments (especially OpenShift with OVN-Kubernetes), workloads often attach to multiple networks. However, standard Kubernetes Services and Headless Services only discover Pods on the Cluster Default Network (CDN).

This creates a conflict for stateful workloads managed by Operators (e.g., the Keycloak Operator):
1.  The Operator manages the Headless Service and reverts any manual changes.
2.  The application tries to cluster using the Default Network IPs, bypassing your high-speed UDN.

**The Solution:**
Instead of fighting the Operator, this controller lets you create a **Shadow Service**—a new Headless Service that you control. By adding a simple annotation referencing the original target Service, this controller:
1.  **Watches** the target Service and its Pods.
2.  **Extracts** the UDN IPs from the Pods' Network Status annotations (Multus/CNI).
3.  **Syncs** these IPs into the Shadow Service's `EndpointSlice`.

This provides a stable DNS entry (e.g., `keycloak-shadow.my-ns.svc`) that resolves exclusively to UDN IPs, allowing you to configure your application's discovery mechanism (like JGroups) to use the UDN without modifying the Operator's resources.

## Getting Started

### Tools
- go version v1.24.0
- podman version 5.3.2.
- oc version v4.19.12.
- Access to a OpenShift local (crc) cluster.

### To Deploy on the cluster
In my case `IMG=quay.io/rh-ee-apalma/shadow-svc-dns-controller` and to configure the environment variables for the operator-sdk based scripts: 

```bash
export IMG=quay.io/rh-ee-apalma/shadow-svc-dns-controller
export KUBECTL=oc
export CONTAINER_TOOL=podman
```

**Build and push your image to the location specified by `IMG` (No need of IMG if you have export the variable):**
```sh
make docker-build docker-push IMG=<some-registry>/operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
oc apply -f https://raw.githubusercontent.com/bizcochillo/shadow-svc-dns-controller/refs/heads/main/dist/install.yaml
```

## Contributing
// Any feedback, appreciated. 

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.