// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
	"unicode"

	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/util/validation"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/types"
)

const (
	EncryptionNone      = "none"
	EncryptionIPsec     = "ipsec"
	EncryptionWireGuard = "wireGuard"
	IPsecPSK            = "psk"
	IPsecCert           = "cert"
)

type EncryptionProfile struct {
	Mode          string
	IPsecAuth     string
	PSKSecretName string
}

// Encryption reads the effective agent and controller settings. The caller
// runs FillConfigs first to resolve the legacy top-level override.
func (c *ConfigOc) Encryption(operConfig *operatorv1.AntreaInstall) (EncryptionProfile, error) {
	return c.encryptionProfile(operConfig, false)
}

func (c *ConfigOc) encryptionProfile(operConfig *operatorv1.AntreaInstall, normalize bool) (EncryptionProfile, error) {
	var profile EncryptionProfile
	values, err := yamlMapping(operConfig.Spec.AntreaAgentConfig)
	if err != nil {
		return profile, fmt.Errorf("failed to parse AntreaAgentConfig: %w", err)
	}
	configured, err := stringOption(values["trafficEncryptionMode"], "trafficEncryptionMode")
	if err != nil {
		return profile, err
	}
	mode, err := encryptionMode(configured)
	if err != nil {
		return profile, err
	}
	if requested := operConfig.Spec.TrafficEncryptionMode; normalize && requested != "" {
		override, err := encryptionMode(requested)
		if err != nil {
			return profile, err
		}
		if mode != EncryptionNone && mode != override {
			return profile, fmt.Errorf("spec.trafficEncryptionMode conflicts with the explicit %s mode in antreaAgentConfig", mode)
		}
		mode = override
	}
	if mode == EncryptionWireGuard && c.DisableWireGuard {
		return profile, fmt.Errorf("WireGuard is unavailable in this OpenShift distribution; select a supported encrypted profile or restore the qualified distribution")
	}
	if err := validateOpenShiftAgent(values, mode); err != nil {
		return profile, err
	}

	profile.Mode = mode
	if mode == EncryptionIPsec {
		profile, err = ipsecProfile(values, operConfig)
		if err != nil {
			return profile, err
		}
	} else if operConfig.Spec.IPsecPSKSecretName != "" {
		return profile, fmt.Errorf("spec.ipsecPSKSecretName requires trafficEncryptionMode ipsec")
	}
	if normalize {
		values["trafficEncryptionMode"] = mode
		updated, err := yaml.Marshal(values)
		if err != nil {
			return profile, fmt.Errorf("failed to fill trafficEncryptionMode: %w", err)
		}
		operConfig.Spec.AntreaAgentConfig = string(updated)
	}
	return profile, nil
}

func encryptionMode(value string) (string, error) {
	switch strings.ToLower(value) {
	case "", EncryptionNone:
		return EncryptionNone, nil
	case EncryptionIPsec:
		return EncryptionIPsec, nil
	case "wireguard":
		return EncryptionWireGuard, nil
	default:
		return "", fmt.Errorf("invalid trafficEncryptionMode %q, expected none, ipsec or wireGuard", value)
	}
}

func validateOpenShiftAgent(values map[string]interface{}, mode string) error {
	encap, err := stringOption(values["trafficEncapMode"], "trafficEncapMode")
	if err != nil {
		return err
	}
	if encap == "" {
		encap = "encap"
	}
	encapMode := strings.ToLower(encap)
	switch encapMode {
	case "encap", "noencap", "hybrid":
	default:
		return fmt.Errorf("OpenShift primary Antrea CNI requires trafficEncapMode encap, noEncap or hybrid, got %q", encap)
	}
	if mode != EncryptionNone && encapMode != "encap" {
		return fmt.Errorf("trafficEncryptionMode %s requires trafficEncapMode encap", mode)
	}
	tunnel, err := stringOption(values["tunnelType"], "tunnelType")
	if err != nil {
		return err
	}
	switch tunnel {
	case "", "geneve", "vxlan", "gre", "stt":
	default:
		return fmt.Errorf("invalid tunnelType %q", tunnel)
	}
	if mode != EncryptionNone {
		multicast, err := nestedMapping(values, "multicast")
		if err != nil {
			return err
		}
		enabled, err := boolOption(multicast, "enable", false)
		if err != nil {
			return err
		}
		if enabled {
			return fmt.Errorf("multicast cannot be enabled with trafficEncryptionMode %s", mode)
		}
		if len(multicast) == 0 {
			delete(values, "multicast")
		}
	}
	if mode == EncryptionWireGuard {
		if err := validateWireGuard(values); err != nil {
			return err
		}
	}
	return validateOpenShiftProxy(values, encap, encapMode != "encap")
}

func validateWireGuard(values map[string]interface{}) error {
	if values["wireGuard"] == nil {
		return nil
	}
	wireGuard, err := nestedMapping(values, "wireGuard")
	if err != nil {
		return err
	}
	if port := wireGuard["port"]; port != nil {
		number, ok := port.(int)
		if !ok || number < 0 || number > 65535 {
			return fmt.Errorf("wireGuard.port must be an integer between 0 and 65535")
		}
	}
	return nil
}

func validateOpenShiftProxy(values map[string]interface{}, encap string, requiresAntreaProxy bool) error {
	gates, err := nestedMapping(values, "featureGates")
	if err != nil {
		return err
	}
	proxy, err := nestedMapping(values, "antreaProxy")
	if err != nil {
		return err
	}
	gate, err := boolOption(gates, "AntreaProxy", true)
	if err != nil {
		return err
	}
	enable, err := boolOption(proxy, "enable", true)
	if err != nil {
		return err
	}
	if requiresAntreaProxy && !(gate && enable) {
		return fmt.Errorf("trafficEncapMode %s requires AntreaProxy", encap)
	}
	all, err := boolOption(proxy, "proxyAll", false)
	if err != nil {
		return err
	}
	if all {
		return fmt.Errorf("antreaProxy.proxyAll must be false when OpenShift CNO runs kube-proxy")
	}
	return nil
}

func ipsecProfile(agent map[string]interface{}, operConfig *operatorv1.AntreaInstall) (EncryptionProfile, error) {
	profile := EncryptionProfile{Mode: EncryptionIPsec}
	ipsec, err := nestedMapping(agent, "ipsec")
	if err != nil {
		return profile, err
	}
	auth, err := stringOption(ipsec["authenticationMode"], "authenticationMode")
	if err != nil {
		return profile, err
	}
	switch strings.ToLower(auth) {
	case "", IPsecPSK:
		profile.IPsecAuth = IPsecPSK
		secret := operConfig.Spec.IPsecPSKSecretName
		if psk := operConfig.Spec.PSK; psk != "" {
			if secret != "" {
				return profile, fmt.Errorf("spec.psk and spec.ipsecPSKSecretName are mutually exclusive")
			}
			if !ValidIPsecPSK(psk) {
				return profile, fmt.Errorf("spec.psk must contain a nonblank key without control characters")
			}
			profile.PSKSecretName = types.ManagedIPsecPSKName
		} else {
			if secret == "" {
				secret = types.DefaultIPsecPSKName
			}
			if errs := validation.IsDNS1123Subdomain(secret); len(errs) > 0 || secret == types.ManagedIPsecPSKName || secret == types.IPsecCAName {
				return profile, fmt.Errorf("spec.ipsecPSKSecretName must be a valid, nonreserved Secret name")
			}
			profile.PSKSecretName = secret
		}
	case IPsecCert:
		profile.IPsecAuth = IPsecCert
		if operConfig.Spec.IPsecPSKSecretName != "" {
			return profile, fmt.Errorf("spec.ipsecPSKSecretName is only used by IPsec PSK authentication")
		}
		if err := validateIPsecCert(agent, operConfig.Spec.AntreaControllerConfig); err != nil {
			return profile, err
		}
	default:
		return profile, fmt.Errorf("ipsec.authenticationMode must be psk or cert")
	}
	return profile, nil
}

func validateIPsecCert(agent map[string]interface{}, controllerConfig string) error {
	controller, err := yamlMapping(controllerConfig)
	if err != nil {
		return fmt.Errorf("invalid AntreaControllerConfig for IPsec certificate authentication: %w", err)
	}
	for _, values := range []map[string]interface{}{agent, controller} {
		gates, err := nestedMapping(values, "featureGates")
		if err != nil {
			return err
		}
		enabled, err := boolOption(gates, "IPsecCertAuth", false)
		if err != nil {
			return err
		}
		if !enabled {
			return fmt.Errorf("IPsec certificate authentication requires featureGates.IPsecCertAuth enabled in agent and controller configs")
		}
	}
	signer, err := nestedMapping(controller, "ipsecCSRSigner")
	if err != nil {
		return err
	}
	selfSigned, err := boolOption(signer, "selfSignedCA", true)
	if err != nil {
		return err
	}
	if !selfSigned {
		return fmt.Errorf("OpenShift IPsec certificate authentication requires ipsecCSRSigner.selfSignedCA enabled")
	}
	autoApprove, err := boolOption(signer, "autoApprove", true)
	if err != nil {
		return err
	}
	if !autoApprove {
		return fmt.Errorf("OpenShift IPsec certificate authentication requires ipsecCSRSigner.autoApprove enabled")
	}
	return nil
}

func ValidIPsecPSK(value string) bool {
	return strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) == -1
}

func stringOption(value interface{}, key string) (string, error) {
	if value == nil {
		return "", nil
	}
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return str, nil
}

func boolOption(values map[interface{}]interface{}, key string, fallback bool) (bool, error) {
	if values[key] == nil {
		return fallback, nil
	}
	enabled, ok := values[key].(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return enabled, nil
}
