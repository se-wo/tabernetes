<!--- app-name: Redis(R) -->

# Redis(R) Helm chart

Redis(R) is an open source, advanced key-value store. It is often referred to
as a data structure server since keys can contain strings, hashes, lists, sets
and sorted sets.

[Overview of Redis(R)](https://redis.io/)

## TL;DR

```console
helm install my-release oci://registry-1.docker.io/bitnamicharts/redis
```

## Prerequisites

- Kubernetes 1.23+
- Helm 3.8.0+

## Installing the Chart

To install the chart with the release name `my-release`:

```console
helm install my-release oci://registry-1.docker.io/bitnamicharts/redis
```

The command deploys Redis(R) on the Kubernetes cluster with the default
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
| `nameOverride`      | String to partially override redis.fullname | `""`            |
| `fullnameOverride`  | String to fully override redis.fullname     | `""`            |
| `commonLabels`      | Labels to add to all deployed objects       | `{}`            |
| `commonAnnotations` | Annotations to add to all deployed objects  | `{}`            |

### Redis(R) parameters

| Name                             | Description                                               | Value           |
| -------------------------------- | --------------------------------------------------------- | --------------- |
| `image.registry`                 | Redis(R) image registry                                   | `docker.io`     |
| `image.repository`               | Redis(R) image repository                                 | `library/redis` |
| `image.tag`                      | Redis(R) image tag                                        | `8.2.2`         |
| `image.digest`                   | Redis(R) image digest (overrides the tag)                 | `""`            |
| `image.pullPolicy`               | Redis(R) image pull policy                                | `IfNotPresent`  |
| `image.pullSecrets`              | Image pull secrets                                        | `[]`            |
| `architecture`                   | Redis(R) architecture: only "standalone"                  | `standalone`    |
| `auth.enabled`                   | Enable password authentication                            | `true`          |
| `auth.password`                  | Redis(R) password (empty: a random one, kept on upgrades) | `""`            |
| `auth.existingSecret`            | The name of an existing secret with the password          | `""`            |
| `auth.existingSecretPasswordKey` | The key of the password in auth.existingSecret            | `""`            |

### Redis(R) master parameters

| Name                              | Description                                                                  | Value             |
| --------------------------------- | ---------------------------------------------------------------------------- | ----------------- |
| `master.count`                    | Number of Redis(R) master instances to deploy                                | `1`               |
| `master.containerPorts.redis`     | Redis(R) container port                                                      | `6379`            |
| `master.extraFlags`               | Extra flags for redis-server                                                 | `[]`              |
| `master.extraEnvVars`             | Array with extra environment variables to add to the Redis(R) master pods    | `[]`              |
| `master.podLabels`                | Extra labels for Redis(R) master pods                                        | `{}`              |
| `master.podAnnotations`           | Annotations for Redis(R) master pods                                         | `{}`              |
| `master.nodeSelector`             | Node labels for pod assignment                                               | `{}`              |
| `master.tolerations`              | Tolerations for pod assignment                                               | `[]`              |
| `master.affinity`                 | Affinity for pod assignment                                                  | `{}`              |
| `master.resources`                | Set container requests and limits for different resources like CPU or memory | `{}`              |
| `master.livenessProbe.enabled`    | Enable the liveness probe                                                    | `true`            |
| `master.readinessProbe.enabled`   | Enable the readiness probe                                                   | `true`            |
| `master.persistence.enabled`      | Enable persistence with a PersistentVolumeClaim                              | `false`           |
| `master.persistence.storageClass` | Storage class of the PersistentVolumeClaim                                   | `""`              |
| `master.persistence.accessModes`  | Access modes of the PersistentVolumeClaim                                    | `[ReadWriteOnce]` |
| `master.persistence.size`         | Size of the PersistentVolumeClaim                                            | `8Gi`             |
| `master.service.type`             | Redis(R) master service type                                                 | `ClusterIP`       |
| `master.service.ports.redis`      | Redis(R) master service port                                                 | `6379`            |
| `master.service.nodePorts.redis`  | Node port of the master service (NodePort and LoadBalancer)                  | `""`              |
| `master.service.annotations`      | Master service annotations                                                   | `{}`              |
