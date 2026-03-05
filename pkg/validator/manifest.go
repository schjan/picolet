package validator

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"

	"go.yaml.in/yaml/v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

// supportedKinds lists Kubernetes resource kinds supported by podman kube play.
var supportedKinds = map[string]bool{
	"Pod":                   true,
	"Deployment":            true,
	"DaemonSet":             true,
	"ConfigMap":             true,
	"Secret":                true,
	"PersistentVolumeClaim": true,
}

// k8sMeta holds just enough to extract kind/apiVersion for dispatch.
type k8sMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
}

func validateManifest(path string, content []byte) error {
	var errs []error

	docs := splitYAMLDocuments(content)
	if len(docs) == 0 {
		return fmt.Errorf("%s: empty manifest (no YAML documents)", path)
	}

	for docIdx, doc := range docs {
		// First pass: extract kind for dispatch
		var meta k8sMeta
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			errs = append(errs, fmt.Errorf("%s: document %d: YAML parse error: %w", path, docIdx, err))
			continue
		}

		if meta.Kind == "" {
			errs = append(errs, fmt.Errorf("%s: document %d: missing 'kind' field", path, docIdx))
			continue
		}
		if meta.APIVersion == "" {
			errs = append(errs, fmt.Errorf("%s: document %d: missing 'apiVersion' field", path, docIdx))
		}
		if meta.Metadata.Name == "" {
			errs = append(errs, fmt.Errorf("%s: document %d: missing 'metadata.name' field", path, docIdx))
		}

		if !supportedKinds[meta.Kind] {
			errs = append(errs, fmt.Errorf("%s: document %d: kind %q not supported by podman kube play", path, docIdx, meta.Kind))
			continue
		}

		// Second pass: unmarshal into real K8s types for schema validation.
		// sigs.k8s.io/yaml handles the json struct tags used by K8s types.
		if err := unmarshalK8sType(meta.Kind, doc); err != nil {
			errs = append(errs, fmt.Errorf("%s: document %d: %s: %w", path, docIdx, meta.Kind, err))
		}
	}

	return errors.Join(errs...)
}

// unmarshalK8sType unmarshals a YAML document into the corresponding K8s type.
// This catches wrong field names, wrong types, and structural issues.
func unmarshalK8sType(kind string, doc []byte) error {
	switch kind {
	case "Deployment":
		var d appsv1.Deployment
		return sigsyaml.UnmarshalStrict(doc, &d)
	case "DaemonSet":
		var d appsv1.DaemonSet
		return sigsyaml.UnmarshalStrict(doc, &d)
	case "Pod":
		var p corev1.Pod
		return sigsyaml.UnmarshalStrict(doc, &p)
	case "ConfigMap":
		var c corev1.ConfigMap
		return sigsyaml.UnmarshalStrict(doc, &c)
	case "Secret":
		var s corev1.Secret
		return sigsyaml.UnmarshalStrict(doc, &s)
	case "PersistentVolumeClaim":
		var p corev1.PersistentVolumeClaim
		return sigsyaml.UnmarshalStrict(doc, &p)
	}
	return nil
}

// splitYAMLDocuments splits a multi-document YAML file on "---" separators.
func splitYAMLDocuments(content []byte) [][]byte {
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(content)))
	var docs [][]byte
	for {
		doc, err := reader.Read()
		if err != nil {
			break // io.EOF or parse error
		}
		if len(bytes.TrimSpace(doc)) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}
