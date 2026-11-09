// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"encoding/json"
	"errors"
	"slices"
)

// Registries prevent unselected protocol constructors. Shared upstream TLS and
// transport implementations also need a runtime gate against the exact receipt.
func checkSelectedCapabilities(content string) error {
	var selected map[string][]string
	if err := json.Unmarshal([]byte(selectedFeaturesJSON), &selected); err != nil {
		return errors.New("invalid compiled mobile capabilities")
	}
	return checkCapabilities(content, selected)
}

func checkCapabilities(content string, selected map[string][]string) error {
	var root map[string]any
	if err := json.Unmarshal([]byte(content), &root); err != nil || root == nil {
		return errors.New("mobile configuration must be a JSON object")
	}
	for _, category := range []string{"inbounds", "outbounds", "endpoints", "services", "certificate_providers"} {
		if value, exists := root[category]; exists {
			if err := checkTypedList(value, category, selected); err != nil {
				return err
			}
		}
	}
	if !slices.Contains(selected["outbounds"], "direct") {
		outbounds, _ := root["outbounds"].([]any)
		route, _ := root["route"].(map[string]any)
		final, _ := route["final"].(string)
		if len(outbounds) == 0 && final == "" {
			return errors.New("direct capability was not selected for this core")
		}
	}
	if value, exists := root["dns"]; exists {
		dns, ok := value.(map[string]any)
		if !ok {
			return errors.New("mobile DNS must be an object")
		}
		if servers, exists := dns["servers"]; exists {
			if err := checkTypedList(servers, "dns", selected); err != nil {
				return err
			}
		}
	}
	if value, exists := root["experimental"]; exists {
		experimental, ok := value.(map[string]any)
		if !ok {
			return errors.New("mobile experimental options must be an object")
		}
		for _, name := range []string{"clash_api", "v2ray_api"} {
			if value, exists := experimental[name]; exists {
				if _, ok := value.(map[string]any); !ok {
					return errors.New("mobile API options must be an object")
				}
				if !slices.Contains(selected["api"], name) {
					return errors.New("API capability was not selected for this core")
				}
			}
		}
	}
	return walkCapabilities(root, "", selected)
}

func checkTypedList(value any, category string, selected map[string][]string) error {
	items, ok := value.([]any)
	if !ok {
		return errors.New("mobile protocol lists must be arrays")
	}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			return errors.New("mobile protocol entries must be objects")
		}
		if err := checkSelectedType(item, category, selected); err != nil {
			return err
		}
	}
	return nil
}

func checkSelectedType(item map[string]any, category string, selected map[string][]string) error {
	name, ok := item["type"].(string)
	if !ok || name == "" {
		return errors.New("mobile protocol type is required")
	}
	if !slices.Contains(selected[category], name) {
		return errors.New("capability was not selected for this core")
	}
	return nil
}

func walkCapabilities(value any, protocol string, selected map[string][]string) error {
	switch item := value.(type) {
	case []any:
		for _, child := range item {
			if err := walkCapabilities(child, protocol, selected); err != nil {
				return err
			}
		}
	case map[string]any:
		if name, ok := item["type"].(string); ok {
			protocol = name
		}
		for key, child := range item {
			switch key {
			case "headers", "header", "predefined", "environment", "env":
				// These dictionaries contain arbitrary application/user keys.
				continue
			case "transport_options":
				// A registered WGC application transport owns its opaque options.
				if protocol == "wgc" {
					continue
				}
			case "transport":
				if transport, ok := child.(map[string]any); ok {
					if err := checkSelectedType(transport, "transports", selected); err != nil {
						return err
					}
				} else if protocol == "vmess" || protocol == "vless" || protocol == "trojan" {
					return errors.New("mobile stream transport must be an object")
				}
			case "tls":
				tls, ok := child.(map[string]any)
				if !ok {
					return errors.New("mobile TLS options must be an object")
				}
				if err := checkTLSCapabilities(tls, selected); err != nil {
					return err
				}
			case "certificate_provider":
				switch provider := child.(type) {
				case string:
					if provider == "" {
						return errors.New("mobile certificate provider reference is empty")
					}
				case map[string]any:
					if err := checkSelectedType(provider, "certificate_providers", selected); err != nil {
						return err
					}
				default:
					return errors.New("invalid mobile certificate provider")
				}
			}
			if err := walkCapabilities(child, protocol, selected); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkTLSCapabilities(tls map[string]any, selected map[string][]string) error {
	if enabled, exists := tls["enabled"]; exists {
		if _, ok := enabled.(bool); !ok {
			return errors.New("mobile TLS enabled must be boolean")
		}
	}
	for _, name := range []string{"utls", "reality", "ech"} {
		value, exists := tls[name]
		if !exists {
			continue
		}
		feature, ok := value.(map[string]any)
		if !ok {
			return errors.New("mobile TLS feature must be an object")
		}
		if enabled, exists := feature["enabled"]; exists {
			active, ok := enabled.(bool)
			if !ok {
				return errors.New("mobile TLS feature enabled must be boolean")
			}
			if active && !slices.Contains(selected["tls"], name) {
				return errors.New("TLS capability was not selected for this core")
			}
		}
	}
	return nil
}
