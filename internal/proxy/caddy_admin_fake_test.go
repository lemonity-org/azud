package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// fakeCaddyAdmin applies admin API requests to an in-memory configuration
// with the semantics Azud relies on, which scripts/caddy_admin_contract_test.sh
// checks against the pinned Caddy image: /config paths, /id lookups, POST
// appending to an array, PUT inserting at an array index and refusing an
// existing key, PATCH replacing only an existing value, DELETE, and a
// configuration with a duplicate @id refused as a whole.
type fakeCaddyAdmin struct {
	config   interface{}
	requests []string
}

func newFakeCaddyAdmin(t *testing.T, config string) *fakeCaddyAdmin {
	t.Helper()
	value, err := decodeJSONValue([]byte(config))
	if err != nil {
		t.Fatalf("fake Caddy config: %v", err)
	}
	return &fakeCaddyAdmin{config: value}
}

// serve answers one `podman exec azud-proxy curl ...` command as curl -f
// would: the body on success, exit 22 on an HTTP error.
func (a *fakeCaddyAdmin) serve(command string, stdin io.Reader) (string, string, int) {
	fields := strings.Fields(command)
	method := "GET"
	for i, field := range fields {
		if field == "-X" && i+1 < len(fields) {
			method = strings.Trim(fields[i+1], "'")
		}
	}
	target := strings.Trim(fields[len(fields)-1], "'")
	path := strings.TrimPrefix(target, "http://localhost")
	var body interface{}
	if strings.Contains(command, "--data-binary") {
		data, _ := io.ReadAll(stdin)
		value, err := decodeJSONValue(data)
		if err != nil {
			return "", "curl: (22) The requested URL returned error: 400", 22
		}
		body = value
	}
	a.requests = append(a.requests, method+" "+path)

	result, status := a.apply(method, path, body)
	if status >= 400 {
		return "", fmt.Sprintf("curl: (22) The requested URL returned error: %d", status), 22
	}
	data, _ := json.Marshal(result)
	return string(data) + "\n", "", 0
}

func (a *fakeCaddyAdmin) apply(method, path string, body interface{}) (interface{}, int) {
	if path == "/load" {
		if method != "POST" {
			return nil, 405
		}
		if duplicateCaddyID(body) {
			return nil, 400
		}
		a.config = body
		return nil, 200
	}

	var parts []string
	switch {
	case path == "/config" || path == "/config/":
	case strings.HasPrefix(path, "/config/"):
		parts = strings.Split(strings.Trim(strings.TrimPrefix(path, "/config/"), "/"), "/")
	case strings.HasPrefix(path, "/id/"):
		rest := strings.SplitN(strings.TrimPrefix(path, "/id/"), "/", 2)
		id, err := url.PathUnescape(rest[0])
		if err != nil {
			return nil, 400
		}
		found, ok := caddyIDPathOf(a.config, id, nil)
		if !ok {
			return nil, 404
		}
		parts = found
		if len(rest) == 2 && rest[1] != "" {
			parts = append(parts, strings.Split(strings.Trim(rest[1], "/"), "/")...)
		}
	default:
		return nil, 404
	}

	if len(parts) == 0 {
		switch method {
		case "GET":
			return a.config, 200
		case "POST", "PATCH":
			if duplicateCaddyID(body) {
				return nil, 400
			}
			a.config = body
			return nil, 200
		default:
			return nil, 405
		}
	}

	working := cloneJSONValue(a.config)
	updated, result, status := applyCaddyAdmin(working, parts, method, body)
	if status >= 400 || method == "GET" {
		return result, status
	}
	if duplicateCaddyID(updated) {
		return nil, 400
	}
	a.config = updated
	return nil, 200
}

// applyCaddyAdmin applies one request at parts below node and returns the
// updated node.
func applyCaddyAdmin(node interface{}, parts []string, method string, body interface{}) (interface{}, interface{}, int) {
	key := parts[0]
	last := len(parts) == 1
	switch typed := node.(type) {
	case map[string]interface{}:
		value, exists := typed[key]
		if !last {
			if !exists {
				return node, nil, 400
			}
			child, result, status := applyCaddyAdmin(value, parts[1:], method, body)
			typed[key] = child
			return typed, result, status
		}
		switch method {
		case "GET":
			return typed, value, 200
		case "POST":
			if array, ok := value.([]interface{}); ok {
				typed[key] = append(array, body)
			} else {
				typed[key] = body
			}
		case "PUT":
			if exists {
				return typed, nil, 409
			}
			typed[key] = body
		case "PATCH":
			if !exists {
				return typed, nil, 404
			}
			typed[key] = body
		case "DELETE":
			if !exists {
				return typed, nil, 404
			}
			delete(typed, key)
		}
		return typed, nil, 200
	case []interface{}:
		index, err := strconv.Atoi(key)
		if err != nil || index < 0 || index > len(typed) {
			return node, nil, 400
		}
		if !last {
			if index == len(typed) {
				return node, nil, 400
			}
			child, result, status := applyCaddyAdmin(typed[index], parts[1:], method, body)
			typed[index] = child
			return typed, result, status
		}
		switch method {
		case "PUT":
			typed = append(typed[:index], append([]interface{}{body}, typed[index:]...)...)
			return typed, nil, 200
		case "POST":
			if index != len(typed) {
				return node, nil, 400
			}
			return append(typed, body), nil, 200
		}
		if index == len(typed) {
			return node, nil, 404
		}
		switch method {
		case "GET":
			return typed, typed[index], 200
		case "PATCH":
			typed[index] = body
		case "DELETE":
			typed = append(typed[:index], typed[index+1:]...)
		}
		return typed, nil, 200
	default:
		return node, nil, 400
	}
}

// caddyIDPathOf returns the config path of the object whose @id is id.
func caddyIDPathOf(node interface{}, id string, prefix []string) ([]string, bool) {
	switch typed := node.(type) {
	case map[string]interface{}:
		if typed["@id"] == id {
			return prefix, true
		}
		for key, child := range typed {
			if found, ok := caddyIDPathOf(child, id, append(append([]string{}, prefix...), key)); ok {
				return found, true
			}
		}
	case []interface{}:
		for index, child := range typed {
			if found, ok := caddyIDPathOf(child, id, append(append([]string{}, prefix...), strconv.Itoa(index))); ok {
				return found, true
			}
		}
	}
	return nil, false
}

func duplicateCaddyID(node interface{}) bool {
	seen := map[string]bool{}
	var walk func(interface{}) bool
	walk = func(node interface{}) bool {
		switch typed := node.(type) {
		case map[string]interface{}:
			if id, ok := typed["@id"].(string); ok {
				if seen[id] {
					return true
				}
				seen[id] = true
			}
			for _, child := range typed {
				if walk(child) {
					return true
				}
			}
		case []interface{}:
			for _, child := range typed {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(node)
}

func cloneJSONValue(value interface{}) interface{} {
	data, _ := json.Marshal(value)
	cloned, _ := decodeJSONValue(data)
	return cloned
}

// routeIDs returns the @id of every srv0 route, in order.
func (a *fakeCaddyAdmin) routeIDs(t *testing.T) []string {
	t.Helper()
	root, _ := a.config.(map[string]interface{})
	routes, _ := jsonPath(root, "apps", "http", "servers", "srv0", "routes").([]interface{})
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		object, _ := route.(map[string]interface{})
		id, _ := object["@id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// route returns the srv0 route with the given @id, encoded as JSON.
func (a *fakeCaddyAdmin) route(t *testing.T, id string) map[string]interface{} {
	t.Helper()
	root, _ := a.config.(map[string]interface{})
	routes, _ := jsonPath(root, "apps", "http", "servers", "srv0", "routes").([]interface{})
	for _, route := range routes {
		object, _ := route.(map[string]interface{})
		if object["@id"] == id {
			return object
		}
	}
	t.Fatalf("no route %s in %s", id, a.dump())
	return nil
}

// upstreamDials returns the dial addresses of a route's reverse_proxy.
func upstreamDials(route map[string]interface{}) []string {
	handlers, _ := jsonObjectSlice(route["handle"])
	reverse := findJSONHandler(handlers, "reverse_proxy")
	upstreams, _ := jsonObjectSlice(reverse["upstreams"])
	var dials []string
	for _, upstream := range upstreams {
		dial, _ := upstream["dial"].(string)
		dials = append(dials, dial)
	}
	return dials
}

func (a *fakeCaddyAdmin) value(path ...string) interface{} {
	root, _ := a.config.(map[string]interface{})
	return jsonPath(root, path...)
}

func (a *fakeCaddyAdmin) dump() string {
	var out bytes.Buffer
	data, _ := json.Marshal(a.config)
	_ = json.Indent(&out, data, "", "  ")
	return out.String()
}
