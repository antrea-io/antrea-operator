// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"antrea.io/antrea/pkg/features"
	configv1 "github.com/openshift/api/config/v1"
	ocoperv1 "github.com/openshift/api/operator/v1"
	"gopkg.in/yaml.v2"

	operatorv1 "antrea.io/antrea-operator/api/v1"
)

// ValidateOpenShiftNetwork checks the installer and CNO inputs before
// applying any primary CNI resources.
func ValidateOpenShiftNetwork(cluster *configv1.Network, operator *ocoperv1.Network) error {
	if err := validateClusterCIDRs(cluster); err != nil {
		return err
	}
	if !strings.EqualFold(cluster.Spec.NetworkType, "antrea") {
		return fmt.Errorf("Network.config.openshift.io/cluster spec.networkType must be antrea, got %q", cluster.Spec.NetworkType)
	}
	if operator == nil {
		return fmt.Errorf("Network.operator.openshift.io/cluster is required")
	}
	if !strings.EqualFold(string(operator.Spec.DefaultNetwork.Type), "antrea") {
		return fmt.Errorf("Network.operator.openshift.io/cluster spec.defaultNetwork.type must be antrea, got %q", operator.Spec.DefaultNetwork.Type)
	}
	if proxy := operator.Spec.DeployKubeProxy; proxy != nil && !*proxy {
		return fmt.Errorf("OpenShift Antrea bootstrap requires Network.operator.openshift.io/cluster spec.deployKubeProxy to be enabled")
	}
	return nil
}

func validateClusterCIDRs(cluster *configv1.Network) error {
	if cluster == nil {
		return fmt.Errorf("Network.config.openshift.io/cluster is required")
	}
	if len(cluster.Spec.ServiceNetwork) == 0 || len(cluster.Spec.ClusterNetwork) == 0 {
		return fmt.Errorf("OpenShift Network config requires spec.serviceNetwork and spec.clusterNetwork")
	}
	services := make(map[bool]netip.Prefix)
	for _, value := range cluster.Spec.ServiceNetwork {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("invalid spec.serviceNetwork CIDR %q", value)
		}
		family := prefix.Addr().Is4()
		if _, exists := services[family]; exists {
			return fmt.Errorf("spec.serviceNetwork must contain at most one CIDR per IP family")
		}
		services[family] = prefix
	}
	pods := make(map[bool]netip.Prefix)
	for _, entry := range cluster.Spec.ClusterNetwork {
		prefix, err := netip.ParsePrefix(entry.CIDR)
		if err != nil {
			return fmt.Errorf("invalid spec.clusterNetwork CIDR %q", entry.CIDR)
		}
		family := prefix.Addr().Is4()
		if _, exists := pods[family]; exists {
			return fmt.Errorf("Antrea NodeIPAM accepts at most one clusterNetwork CIDR per IP family")
		}
		if entry.HostPrefix < uint32(prefix.Bits()) || entry.HostPrefix > uint32(prefix.Addr().BitLen()-2) || entry.HostPrefix == 0 {
			return fmt.Errorf("invalid hostPrefix %d for spec.clusterNetwork CIDR %q", entry.HostPrefix, entry.CIDR)
		}
		service, exists := services[family]
		if !exists {
			return fmt.Errorf("spec.serviceNetwork and spec.clusterNetwork must use the same IP families")
		}
		if prefix.Overlaps(service) {
			return fmt.Errorf("cluster network %s overlaps service network %s", prefix, service)
		}
		pods[family] = prefix
	}
	if len(pods) != len(services) {
		return fmt.Errorf("spec.serviceNetwork and spec.clusterNetwork must use the same IP families")
	}
	return nil
}

func yamlMapping(data string) (map[string]interface{}, error) {
	var values map[string]interface{}
	if err := yaml.UnmarshalStrict([]byte(data), &values); err != nil {
		return nil, err
	}
	if values == nil {
		values = make(map[string]interface{})
	}
	return values, nil
}

func fillOpenShiftControllerConfig(cluster *configv1.Network, install *operatorv1.AntreaInstall) error {
	values, err := yamlMapping(install.Spec.AntreaControllerConfig)
	if err != nil {
		return fmt.Errorf("failed to parse AntreaControllerConfig: %w", err)
	}
	gates, err := nestedMapping(values, "featureGates")
	if err != nil {
		return fmt.Errorf("AntreaControllerConfig: %w", err)
	}
	nodeIPAM, err := nestedMapping(values, "nodeIPAM")
	if err != nil {
		return fmt.Errorf("AntreaControllerConfig: %w", err)
	}

	gates[string(features.NodeIPAM)] = true
	nodeIPAM["enableNodeIPAM"] = true
	delete(nodeIPAM, "serviceCIDR")
	delete(nodeIPAM, "serviceCIDRv6")
	delete(nodeIPAM, "nodeCIDRMaskSizeIPv4")
	delete(nodeIPAM, "nodeCIDRMaskSizeIPv6")

	cidrs := make([]string, 0, len(cluster.Spec.ClusterNetwork))
	for _, entry := range cluster.Spec.ClusterNetwork {
		cidrs = append(cidrs, entry.CIDR)
		prefix := netip.MustParsePrefix(entry.CIDR)
		key := "nodeCIDRMaskSizeIPv6"
		if prefix.Addr().Is4() {
			key = "nodeCIDRMaskSizeIPv4"
		}
		nodeIPAM[key] = entry.HostPrefix
	}
	nodeIPAM["clusterCIDRs"] = cidrs
	for _, cidr := range cluster.Spec.ServiceNetwork {
		key := "serviceCIDRv6"
		if netip.MustParsePrefix(cidr).Addr().Is4() {
			key = "serviceCIDR"
		}
		nodeIPAM[key] = cidr
	}

	updated, err := yaml.Marshal(values)
	if err != nil {
		return fmt.Errorf("failed to fill AntreaControllerConfig: %w", err)
	}
	install.Spec.AntreaControllerConfig = string(updated)
	return nil
}

func nestedMapping(values map[string]interface{}, key string) (map[interface{}]interface{}, error) {
	if values[key] == nil {
		nested := make(map[interface{}]interface{})
		values[key] = nested
		return nested, nil
	}
	nested, ok := values[key].(map[interface{}]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	return nested, nil
}

func validateOpenShiftCNI(raw string) error {
	var config struct {
		Name       string `json:"name"`
		CNIVersion string `json:"cniVersion"`
		Plugins    []struct {
			Type string `json:"type"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return fmt.Errorf("spec.antreaCNIConfig must contain a valid CNI JSON conflist: %w", err)
	}
	if config.Name == "" || config.CNIVersion == "" || len(config.Plugins) == 0 || config.Plugins[0].Type != "antrea" {
		return fmt.Errorf("spec.antreaCNIConfig requires name, cniVersion and antrea as the first plugin")
	}
	return nil
}
