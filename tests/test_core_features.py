"""Selection/registry tests; no SDK, Go compilation, network, or YAML dependency."""
import json
from pathlib import Path
import sys
import unittest

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tool"))
from core_features import (CATEGORIES, FeatureError, canonicalize, generate_registry,
                           resolve_features, selection_hash)


class SelectionTests(unittest.TestCase):
    def test_defaults_and_empty_lists(self):
        self.assertEqual(canonicalize({})["dns"], ["local", "tcp", "udp"])
        features = canonicalize({"inbounds": [], "dns": []})
        self.assertEqual(features["inbounds"], [])
        self.assertEqual(features["dns"], [])
        self.assertEqual(features["outbounds"], ["direct"])
        self.assertEqual(set(features), set(CATEGORIES))

    def test_invalid_input_never_falls_back(self):
        for selection in [None, [], {"unknown": []}, {"dns": None},
                          {"dns": "udp"}, {"dns": [1]},
                          {"dns": ["udp", "udp"]}, {"dns": ["udpp"]}]:
            with self.subTest(selection=selection), self.assertRaises(FeatureError):
                canonicalize(selection)

    def test_canonical_hash_is_order_independent(self):
        first = {"dns": ["udp", "tcp"], "endpoints": ["wireguard", "wgc"]}
        second = {"endpoints": ["wgc", "wireguard"], "dns": ["tcp", "udp"]}
        self.assertEqual(selection_hash(first), selection_hash(second))
        self.assertEqual(generate_registry(first), generate_registry(second))

    def test_mobile_exclusions(self):
        for target in ["android", "apple"]:
            for category, feature in [("inbounds", "redirect"), ("inbounds", "tproxy"),
                                      ("dns", "resolved"), ("services", "resolved"),
                                      ("outbounds", "bridge"), ("outbounds", "tor"),
                                      ("services", "usbip-client"), ("services", "usbip-server")]:
                with self.subTest(target=target, feature=feature), self.assertRaisesRegex(FeatureError, "unsupported"):
                    resolve_features({category: [feature]}, target)
        with self.assertRaisesRegex(FeatureError, "Cronet"):
            resolve_features({"outbounds": ["naive"]}, "apple")
        self.assertIn("with_naive_outbound", resolve_features({"outbounds": ["naive"]}, "android")["tags"])

    def test_quic_and_utls_dependencies(self):
        for category, name in [("outbounds", "hysteria"), ("inbounds", "hysteria2"),
                               ("outbounds", "tuic"), ("dns", "quic"), ("dns", "h3"),
                               ("transports", "quic"), ("services", "hysteria-realm")]:
            with self.subTest(feature=name):
                self.assertIn("with_quic", resolve_features({category: [name]}, "android")["tags"])
        naive = resolve_features({"inbounds": ["naive"]}, "apple")
        self.assertIn("with_quic", naive["tags"])
        self.assertIn('_ "github.com/sagernet/sing-box/protocol/naive/quic"', generate_registry(naive["features"]))
        self.assertNotIn("naive/quic", generate_registry({"outbounds": ["direct"]}))
        selection = {"tls": ["reality"]}
        resolved = resolve_features(selection, "android")
        self.assertEqual(selection, {"tls": ["reality"]})
        self.assertEqual(resolved["requested_features"]["tls"], ["reality"])
        self.assertEqual(resolved["features"]["tls"], ["reality", "utls"])
        self.assertIn("with_utls", resolved["tags"])
        generated = generate_registry(resolved["features"])
        encoded = generated.split('const selectedFeaturesJSON = `', 1)[1].split('`', 1)[0]
        self.assertEqual(json.loads(encoded), resolved["features"])
        self.assertEqual(encoded, json.dumps(resolved["features"], sort_keys=True, separators=(",", ":")))
        self.assertEqual(resolved["tags"], sorted(set(resolved["tags"])))

    def test_minimal_wgc_imports(self):
        resolved = resolve_features({"endpoints": ["wgc"], "dns": ["tcp", "udp"]}, "android")
        generated = generate_registry(resolved["features"])
        self.assertIn("with_gvisor", resolved["tags"])
        self.assertNotIn("with_wireguard", resolved["tags"])
        self.assertNotIn("with_quic", resolved["tags"])
        imports = {line.split('"')[1] for line in generated.splitlines() if line.startswith('\t') and '"' in line}
        self.assertEqual(imports, {
            "context", "github.com/sagernet/sing-box",
            "github.com/sagernet/sing-box/adapter/inbound", "github.com/sagernet/sing-box/adapter/outbound",
            "github.com/sagernet/sing-box/adapter/endpoint", "github.com/sagernet/sing-box/adapter/service",
            "github.com/sagernet/sing-box/adapter/certificate", "github.com/sagernet/sing-box/dns",
            "github.com/sagernet/sing-box/dns/transport", "github.com/sagernet/sing-box/protocol/tun",
            "github.com/sagernet/sing-box/protocol/direct", "github.com/idy/sing-box-flutter/native/wgc",
            "github.com/sagernet/sing/service/filemanager"})
        self.assertNotIn("/include", generated)
        self.assertNotIn("grpc", generated)
        self.assertNotIn("command", generated)
        self.assertIn("filemanager.WithDefault", generated)
        self.assertIn("return box.Context(", generated)

    def test_tailscale_dns_does_not_expose_endpoint(self):
        resolved = resolve_features({"dns": ["tailscale"], "certificate_providers": ["tailscale"]}, "apple")
        generated = generate_registry(resolved["features"])
        self.assertIn("with_tailscale", resolved["tags"])
        self.assertIn("ts_omit_ssh", resolved["tags"])
        self.assertIn("p_tailscale.RegistryTransport(dnsRegistry)", generated)
        self.assertIn("p_tailscale.RegisterCertificateProvider(certificateRegistry)", generated)
        self.assertNotIn("RegisterEndpoint(endpointRegistry)", generated)
        self.assertNotIn("service/derp", generated)

    def test_local_override_and_extensions(self):
        self.assertIn("newPlatformTransport", generate_registry({}))
        self.assertNotIn("newPlatformTransport", generate_registry({"dns": []}))
        generated = generate_registry({"dns": ["local"], "endpoints": ["wgc"]}, [{"package": "example.org/outer"}])
        self.assertIn('dns_local.RegisterTransport(dnsRegistry)', generated)
        self.assertIn('newPlatformTransport(ctx, logger, localTransport, tag, options)', generated)
        self.assertIn('_ "example.org/outer"', generated)

    def test_openvpn_constructor_selection(self):
        for name, kind in [("openvpn-client", "Client"), ("openvpn-server", "Server")]:
            resolved = resolve_features({"endpoints": [name]}, "apple")
            self.assertIn("with_openvpn", resolved["tags"])
            self.assertIn("with_gvisor", resolved["tags"])
            generated = generate_registry(resolved["features"])
            self.assertIn(f"p_openvpn.New{kind}Endpoint", generated)
            other = "Server" if kind == "Client" else "Client"
            self.assertNotIn(f"New{other}Endpoint", generated)
        resolved = resolve_features({"endpoints": ["openconnect"], "dns": ["openconnect", "openvpn"]}, "android")
        self.assertIn("with_openconnect", resolved["tags"])
        self.assertIn("with_openvpn", resolved["tags"])
        generated = generate_registry(resolved["features"])
        self.assertIn('p_openconnect "github.com/sagernet/sing-box/protocol/openconnect"', generated)
        self.assertIn("p_openconnect.RegisterEndpoint(endpointRegistry)", generated)
        self.assertIn("p_openconnect.RegisterDNSTransport(dnsRegistry)", generated)
        self.assertIn("p_openvpn.RegisterDNSTransport(dnsRegistry)", generated)
        self.assertNotIn("p_openvpn.NewClientEndpoint", generated)
        self.assertNotIn("p_openvpn.NewServerEndpoint", generated)

    def test_catalogue_paths_for_platform_rejected_components(self):
        generated = generate_registry({"dns": ["resolved"], "services": ["usbip-client"]})
        self.assertIn('"github.com/sagernet/sing-box/service/resolved"', generated)
        self.assertIn('"github.com/sagernet/sing-box/service/usbip"', generated)
        self.assertNotIn("dns/transport/resolved", generated)
        self.assertNotIn("service/usbip-client", generated)
        self.assertIn("s_usbip.NewClientService", generated)
        self.assertNotIn("s_usbip.NewServerService", generated)


if __name__ == "__main__":
    unittest.main()
