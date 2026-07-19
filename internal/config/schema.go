package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var yamlFieldAliases = map[reflect.Type]map[string]string{
	reflect.TypeOf(LoggingConfig{}): {
		"request_headers":  "redact_request_headers",
		"response_headers": "redact_response_headers",
	},
}

// validateConfigSchema reports unknown YAML keys with their complete config
// path before decoding. yaml.Decoder.KnownFields remains enabled as a second
// line of defense, while this pass supplies the actionable path and typo hint.
func validateConfigSchema(document *yaml.Node) error {
	if document == nil || len(document.Content) == 0 {
		return nil
	}
	node := document
	if node.Kind == yaml.DocumentNode {
		node = node.Content[0]
	}
	return validateConfigNode(node, reflect.TypeOf(Config{}), "")
}

func validateConfigNode(node *yaml.Node, expected reflect.Type, configPath string) error {
	for expected.Kind() == reflect.Pointer {
		expected = expected.Elem()
	}
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}

	switch expected.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		fields := yamlStructFields(expected)
		aliases := yamlFieldAliases[expected]
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode, valueNode := node.Content[i], node.Content[i+1]
			key := keyNode.Value
			fieldType, ok := fields[key]
			if !ok {
				if canonical, aliasOK := aliases[key]; aliasOK {
					fieldType = fields[canonical]
					ok = true
				}
			}
			currentPath := joinConfigPath(configPath, key)
			if !ok {
				message := fmt.Sprintf("line %d: unknown configuration key %q", keyNode.Line, currentPath)
				if suggestion := closestYAMLField(key, fields); suggestion != "" {
					message += fmt.Sprintf("; did you mean %q?", joinConfigPath(configPath, suggestion))
				}
				return fmt.Errorf("%s", message)
			}
			if err := validateConfigNode(valueNode, fieldType, currentPath); err != nil {
				return err
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if err := validateConfigNode(node.Content[i+1], expected.Elem(), joinConfigPath(configPath, key)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		for i, child := range node.Content {
			if err := validateConfigNode(child, expected.Elem(), fmt.Sprintf("%s[%d]", configPath, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func yamlStructFields(structType reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if tag == "-" {
			continue
		}
		if tag == "" {
			tag = strings.ToLower(field.Name)
		}
		fields[tag] = field.Type
	}
	return fields
}

func closestYAMLField(unknown string, fields map[string]reflect.Type) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	best := ""
	bestDistance := len(unknown) + 1
	for _, candidate := range keys {
		distance := levenshteinDistance(unknown, candidate)
		if distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	threshold := 2
	if len(unknown) >= 9 {
		threshold = 3
	}
	if bestDistance > threshold {
		return ""
	}
	return best
}

func levenshteinDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = minInt(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}

func minInt(values ...int) int {
	minimum := values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return minimum
}

func joinConfigPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
