<!--- app-name: NGINX -->

# NGINX Helm chart

NGINX Open Source is a web server that can be also used as a reverse proxy,
load balancer, and HTTP cache. Recommended for high-demanding sites due to its
ability to provide faster content.

[Overview of NGINX](https://nginx.org/)

## TL;DR

```console
helm install my-release oci://registry-1.docker.io/bitnamicharts/nginx
```

## Prerequisites

- Kubernetes 1.23+
- Helm 3.8.0+

## Installing the Chart

To install the chart with the release name `my-release`:

```console
helm install my-release oci://registry-1.docker.io/bitnamicharts/nginx
```

The command deploys NGINX on the Kubernetes cluster with the default
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

| Name                | Description                                 | Value           |
| ------------------- | ------------------------------------------- | --------------- |
| `clusterDomain`     | Kubernetes Cluster Domain                   | `cluster.local` |
| `nameOverride`      | String to partially override nginx.fullname | `""`            |
| `fullnameOverride`  | String to fully override nginx.fullname     | `""`            |
| `commonLabels`      | Labels to add to all deployed objects       | `{}`            |
| `commonAnnotations` | Annotations to add to all deployed objects  | `{}`            |

### NGINX parameters

| Name                     | Description                                                                       | Value           |
| ------------------------ | --------------------------------------------------------------------------------- | --------------- |
| `image.registry`         | NGINX image registry                                                              | `docker.io`     |
| `image.repository`       | NGINX image repository                                                            | `library/nginx` |
| `image.tag`              | NGINX image tag                                                                   | `1.29.3`        |
| `image.digest`           | NGINX image digest (overrides the tag)                                            | `""`            |
| `image.pullPolicy`       | NGINX image pull policy                                                           | `IfNotPresent`  |
| `image.pullSecrets`      | Image pull secrets                                                                | `[]`            |
| `replicaCount`           | Number of NGINX replicas to deploy                                                | `1`             |
| `revisionHistoryLimit`   | The number of old history to retain to allow rollback                             | `10`            |
| `updateStrategy.type`    | NGINX deployment strategy type                                                    | `RollingUpdate` |
| `containerPorts.http`    | Port of the NGINX container                                                       | `80`            |
| `extraEnvVars`           | Array with extra environment variables to add to the NGINX container              | `[]`            |
| `podLabels`              | Extra labels for NGINX pods                                                       | `{}`            |
| `podAnnotations`         | Annotations for NGINX pods                                                        | `{}`            |
| `nodeSelector`           | Node labels for pod assignment                                                    | `{}`            |
| `tolerations`            | Tolerations for pod assignment                                                    | `[]`            |
| `affinity`               | Affinity for pod assignment                                                       | `{}`            |
| `resources`              | Set container requests and limits for different resources like CPU or memory      | `{}`            |
| `livenessProbe.enabled`  | Enable the liveness probe                                                         | `true`          |
| `readinessProbe.enabled` | Enable the readiness probe                                                        | `true`          |
| `serverBlock`            | Custom server block to be added to NGINX configuration (replaces the default one) | `""`            |

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
