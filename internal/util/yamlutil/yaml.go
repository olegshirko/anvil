package yamlutil

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"anvil/internal/domain"
	"anvil/internal/embedded"

	"gopkg.in/yaml.v3"
)

// MarshalToFile serialises a value as YAML and writes it to a file.
func MarshalToFile(v any, path string) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("cannot marshal to YAML: %w", err)
	}
	return os.WriteFile(path, b, 0644)
}

// PersistConfig writes the domain config to a YAML file while preserving
// comments from the embedded default template.
func PersistConfig(cfg domain.Config, path string) error {
	b, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("cannot create config directory: %w", err)
	}
	if err := os.WriteFile(path, b, 0644); err != nil {
		return fmt.Errorf("cannot write config file: %w", err)
	}
	return nil
}

// marshalConfig overlays the supplied config onto the embedded default YAML
// template, preserving comments and structure.
func marshalConfig(cfg domain.Config) ([]byte, error) {
	var doc yaml.Node

	tmpl, err := embedded.Read("defaults/anvil.yaml")
	if err != nil {
		return nil, fmt.Errorf("cannot read embedded default config: %w", err)
	}
	if err := yaml.Unmarshal(tmpl, &doc); err != nil {
		return nil, fmt.Errorf("embedded default config is invalid YAML: %w", err)
	}

	if len(doc.Content) != 1 {
		return nil, fmt.Errorf("unexpected YAML document root: %d top-level nodes", len(doc.Content))
	}
	root := doc.Content[0]

	// index every node by its dotted path
	nodeIndex := map[string]*yaml.Node{}
	if err := walkYamlNodes("", root, nodeIndex); err != nil {
		return nil, fmt.Errorf("cannot index YAML nodes: %w", err)
	}

	// flatten the struct into a dotted-path map
	valueIndex := map[string]any{}
	extractStructValues("", cfg, valueIndex)

	// replace each template node with the matching config value
	for path, node := range nodeIndex {
		val := valueIndex[path]

		// keep top-level mapping nodes unless they correspond to explicit maps
		if node.Kind == yaml.MappingNode {
			switch val.(type) {
			case map[string]any, map[string]string:
				// okay, overlay
			default:
				continue
			}
		}

		// round-trip through the yaml library to build the replacement node
		b, err := yaml.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("cannot marshal value for %q: %w", path, err)
		}
		var wrapped yaml.Node
		if err := yaml.Unmarshal(b, &wrapped); err != nil {
			return nil, fmt.Errorf("cannot reconstruct YAML node for %q: %w", path, err)
		}
		if len(wrapped.Content) != 1 {
			return nil, fmt.Errorf("unexpected multi-root YAML fragment for %q", path)
		}
		*node = *wrapped.Content[0]
	}

	return encodeNode(root)
}

// extractStructValues recursively flattens a struct into dotted-path keys.
func extractStructValues(prefix string, s any, out map[string]any) {
	typ := reflect.TypeOf(s)
	val := reflect.ValueOf(s)

	if typ.Kind() != reflect.Struct {
		out[prefix] = val.Interface()
		return
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := strings.TrimSuffix(field.Tag.Get("yaml"), ",omitempty")
		if tag == "" || tag == "-" {
			continue
		}
		key := tag
		if prefix != "" {
			key = prefix + "." + tag
		}
		extractStructValues(key, val.Field(i).Interface(), out)
	}
}

// walkYamlNodes recursively indexes YAML nodes by dotted path.
func walkYamlNodes(prefix string, node *yaml.Node, out map[string]*yaml.Node) error {
	switch node.Kind {
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return fmt.Errorf("mapping node has uneven %d children", len(node.Content))
		}
		for i := 0; i < len(node.Content); i += 2 {
			if i > 1 {
				// preserve comment spacing
				if cn := node.Content[i]; cn.HeadComment != "" && strings.HasPrefix(cn.HeadComment, "#") {
					cn.HeadComment = "\n" + cn.HeadComment
				}
			}
			key := node.Content[i].Value
			if prefix != "" {
				key = prefix + "." + key
			}
			child := node.Content[i+1]
			out[key] = child
			if err := walkYamlNodes(key, child, out); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i := 0; i < len(node.Content); i++ {
			key := strconv.Itoa(i)
			if prefix != "" {
				key = prefix + "." + key
			}
			child := node.Content[i]
			out[key] = child
			if err := walkYamlNodes(key, child, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// encodeNode serialises a *yaml.Node back to bytes with 2-space indentation.
func encodeNode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
