# Antrea core on OpenShift

## Bootstrap and artifact inputs

Use the release installer to create the cluster configuration with
`networking.networkType: antrea`, the Pod CIDR and host prefix, and the
service CIDR. Create the installer manifests, then add the operator CRD,
namespace, service account, RBAC, Deployment and `AntreaInstall` from the
OpenShift deployment archive before creating ignition configuration.
Include the bootstrap SCC and credential RBAC in
[`deploy/openshift/core_scc.yaml`](../deploy/openshift/core_scc.yaml).
Create any external IPsec credential in the selected core namespace by
the time the operator reads it.

The operator uses host networking, the OpenShift `hostnetwork` SCC and
leader election. The agent and controller use host networking and the
`privileged` SCC granted to their named service accounts. An alternate
core namespace needs Pod Security admission compatible with those
workloads as well as the role bindings. The shipped agent DaemonSet runs
on eligible Linux nodes, including tainted control-plane nodes. The
controller can also schedule on the control plane during bootstrap.

Use `AntreaInstall/antrea-install` in `antrea-operator` with
`spec.antreaPlatform: openshift`. The operator waits for
`Network.config.openshift.io/cluster` and
`Network.operator.openshift.io/cluster` to select Antrea. It derives the
agent Service CIDR, the controller NodeIPAM CIDRs and node masks, and
the status of the applied OpenShift network from the cluster Network.
It uses the OpenShift CNO paths for CNI configuration and binaries.
`DisableMultiNetwork` on the operator Network selects between the
Multus and direct system CNI configuration paths. An omitted agent
`defaultMTU` is set to 1450. An explicit zero retains the agent's
automatic MTU selection.

| Output | Baseline artifact contract | Release input |
| --- | --- | --- |
| Agent, OVS and CNI installer | A04 uses the agent UBI image for all three | Set `spec.antreaAgentImage` to the release agent reference. |
| Controller | A04 produces a separate controller UBI image | Set `spec.antreaControllerImage` to the release controller reference. |
| IPsec agent, OVS, CNI installer and sidecar | A05 uses the IPsec UBI agent image for all four. The image provides `start_ovs_ipsec`, `container_liveness_probe`, OVS IPsec and strongSwan. | Select the A05 agent image in `spec.antreaAgentImage`. |
| IPsec controller | A05 produces a separate IPsec controller UBI image | Select the A05 controller image in `spec.antreaControllerImage`. |

The common legacy `spec.antreaImage` fills any omitted image for the
shared-image layout. `ResourceBundle.Configure` can supply
`AntreaAgentImage` and `AntreaControllerImage` to a distribution template.
In IPsec mode the operator requires a usable reference for both operands
and supplies no default image. Use the A04 or A05 candidate digests for
qualification. The commercial 2.7 build definitions establish the
component and executable layout. A04, A05 and A22 still need to supply
the final 2.8 artifact digests and verify their contents and startup on
stock RHCOS.

## Network and encryption configuration

The OpenShift bootstrap profile uses CNO kube-proxy for host and
NodePort traffic. Leave `spec.deployKubeProxy` enabled or unset in the
operator Network, and leave `antreaProxy.proxyAll` disabled in the agent
config. Keep AntreaProxy enabled for `noEncap` and `hybrid`. Supply
underlay routes for peer Pod CIDRs where either mode sends traffic
without a tunnel. Select `encap` with Geneve for the unencrypted test
baseline. Use fresh OpenShift clusters to qualify different routing
modes and Multus settings.

Set `spec.trafficEncryptionMode` or `trafficEncryptionMode` in
`spec.antreaAgentConfig` to `none`, `ipsec` or `wireGuard`.
The top-level option takes precedence over an omitted or `none` agent
value, as in the legacy install. A conflicting explicit protected
value in the agent configuration blocks apply. To switch intentionally
away from IPsec, set `none` and remove or update any protected value in
the embedded agent configuration. Clear `spec.ipsecPSKSecretName` in the
same change when using a named credential. Supply the correct A04
images when returning to the baseline.

| Profile | Configuration |
| --- | --- |
| IPsec PSK | Use `trafficEncapMode: encap`, the selected tunnel, the two A05 images and `ipsec.authenticationMode: psk` (the default). Set `spec.ipsecPSKSecretName` to an Opaque Secret in the core namespace with a nonblank `psk` value. With neither credential field set the operator uses an existing `antrea-ipsec` Secret in that namespace. |
| Legacy inline IPsec PSK | Preserve an existing `spec.psk` value without adding a Secret reference. The operator copies it to the owned `antrea-ipsec-operator-psk` Secret. `spec.psk` and `spec.ipsecPSKSecretName` are mutually exclusive in PSK mode. |
| IPsec certificate | Set `ipsec.authenticationMode: cert` in the agent config and `featureGates.IPsecCertAuth: true` in the agent and controller configs. Use the controller's self-signed CA and automatic CSR approval by keeping `ipsecCSRSigner.selfSignedCA` and `ipsecCSRSigner.autoApprove` enabled. The controller owns the CA material. This profile needs its own A15 result alongside required PSK coverage. |
| WireGuard qualification | Use `trafficEncapMode: encap`, `trafficEncryptionMode: wireGuard` and the A04 images on stock RHCOS. Confirm host support, device and route creation, fresh peer handshakes, packet delivery and underlay encryption. The release still needs the A21 support decision. |

If qualification excludes WireGuard, set
`CoreOptions.DisableOpenShiftWireGuard` in the distribution or pass
`--disable-openshift-wireguard` to the standalone binary. A new or
pre-existing WireGuard request then reports a degraded configuration
before applying operands. The requested fields and last applied
configuration remain intact. Recover by restoring the qualified
distribution or selecting a supported protected configuration.

## Rollout, ownership and recovery

- `AntreaInstall.status.coreAppliedGeneration` records accepted
  OpenShift applies. Check it against `metadata.generation` separately
  from the core `Available`, `Progressing` and `Degraded` conditions in
  the install and ClusterOperator `antrea`. Confirm the DaemonSet and
  Deployment observed generations, updated replicas and ready agents.
  A valid WireGuard request can be applied while its agent fails readiness
  due to host support.
- A rendered core ConfigMap change updates the Pod-template revision.
  In IPsec PSK mode the agent reads the key from its Secret. Credential
  rotation rolls the agent after the operator observes the new Secret
  revision during reconciliation. On an invalid request, the operator
  publishes degraded status while retaining the previous applied
  generation and operands, or starts no new operands on a fresh install.
- For C5 and I3, replace one agent Pod and wait for its replacement to
  become ready before restarting the operator Pod separately. Record both
  generations and UIDs, and observe traffic and protection throughout.
  During an intentional transition between profiles a rolling update can
  temporarily interrupt cross-node traffic. Use the applied generation
  and workload rollout state to identify the convergence window.
- For I3, explicitly apply the baseline mode and images. Confirm the
  IPsec sidecar disappears and the remaining agent removes the obsolete
  OVS tunnel ports. Check that the IKE and child associations for those
  peers are removed after shutdown or expiry. This cleanup needs
  datapath evidence on the selected image pair.
- Core prunes the marked, reserved managed PSK Secret after applying a
  configuration that no longer uses it. An external credential,
  controller-managed CA, ACNP data and the preprovisioned namespace
  have separate owners. Keep the old unmarked legacy Secret available
  until its lifecycle owner confirms the handover.

## Namespace and composition handoff

N01 selects the deployment namespace and coordinates the handover. In
the current build the default is `kube-system`. Select a provisioned
alternate namespace through `CoreOptions.OpenShiftNamespace` or
`--openshift-core-namespace`. When set, `spec.coreNamespace` must match.
Render the core Role and RoleBinding namespaces and the agent and
controller subjects with that selection. Keep the operator subject in
`antrea-operator`.
The reconciler blocks apply when another namespace still has a core
DaemonSet, Deployment or labelled core Pod. The operator namespace and
Interworking namespace have distinct roles. The controller publishes
`antrea-cluster-identity` in its own Pod namespace, so A11/A22 need a Flow
Aggregator that resolves that same location before enabling a move.

Use `{{.AntreaNamespace}}` in distribution templates for all core
namespace references. Provide exact active and retired identities in
`ResourceBundle.OwnedResources`. Include `antrea-agent`,
`antrea-controller` and `antrea-config` in that namespace and declare the
reserved `antrea-ipsec-operator-psk` identity if the distribution retains
inline PSK. Supply the shared OVS run and log mounts in the agent
template. Core adds the IPsec sidecar, credential environment and
certificate mount. A distribution's Flow Exporter changes may extend the
agent config while preserving the resolved encryption settings. Keep
Flow Aggregator identities and status under the U04 ownership contract.
[Module composition](module-composition.md)

Before generating the release archive, carry the core API fields through
the compatible commercial CRD, including `status.coreAppliedGeneration`.
A18 should record the actual bootstrap manifests, selected images and
digests, installer version, stock RHCOS and kernel identities, and
namespace and SCC choices. A15 can use the profiles above to bind its
core, IPsec, WireGuard and recovery assertions after N01 selects the
namespace.
