<!--- app-name: Apache -->

# Apache Helm chart

Apache HTTP Server is an open-source HTTP server. The goal of this project is
to provide a secure, efficient and extensible server that provides HTTP
services in sync with the current HTTP standards.

[Overview of Apache](https://httpd.apache.org/)

## TL;DR

```console
helm install my-release oci://registry-1.docker.io/bitnamicharts/apache
```

## Prerequisites

- Kubernetes 1.23+
- Helm 3.8.0+

## Installing the Chart

To install the chart with the release name `my-release`:

```console
helm install my-release oci://registry-1.docker.io/bitnamicharts/apache
```

The command deploys Apache on the Kubernetes cluster with the default
configuration. The [Parameters](#parameters) section lists the parameters that
can be configured during installation.

## Uninstalling the Chart

To uninstall the `my-release` release:

```console
helm uninstall my-release
```

The command removes all the Kubernetes components associated with the chart and
deletes the release.

## Parameters

### Common parameters

| Name                | Description                                  | Value           |
| ------------------- | -------------------------------------------- | --------------- |
| `clusterDomain`     | Kubernetes Cluster Domain                    | `cluster.local` |
| `nameOverride`      | String to partially override apache.fullname | `""`            |
| `fullnameOverride`  | String to fully override apache.fullname     | `""`            |
| `commonLabels`      | Labels to add to all deployed objects        | `{}`            |
| `commonAnnotations` | Annotations to add to all deployed objects   | `{}`            |

### Apache parameters

| Name                     | Description                                                                  | Value           |
| ------------------------ | ---------------------------------------------------------------------------- | --------------- |
| `image.registry`         | Apache image registry                                                        | `docker.io`     |
| `image.repository`       | Apache image repository                                                      | `library/httpd` |
| `image.tag`              | Apache image tag                                                             | `2.4.65`        |
| `image.digest`           | Apache image digest (overrides the tag)                                      | `""`            |
| `image.pullPolicy`       | Apache image pull policy                                                     | `IfNotPresent`  |
| `image.pullSecrets`      | Image pull secrets                                                           | `[]`            |
| `replicaCount`           | Number of Apache replicas to deploy                                          | `1`             |
| `revisionHistoryLimit`   | The number of old history to retain to allow rollback                        | `10`            |
| `updateStrategy.type`    | Apache deployment strategy type                                              | `RollingUpdate` |
| `containerPorts.http`    | Port of the Apache container                                                 | `80`            |
| `extraEnvVars`           | Array with extra environment variables to add to the Apache container        | `[]`            |
| `podLabels`              | Extra labels for Apache pods                                                 | `{}`            |
| `podAnnotations`         | Annotations for Apache pods                                                  | `{}`            |
| `nodeSelector`           | Node labels for pod assignment                                               | `{}`            |
| `tolerations`            | Tolerations for pod assignment                                               | `[]`            |
| `affinity`               | Affinity for pod assignment                                                  | `{}`            |
| `resources`              | Set container requests and limits for different resources like CPU or memory | `{}`            |
| `livenessProbe.enabled`  | Enable the liveness probe                                                    | `true`          |
| `readinessProbe.enabled` | Enable the readiness probe                                                   | `true`          |

### Traffic exposure parameters

| Name                            | Description                                                          | Value          |
| ------------------------------- | -------------------------------------------------------------------- | -------------- |
| `service.type`                  | Service type: LoadBalancer, NodePort or ClusterIP                    | `LoadBalancer` |
| `service.ports.http`            | Service HTTP port                                                    | `80`           |
| `service.nodePorts.http`        | Node port for HTTP (NodePort and LoadBalancer; empty: one is chosen) | `""`           |
| `service.clusterIP`             | Service Cluster IP                                                   | `""`           |
| `service.loadBalancerIP`        | LoadBalancer service IP address                                      | `""`           |
| `service.externalTrafficPolicy` | Service external traffic policy                                      | `Cluster`      |
| `service.annotations`           | Service annotations                                                  | `{}`           |
