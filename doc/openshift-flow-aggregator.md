# Flow visibility on OpenShift

Set `spec.flowAggregator` on the shared namespaced `AntreaInstall`. The
upstream and historical commercial GVK is `operator.antrea.vmware.com/v1`.
M03 must ship this additive schema in the commercial CRD. Omitting the
field preserves the existing installation's agent configuration. A minimal
enabled specification, with a distribution-supplied image, is shown below.

```yaml
spec:
  flowAggregator:
    enabled: true
    mode: Aggregate
    collectorAddress: collector.monitoring.svc:4739
    podLabels: false
```

`Aggregate` is the default mode. `Proxy` sends individual agent observations.
Both export IPFIX over TCP to the specified IPv4 or DNS host and port. The
receiver must accept plaintext TCP from the aggregator and decode Antrea's
enterprise information elements. This profile uses Kubernetes names. The
`podLabels` field controls enrichment in both modes. The consumer may set
`FlowAggregatorImage` in `ResourceBundle.Configure`. An explicit
`spec.flowAggregator.image` takes precedence. Enablement requires an image.

## Agent, credentials and ownership

The operator enables the agent's `featureGates.FlowExporter` in the rendered
config. The managed cluster-scoped `FlowExporterDestination/operator-flow-aggregator`
references `<core namespace>/flow-aggregator:14739`, the Service DNS name
for server authentication, and CA/client credentials in that namespace.
The agent uses gRPC with mutual TLS. Its static export configuration and
any other dynamic destinations retain their values. Setting `enabled: false`
or removing the flow setting restores the requested raw agent configuration
and removes only the managed destination and its owned resources. Other
independently configured export paths can continue sending records.

The OpenShift namespace is selected once at startup by U03. Use
`flow.OwnedResources(namespace)` from
`antrea.io/antrea-operator/controllers/flow` when building the custom
`ResourceBundle.OwnedResources`. The binary embeds the flow templates.
Include the 2.7-compatible `flowexporterdestinations.crd.antrea.io` CRD
and `antrea-cluster-identity-reader` ClusterRole in the generated core
bundle. The standalone OpenShift renderer supplies the baseline CRD if
absent and preserves an existing schema. The shared CRD remains installed when the aggregator is disabled,
including any user-created destinations. The operator retains its core field
manager. Core and flow markers, live resource version, controller UID and
an atomic create protect occupied identities on enable. Cleanup of retired
flow objects requires the declared identity, both live ownership labels and
the current Network controller UID.
Deletes carry the observed UID and resource version.

| Scope | Managed identities while enabled |
| --- | --- |
| Core namespace | ServiceAccount, Service and Deployment `flow-aggregator`. ConfigMaps `flow-aggregator-config` and `flow-aggregator-ca`. Secrets `flow-aggregator-ca-tls` and `flow-aggregator-client-tls`. Roles and RoleBindings `flow-aggregator-certificates` and `flow-aggregator-agent`. RoleBinding `flow-aggregator-cluster-identity`. |
| kube-system | RoleBinding `flow-aggregator-authentication-reader` to the existing API server authentication reader. |
| Cluster | ClusterRoles and ClusterRoleBindings `flow-aggregator` and `flow-aggregator-agent`. ClusterRoleBinding `flow-aggregator-auth-delegator`. FlowExporterDestination `operator-flow-aggregator`. |

The operator creates credential objects with owner metadata and no data.
The provider writes the CA ConfigMap and the two Secret key pairs under
`tls.crt` and `tls.key`, preserving object metadata. It watches and updates
those named objects. Its Role omits create privileges. The agent can read the
client key and CA certificate. Reconciliation preserves certificate data
during rollout. The receiver is externally managed. For N01, hand over or
remove an older writer and resolve same-name resources before enabling. A
pre-existing unmarked credential is a collision. The selected namespace and
the ClusterOperator inventory alone cannot authorize its takeover.

## Rollout and status

Changing mode, labels or receiver changes the aggregator Pod template. One
replica uses a Recreate strategy so Aggregate mode has a single active
correlator. The config is mounted at
`/etc/flow-aggregator/flow-aggregator.conf`. A TCP readiness probe checks
the gRPC listener on 14739. The Deployment uses the image entry point,
stderr logging, the assigned non-root UID, and a writable emptyDir at
`/var/run/antrea/apiserver` for the loopback token. A Pod loss triggers
Deployment replacement. The install reconciler also repairs missing
dedicated resources on its periodic reconciliation.

Core config changes roll the agent and controller under the U03/M01
revision contract. A change limited to aggregator configuration rolls the
aggregator. Changes to the agent gate may also activate existing independent
dynamic destinations. `AntreaInstall.status.conditions` includes
`FlowAggregatorAvailable`, with the reconciled install generation in the
message. Compare `status.coreAppliedGeneration` with the install generation.
`False/Progressing` observes rollout or termination,
`True/Available` requires the agent DaemonSet and one updated available
aggregator, and `False/Disabled` follows managed cleanup and agent
convergence. Invalid configuration, resource collisions and failed writes
surface in the core Degraded condition. Check the generation and core
Degraded condition together. Core readiness and the listener probe can be
healthy while an external receiver is unreachable.

## Qualification handoffs

- **A11 and A22.** Supply the immutable 2.8 Linux amd64 UBI image and
  exercise the entry point, restricted SCC, writable loopback token,
  provider certificate updates, listener, SAN and receiver on OpenShift.
  Verify the agent supports the dynamic destination CRD. The pinned 2.7
  startup function `pkg/flowaggregator/cluster_uuid.go:GetClusterUUID`
  reads only `kube-system/antrea-cluster-identity` and exits after a failed
  lookup. The controller allocates cluster identity in its Pod namespace.
  A relocated core requires a binary that reads the selected namespace,
  before it can pass readiness or deliver records. The operator binds the
  reader in the selected core namespace. It leaves cluster identity with
  its allocating controller. The default core namespace matches the 2.7
  behavior.
- **A23 and A19.** Use an independently queryable Antrea-aware IPFIX TCP
  collector in both modes. F3 can toggle `podLabels`, F4 can change
  `collectorAddress`, and F6 can change mode in place. Compare
  `clusterId` with the controller-allocated identity. For Proxy, baseline
  agents derive the original observation domain using FNV-1 (32 bit) over
  the node name. The original exporter address is the observed gRPC peer
  IPv4 address. Capture actual node, routing and NAT evidence for this
  mapping. For Aggregate, check the correlated timestamps and counters
  from both nodes. Default dynamic active/idle intervals here are 5s/15s.
  The 2.7 aggregate active/inactive defaults are 60s/90s. Record the
  effective agent poll interval for each run. Recreate and Pod loss can
  drop buffered records. Verify fresh flows after reconnection.
- **A18 and A21.** Freeze receiver versions and ingestion delay, image
  digests and measured scheduling/clock margins. As initial test budgets,
  allow 10 minutes for workload convergence, use a 3-minute stream for
  three aggregate active periods, and poll for 180s after close. A 180s
  initial drain and a further 180s quiet window assume a healthy receiver
  with delivery/backlog within that bound. Measure these on the candidate,
  including rollout outage. They are proposed test deadlines. No outage
  replay or production delivery latency guarantee has been measured.
- **M03 and N01.** Package the compatible CRD, exact resource declarations,
  operator RBAC and image default. Verify the effective permissions for
  binding the built-in authentication roles. Confirm ownership handover,
  namespace selection and final inventory against a 1.11 installation.
  The operator does not own the receiver, its lifecycle, other projects,
  unrelated Secrets, controller identity ConfigMap or Service-generated
  EndpointSlices. Live record delivery and commercial archive validation
  remain release checks.

Both rendered configurations passed the actual baseline `options.LoadConfig` parser in a local offline check. It selected TCP/IPFIX and the expected 60s/90s aggregate timeouts.

Baseline source is Antrea commercial commit
`592a14c1f29265b1ba37e369c076e6b16e42317b`, specifically
`build/charts/flow-aggregator`, `pkg/agent/flowexporter/exporter.go`,
`pkg/flowaggregator/certificate/provider.go`,
`pkg/flowaggregator/collector/grpc.go`, and
`test/e2e/flowaggregator_test.go`.
