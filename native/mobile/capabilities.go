// SPDX-License-Identifier: GPL-3.0-or-later
package libbox

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/sagernet/sing-box/option"
)

// Registries enforce protocol selection while decoding. Shared upstream TLS,
// transport and API implementations also need a gate against the exact receipt.
func checkSelectedCapabilities(options option.Options) error {
	selected, err := decodeCapabilities(selectedFeaturesJSON)
	if err != nil {
		return err
	}
	return checkCapabilities(options, selected)
}

func decodeCapabilities(content string) (map[string][]string, error) {
	var selected map[string][]string
	if err := json.Unmarshal([]byte(content), &selected); err != nil || selected == nil {
		return nil, errors.New("invalid compiled capabilities")
	}
	return selected, nil
}

// Inspect decoded option types, never JSON field names: application dictionaries
// can contain keys such as "tls" or "transport" without requesting a capability.
// Interfaces and json:"-" fields include the registries' concrete option values.
func checkCapabilities(options any, selected map[string][]string) error {
	type visit struct {
		typ     reflect.Type
		pointer uintptr
		length  int
	}
	seen := make(map[visit]bool)
	var walk func(reflect.Value) error
	walk = func(value reflect.Value) error {
		if !value.IsValid() {
			return nil
		}
		kind := value.Kind()
		if kind == reflect.Slice || kind == reflect.Array || kind == reflect.Map {
			// Named scalar types cannot contain option objects either. Skip their
			// entire containers, including large string lists and headers.
			switch value.Type().Elem().Kind() {
			case reflect.Interface, reflect.Pointer, reflect.Struct, reflect.Slice, reflect.Array, reflect.Map:
			default:
				return nil
			}
		}
		switch kind {
		case reflect.Interface:
			if !value.IsNil() {
				return walk(value.Elem())
			}
		case reflect.Pointer, reflect.Map, reflect.Slice:
			if value.IsNil() {
				return nil
			}
			key := visit{typ: value.Type(), pointer: uintptr(value.UnsafePointer())}
			if value.Kind() == reflect.Slice {
				key.length = value.Len()
			}
			if seen[key] {
				return nil
			}
			seen[key] = true
			switch value.Kind() {
			case reflect.Pointer:
				return walk(value.Elem())
			case reflect.Map:
				values := value.MapRange()
				for values.Next() {
					if err := walk(values.Value()); err != nil {
						return err
					}
				}
			case reflect.Slice:
				for index := 0; index < value.Len(); index++ {
					if err := walk(value.Index(index)); err != nil {
						return err
					}
				}
			}
		case reflect.Array:
			for index := 0; index < value.Len(); index++ {
				if err := walk(value.Index(index)); err != nil {
					return err
				}
			}
		case reflect.Struct:
			if !value.CanInterface() {
				return nil
			}
			var category, feature string
			switch options := value.Interface().(type) {
			case option.Options:
				if len(options.Outbounds) == 0 && (options.Route == nil || options.Route.Final == "") && !slices.Contains(selected["outbounds"], "direct") {
					return errors.New("direct capability was not selected for this core")
				}
			case option.V2RayTransportOptions:
				category, feature = "transports", options.Type
			case option.OutboundUTLSOptions:
				if options.Enabled {
					category, feature = "tls", "utls"
				}
			case option.InboundRealityOptions:
				if options.Enabled {
					category, feature = "tls", "reality"
				}
			case option.OutboundRealityOptions:
				if options.Enabled {
					category, feature = "tls", "reality"
				}
			case option.InboundECHOptions:
				if options.Enabled {
					category, feature = "tls", "ech"
				}
			case option.OutboundECHOptions:
				if options.Enabled {
					category, feature = "tls", "ech"
				}
			case option.ClashAPIOptions:
				category, feature = "api", "clash_api"
			case option.V2RayAPIOptions:
				category, feature = "api", "v2ray_api"
			}
			if category != "" && !slices.Contains(selected[category], feature) {
				return errors.New("capability was not selected for this core")
			}
			for index := 0; index < value.NumField(); index++ {
				if value.Type().Field(index).IsExported() {
					if err := walk(value.Field(index)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return walk(reflect.ValueOf(options))
}
