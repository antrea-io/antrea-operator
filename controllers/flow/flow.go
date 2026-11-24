// SPDX-License-Identifier: Apache-2.0

// Package flow builds the optional OpenShift flow resources from the upstream
// agent and Flow Aggregator contracts.
package flow

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"text/template"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-network-operator/pkg/names"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	operatorv1 "antrea.io/antrea-operator/api/v1"
	"antrea.io/antrea-operator/controllers/types"
)

const (
	ImageKey       = "FlowAggregatorImage"
	ComponentLabel = "operator.antrea.io/component"
	Name           = "flow-aggregator"
	DestinationCRD = "flowexporterdestinations.crd.antrea.io"
)

var DestinationGVK = schema.GroupVersionKind{Group: "crd.antrea.io", Version: "v1alpha1", Kind: "FlowExporterDestination"}

//go:embed resources.yaml
var resources string

//go:embed destination-crd.yaml
var destinationDefinition []byte

//go:embed aggregator.conf
var configTemplate string

var aggregatorConfig = template.Must(template.New("config").Parse(configTemplate))
var manifest = template.Must(template.New("flow").Parse(resources))

type settings struct {
	operatorv1.FlowAggregatorSpec
	AntreaNamespace string
	Config          string
	ConfigHash      string
}

func Render(spec *operatorv1.FlowAggregatorSpec, namespace string, values map[string]interface{}) ([]*unstructured.Unstructured, error) {
	if spec == nil || !spec.Enabled {
		return nil, nil
	}
	opts := settings{FlowAggregatorSpec: *spec, AntreaNamespace: namespace}
	if opts.Image == "" {
		opts.Image, _ = values[ImageKey].(string)
	}
	if strings.TrimSpace(opts.Image) == "" || strings.ContainsAny(opts.Image, " \n\r\t") {
		return nil, fmt.Errorf("an enabled Flow Aggregator requires an image in spec.flowAggregator.image or %s", ImageKey)
	}
	if opts.Mode == "" {
		opts.Mode = "Aggregate"
	}
	if opts.Mode != "Aggregate" && opts.Mode != "Proxy" {
		return nil, fmt.Errorf("unsupported Flow Aggregator mode %q", opts.Mode)
	}
	host, port, err := net.SplitHostPort(opts.CollectorAddress)
	n, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || portErr != nil || n == 0 || len(validation.IsDNS1123Subdomain(host)) != 0 {
		return nil, fmt.Errorf("flowAggregator.collectorAddress must specify an IPv4 or DNS host and TCP port")
	}
	// IsDNS1123Subdomain accepts strings composed only of dots and digits.
	if strings.Trim(host, ".0123456789") == "" && net.ParseIP(host).To4() == nil {
		return nil, fmt.Errorf("invalid IPv4 collector address %q", host)
	}
	var conf bytes.Buffer
	if err := aggregatorConfig.Execute(&conf, opts); err != nil {
		return nil, err
	}
	opts.Config = conf.String()
	opts.ConfigHash = fmt.Sprintf("%x", sha256.Sum256(conf.Bytes()))
	agent, ok := values[types.AntreaAgentConfigRenderKey].(string)
	if !ok {
		return nil, fmt.Errorf("missing rendered %s", types.AntreaAgentConfigRenderKey)
	}
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(agent), &config); err != nil {
		return nil, fmt.Errorf("invalid agent configuration: %w", err)
	}
	if config == nil {
		config = make(map[string]interface{})
	}
	// FlowExporter gates the dynamic destination informer. The independent
	// static destination retains the user's requested configuration.
	if config["featureGates"] == nil {
		config["featureGates"] = map[string]interface{}{}
	}
	if err := unstructured.SetNestedField(config, true, "featureGates", "FlowExporter"); err != nil {
		return nil, err
	}
	b, err := yaml.Marshal(config)
	if err != nil {
		return nil, err
	}
	values[types.AntreaAgentConfigRenderKey] = string(b)
	return objects(opts)
}

func objects(s settings) ([]*unstructured.Unstructured, error) {
	var buf bytes.Buffer
	if err := manifest.Execute(&buf, s); err != nil {
		return nil, err
	}
	d := yamlutil.NewYAMLOrJSONDecoder(&buf, 4096)
	var result []*unstructured.Unstructured
	for {
		obj := new(unstructured.Unstructured)
		if err := d.Decode(obj); err != nil {
			if err == io.EOF {
				return result, nil
			}
			return nil, err
		}
		obj.SetLabels(map[string]string{ComponentLabel: Name})
		result = append(result, obj)
	}
}

// OwnedResources returns the complete optional identity set to include in a
// distribution's ResourceBundle, independent of the current enablement state.
func OwnedResources(namespace string) ([]configv1.ObjectReference, error) {
	objs, err := objects(settings{AntreaNamespace: namespace})
	if err != nil {
		return nil, err
	}
	refs := make([]configv1.ObjectReference, 0, len(objs))
	for _, obj := range objs {
		resource, _ := meta.UnsafeGuessKindToResource(obj.GroupVersionKind())
		refs = append(refs, configv1.ObjectReference{Group: resource.Group, Resource: resource.Resource, Namespace: obj.GetNamespace(), Name: obj.GetName()})
	}
	return refs, nil
}

// EnsureDestinationCRD supplies the pinned shared CRD with the standalone
// core. A distribution owns the version of the CRD in its generated bundle.
func EnsureDestinationCRD(objs []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
	if HasDestinationCRD(objs) {
		return objs, nil
	}
	crd := new(unstructured.Unstructured)
	if err := yaml.Unmarshal(destinationDefinition, crd); err != nil {
		return nil, err
	}
	crd.SetAnnotations(map[string]string{names.CreateOnlyAnnotation: "true"})
	return append(objs, crd), nil
}

// HasDestinationCRD identifies an install carrying the shared API in the same apply.
func HasDestinationCRD(objs []*unstructured.Unstructured) bool {
	for _, obj := range objs {
		if obj.GroupVersionKind().Group == "apiextensions.k8s.io" && obj.GetKind() == "CustomResourceDefinition" && obj.GetName() == DestinationCRD {
			return true
		}
	}
	return false
}
