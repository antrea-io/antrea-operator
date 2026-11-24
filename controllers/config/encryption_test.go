// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"

	ocoperv1 "github.com/openshift/api/operator/v1"
	"k8s.io/utils/ptr"

	"antrea.io/antrea-operator/controllers/types"
)

func TestOpenShiftEncryptionIntent(t *testing.T) {
	const certGates = "featureGates:\n  IPsecCertAuth: true\n"
	for _, tc := range []struct {
		name, top, agent, controller, psk, secret, errorText string
		disableWireGuard                                     bool
		want                                                 EncryptionProfile
	}{
		{name: "explicit unencrypted", top: "none", want: EncryptionProfile{Mode: EncryptionNone}},
		{name: "legacy IPsec override", top: "IPsec", agent: "trafficEncryptionMode: none\n", psk: "legacy key",
			want: EncryptionProfile{Mode: EncryptionIPsec, IPsecAuth: IPsecPSK, PSKSecretName: types.ManagedIPsecPSKName}},
		{name: "legacy PSK Secret", agent: "trafficEncryptionMode: ipsec\n",
			want: EncryptionProfile{Mode: EncryptionIPsec, IPsecAuth: IPsecPSK, PSKSecretName: types.DefaultIPsecPSKName}},
		{name: "IPsec certificate", top: "ipsec", agent: certGates + "ipsec:\n  authenticationMode: cert\n", controller: certGates,
			want: EncryptionProfile{Mode: EncryptionIPsec, IPsecAuth: IPsecCert}},
		{name: "certificate gate missing", top: "ipsec", agent: certGates + "ipsec:\n  authenticationMode: cert\n",
			errorText: "requires featureGates.IPsecCertAuth"},
		{name: "explicit WireGuard", agent: "trafficEncryptionMode: WireGuard\n", want: EncryptionProfile{Mode: EncryptionWireGuard}},
		{name: "WireGuard release exclusion", agent: "trafficEncryptionMode: wireGuard\n", disableWireGuard: true, errorText: "WireGuard is unavailable"},
		{name: "encrypted noEncap rejected", top: "wireGuard", agent: "trafficEncapMode: noEncap\n", errorText: "requires trafficEncapMode encap"},
		{name: "encrypted Hybrid rejected", top: "ipsec", agent: "trafficEncapMode: hybrid\n", errorText: "requires trafficEncapMode encap"},
		{name: "no silent downgrade", top: "none", agent: "trafficEncryptionMode: wireGuard\n", errorText: "conflicts with the explicit"},
		{name: "incorrect encryption type", agent: "trafficEncryptionMode: true\n", errorText: "must be a string"},
		{name: "unrecognized encryption mode", top: "secret", errorText: "invalid trafficEncryptionMode"},
		{name: "ambiguous PSK", top: "ipsec", psk: "legacy key", secret: "fixture-key", errorText: "mutually exclusive"},
		{name: "malformed PSK", top: "ipsec", psk: "invalid\nkey", errorText: "without control characters"},
		{name: "duplicate modes", agent: "trafficEncryptionMode: ipsec\ntrafficEncryptionMode: none\n", errorText: "already set"},
		{name: "noEncap needs AntreaProxy", agent: "trafficEncapMode: noEncap\nantreaProxy:\n  enable: false\n", errorText: "requires AntreaProxy"},
		{name: "kube proxy conflict", agent: "antreaProxy:\n  proxyAll: true\n", errorText: "must be false"},
		{name: "primary CNI", agent: "trafficEncapMode: networkPolicyOnly\n", errorText: "primary Antrea CNI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ConfigOc{DisableWireGuard: tc.disableWireGuard}
			install := mockOperConfig.DeepCopy()
			install.Spec.TrafficEncryptionMode = tc.top
			install.Spec.AntreaAgentConfig = tc.agent
			install.Spec.AntreaControllerConfig = tc.controller
			install.Spec.PSK = tc.psk
			install.Spec.IPsecPSKSecretName = tc.secret
			err := cfg.FillConfigs(&mockClusterConfig, install)
			if tc.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("expected %q error, got %v", tc.errorText, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := cfg.Encryption(install); err != nil || got != tc.want {
				t.Fatalf("profile = %+v, %v, want %+v", got, err, tc.want)
			}
		})
	}
}

func TestOpenShiftNetworkInputs(t *testing.T) {
	operator := mockOperatorNetwork.DeepCopy()
	operator.Spec.DefaultNetwork.Type = "antrea"
	if err := ValidateOpenShiftNetwork(&mockClusterConfig, operator); err != nil {
		t.Fatal(err)
	}
	operator.Spec.DeployKubeProxy = ptr.To(false)
	if err := ValidateOpenShiftNetwork(&mockClusterConfig, operator); err == nil {
		t.Fatal("an OpenShift bootstrap without the selected service proxy was accepted")
	}
	operator.Spec.DeployKubeProxy = nil
	operator.Spec.DefaultNetwork.Type = ocoperv1.NetworkTypeOVNKubernetes
	if err := ValidateOpenShiftNetwork(&mockClusterConfig, operator); err == nil {
		t.Fatal("an OpenShift network with another primary CNI was accepted")
	}
	cluster := mockClusterConfig.DeepCopy()
	cluster.Spec.ServiceNetwork = nil
	if err := (&ConfigOc{}).FillConfigs(cluster, mockOperConfig.DeepCopy()); err == nil {
		t.Fatal("an empty service network was accepted")
	}
	cluster = mockClusterConfig.DeepCopy()
	cluster.Spec.ClusterNetwork[0].HostPrefix = 8
	if err := (&ConfigOc{}).FillConfigs(cluster, mockOperConfig.DeepCopy()); err == nil {
		t.Fatal("an invalid node CIDR prefix was accepted")
	}
	install := mockOperConfig.DeepCopy()
	install.Spec.AntreaControllerConfig = "ipsecCSRSigner:\n  autoApprove: false\nnodeIPAM:\n  clusterCIDRs: [198.51.100.0/24]\n"
	if err := (&ConfigOc{}).FillConfigs(&mockClusterConfig, install); err != nil {
		t.Fatal(err)
	}
	rendered := install.Spec.AntreaControllerConfig
	if strings.Contains(rendered, "198.51.100.") || !strings.Contains(rendered, "autoApprove: false") ||
		!strings.Contains(rendered, "192.168.0.0/16") || !strings.Contains(rendered, "nodeCIDRMaskSizeIPv4: 24") {
		t.Fatalf("unexpected NodeIPAM and signer configuration:\n%s", rendered)
	}
}
