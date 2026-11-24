# Composing the Antrea operator

A distribution runs the core and its own controller on one controller-runtime
manager with one leader-election configuration. The standalone entry point
passes zero-valued options. The historical commercial install API is
`operator.antrea.vmware.com/v1, Kind=AntreaInstall`. Both binaries read
`antrea-operator/antrea-install` at startup to select the platform.

## Registration and configuration

`controllers.RegisterCore(ctx context.Context, mgr ctrl.Manager, opts controllers.CoreOptions) error`
registers the install and workload controllers. Configure the Kubernetes,
OpenShift config/v1 and OpenShift operator/v1 schemes on the manager, register
core, register the distribution controller with its own name, and start the
manager once. The distribution installs its compatible CRD and RBAC before
startup. If it uses a typed install API, register the full distribution type
once for that GVK. Core reads
and watches an unstructured object at the selected GVK. It projects the
shared spec into a detached configuration and writes installation health
through the status subresource.

| Input | Contract |
| --- | --- |
| `CoreOptions.InstallGVK` | A zero value selects the standalone GVK. A distribution can select another group/version serving the same namespaced kind and shared field formats. |
| `CoreOptions.Version` | Release value supplied to templates and reported in the OpenShift ClusterOperator's operator version. The default comes from the upstream build. |
| `CoreOptions.Bundle` | A nil bundle selects the shipped templates. A custom `*controllers.ResourceBundle` provides a manifest directory and its core resource identities. |
| `ResourceBundle.ManifestDir` | CNO renders the generated directory. Include core manifests, the required agent and controller, and distribution-specific core CRDs/RBAC here. Keep the install CRD in the distribution's install archive. |
| `ResourceBundle.OwnedResources` | Exact OpenShift `configv1.ObjectReference` values with API group, plural resource, namespace and name. Include every active identity and retired identities still eligible for cleanup. Registration copies the list. Generate it from the core resource contract independently of optional enablement flags. |
| `ResourceBundle.Configure` | Optional `func(*unstructured.Unstructured, map[string]interface{}) error`. Read the full install and supply distribution template values after core defaults and validation. Return errors suitable for installation status. This callback may run more than once for the same input. |

For example, the commercial generator can render distinct agent and controller
image keys. Its callback reads `spec.antreaAgentImage` and
`spec.antreaControllerImage` and selects its image defaults. A generation
change rerenders a custom bundle even when the projected core spec is
identical. Core adds a revision derived from rendered core ConfigMap names,
data and binary data to both core Pod templates. A change in a distribution
ConfigMap rolls the workloads. Other inputs that require a restart, such as a
separately managed credential, need a Pod-template change from their owner.
The external-package test registers a second controller, reconciles custom
image and ConfigMap inputs, and observes the server-side apply requests.
[Executable consumer](../controllers/register_test.go)

## Ownership and health

The generator assigns each core identity exclusively to the core controller.
Remove a previous reconciler's ability to write it before handover. CNO uses
forced server-side apply. The composed bundle and workload recovery use the
field manager `cluster-network-operator/antrea-core`. A distinct field manager
records provenance but cannot arbitrate overlapping desired fields during
forced apply. The standalone path retains its existing field manager.

The core validates the whole rendered inventory before the first apply and
marks desired objects with `operator.antrea.io/operand=core`. On OpenShift,
after all applies succeed, it publishes the new core inventory alongside
foreign and unclassified references already in the ClusterOperator. To
prune a retired object the composed core requires a declared identity and a
matching live marker. Deletion carries the observed UID and resource version.
The core also checks declared inactive identities for marked objects left by
an interrupted apply. Namespaces and unmarked legacy objects survive.
Kubernetes mode retains its install-status-only behavior.

The 1.11 ClusterOperator inventory is shared. During migration the commercial
owner must partition its identities, preserve user-managed resources such as
`nsx-cert`, and take responsibility for retired unmarked objects. For an
identity moving between controllers, remove it from the old owner's desired
and cleanup sets before the new owner claims it. A list entry or an existing
Network owner reference alone grants no cleanup authority. Replacements
under the same name require a new ownership decision.

Core reports Available, Progressing, Degraded and Upgradeable for the core
operands. It mirrors them to the install on OpenShift. A distribution
reconciler calls
`install.ReportOperandCondition(ctx, client, gvk, condition) error` with a
distinct type such as `InterworkingAvailable`. That report belongs to
`AntreaInstall.status.conditions`. Both writers merge by type, retain
unrelated fields, and retry resource-version conflicts. Each type needs one
writer. Consumers can read core availability and Interworking readiness
separately. [Condition merge regression](../controllers/install/install_test.go)
· [Inventory regression](../controllers/statusmanager/inventory_test.go)

## Integration handoff

- **U03 and N01.** The current core identities are DaemonSet
  `kube-system/antrea-agent` and Deployment `kube-system/antrea-controller`.
  U03 and N01 must agree on the replacement namespace source for render,
  workload identification, recovery, and handover before enabling a move.
  U03 owns generic core and IPsec configuration and validation.
- **M02 and M04.** M02 owns the compatible full install CRD, distribution
  defaults, Interworking resources and conditions, and migration of the
  legacy shared inventory. M04 exercises the actual composed binary,
  interrupted legacy handovers and recovery. U03/M02 signature and
  ownership signoff and integrated cluster validation are pending.
- **M03.** Build against the reviewed upstream commit. For a local review
  use a temporary consumer with a local module replacement. After upstream
  acceptance resolve the immutable commit, record its Go pseudo-version or
  tag and module checksum, and compile the actual downstream dependency
  graph. [Module maintenance](../CONTRIBUTING.md#maintaining-the-go-module)
