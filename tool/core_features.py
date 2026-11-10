"""Strict host core selections and constructor-only registries for the pinned core.

Transport and TLS choices describe supported capabilities; shared upstream packages
can retain implementation code for other capabilities in the same package.
"""
from __future__ import annotations

import hashlib
import json
from native_common import TS_OMIT_TAGS

CATEGORIES = {
    "inbounds": "tun direct socks http mixed shadowsocks snell vmess vless trojan naive shadowtls anytls hysteria hysteria2 tuic cloudflared redirect tproxy".split(),
    "outbounds": "direct bridge block selector urltest socks http shadowsocks snell vmess vless trojan naive tor ssh shadowtls anytls hysteria hysteria2 tuic".split(),
    "endpoints": "wireguard tailscale openconnect openvpn-client openvpn-server wgc".split(),
    "dns": "udp tcp tls https quic h3 local hosts fakeip mdns dhcp resolved tailscale openconnect openvpn".split(),
    "transports": "http ws quic grpc httpupgrade".split(),
    "tls": "utls reality ech".split(),
    "api": "clash_api v2ray_api".split(),
    "services": "api resolved ssm-api hysteria-realm derp ccm ocm oom-killer usbip-server usbip-client".split(),
    "certificate_providers": "acme tailscale cloudflare-origin-ca".split(),
}
DEFAULTS = {"inbounds": ["tun"], "outbounds": ["direct"], "dns": ["local", "tcp", "udp"]}


class FeatureError(ValueError):
    """A selection cannot be represented by this pinned mobile core."""


def canonicalize(selection: dict) -> dict:
    if not isinstance(selection, dict):
        raise FeatureError("sing_box_flutter.core must be an object of feature lists")
    unknown = set(selection) - set(CATEGORIES)
    if unknown:
        raise FeatureError(f"Unknown core categories: {', '.join(sorted(map(str, unknown)))}")
    result = {}
    for category in sorted(CATEGORIES):
        values = selection.get(category, DEFAULTS.get(category, []))
        if not isinstance(values, list) or any(not isinstance(value, str) for value in values):
            raise FeatureError(f"core.{category} must be a list of feature names")
        if len(values) != len(set(values)):
            raise FeatureError(f"Duplicate feature in core.{category}")
        unknown_features = set(values) - set(CATEGORIES[category])
        if unknown_features:
            raise FeatureError(f"Unknown core.{category} features: {', '.join(sorted(unknown_features))}")
        result[category] = sorted(values)
    return result


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise FeatureError(f"Duplicate JSON key: {key}")
        result[key] = value
    return result


def selection_hash(selection: dict) -> str:
    canonical = json.dumps(canonicalize(selection), sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(canonical.encode()).hexdigest()


def resolve_features(selection: dict, target: str) -> dict:
    requested = canonicalize(selection)
    if target not in {"android", "apple", "ios", "macos", "linux"}:
        raise FeatureError(f"Unsupported core target: {target}")
    unsupported = {
        "inbounds": {"redirect", "tproxy"},
        "outbounds": {"bridge", "tor"},
        "dns": {"resolved"},
        "services": {"resolved", "usbip-server", "usbip-client"},
    }
    if target == "linux":
        unsupported["inbounds"].clear()
        unsupported["dns"].clear()
        unsupported["services"].remove("resolved")
    if target in {"apple", "ios", "macos", "linux"}:
        unsupported["outbounds"].add("naive")
    reasons = {
        "redirect": "Linux transparent interception is unavailable to the mobile VPN host",
        "tproxy": "Linux transparent interception is unavailable to the mobile VPN host",
        "resolved": "systemd-resolved integration requires a Linux system service",
        "bridge": "the host does not implement native bridge facilities",
        "tor": "the host does not provide the Tor helper executable/process integration",
        "usbip-server": "the host does not implement USB/IP device access or native bridges",
        "usbip-client": "the host does not implement USB/IP device access or native bridges",
        "naive": ("Linux source builds use CGO_ENABLED=0 and do not bundle the Cronet native library"
                  if target == "linux" else "Apple Naive outbound requires an incompatible Cronet linker setup"),
    }
    for category, denied in unsupported.items():
        invalid = denied.intersection(requested[category])
        if invalid:
            detail = "; ".join(f"{name}: {reasons[name]}" for name in sorted(invalid))
            raise FeatureError(f"core.{category} unsupported on {target}: {detail}")
    features = {key: list(value) for key, value in requested.items()}
    dependencies = []
    if "reality" in features["tls"] and "utls" not in features["tls"]:
        features["tls"].append("utls")
        features["tls"].sort()
        dependencies.append("tls.reality requires tls.utls")
    tags = {"badlinkname", "tfogo_checklinkname0"}
    if "tun" in features["inbounds"] or set(features["endpoints"]) & {"wgc", "wireguard", "tailscale", "openconnect", "openvpn-client", "openvpn-server"} or "derp" in features["services"]:
        tags.add("with_gvisor")
    if "wireguard" in features["endpoints"]:
        tags.add("with_wireguard")
    if any("tailscale" in features[category] for category in ("endpoints", "dns", "certificate_providers")) or "derp" in features["services"]:
        tags.update(["with_tailscale", *TS_OMIT_TAGS])
    if any(set(features[category]) & {"hysteria", "hysteria2", "tuic"} for category in ("inbounds", "outbounds")) or set(features["dns"]) & {"quic", "h3"} or "quic" in features["transports"] or "hysteria-realm" in features["services"] or "naive" in features["inbounds"]:
        tags.add("with_quic")
    for protocol in ("openconnect", "openvpn"):
        if any(name.startswith(protocol) for name in features["endpoints"]) or protocol in features["dns"]:
            tags.update(["with_" + protocol, "with_gvisor"])
    if "utls" in features["tls"]:
        tags.add("with_utls")
    for category, name, tag in [
        ("inbounds", "cloudflared", "with_cloudflared"),
        ("outbounds", "naive", "with_naive_outbound"),
        ("dns", "dhcp", "with_dhcp"),
        ("certificate_providers", "acme", "with_acme"),
        ("services", "ccm", "with_ccm"), ("services", "ocm", "with_ocm"),
        ("api", "clash_api", "with_clash_api"), ("api", "v2ray_api", "with_v2ray_api"),
    ]:
        if name in features[category]:
            tags.add(tag)
    if target in {"apple", "ios"}:
        tags.update(["with_low_memory", "grpcnotrace"])
    return {"requested_features": requested, "features": features, "tags": sorted(tags),
            "dependencies": sorted(dependencies), "selection_hash": selection_hash(requested)}


def generate_registry(features: dict, extensions: list[dict] | None = None) -> str:
    """Emit libbox's context, registering only selected component constructors.

    Extension records are validated by wgc_overlay.load_extensions; their package
    imports install WGC transport factories via init, without extra endpoints.
    """
    features = canonicalize(features)
    prefix = "github.com/sagernet/sing-box/"
    imports = {
        "context": "", "github.com/sagernet/sing-box": "box",
        prefix + "adapter/inbound": "inbound", prefix + "adapter/outbound": "outbound",
        prefix + "adapter/endpoint": "endpoint", prefix + "dns": "dns",
        prefix + "adapter/service": "boxservice", prefix + "adapter/certificate": "certificate",
        "github.com/sagernet/sing/service/filemanager": "filemanager",
    }
    lines = ["\tinboundRegistry := inbound.NewRegistry()", "\toutboundRegistry := outbound.NewRegistry()",
             "\tendpointRegistry := endpoint.NewRegistry()", "\tdnsRegistry := dns.NewTransportRegistry()",
             "\tserviceRegistry := boxservice.NewRegistry()", "\tcertificateRegistry := certificate.NewRegistry()"]

    def register(path, alias, function, registry):
        imports[prefix + path] = alias
        lines.append(f"\t{alias}.{function}({registry}Registry)")

    for category, kind in [("inbounds", "Inbound"), ("outbounds", "Outbound")]:
        for name in features[category]:
            package = {"selector": "group", "urltest": "group", "tproxy": "redirect", "cloudflared": "cloudflare"}.get(name, name)
            function = {"selector": "RegisterSelector", "urltest": "RegisterURLTest", "redirect": "RegisterRedirect", "tproxy": "RegisterTProxy"}.get(name, "Register" + kind)
            register("protocol/" + package, "p_" + package, function, kind.lower())
    for name in features["endpoints"]:
        if name == "wgc":
            imports["github.com/idy/sing-box-flutter/native/wgc"] = "wgc"
            lines.append("\twgc.RegisterEndpoint(endpointRegistry)")
        elif name.startswith("openvpn-"):
            imports[prefix + "protocol/openvpn"] = "p_openvpn"
            imports[prefix + "constant"] = "C"
            imports[prefix + "option"] = "option"
            kind = "Client" if name == "openvpn-client" else "Server"
            lines.append(f"\tendpoint.Register[option.OpenVPN{kind}EndpointOptions](endpointRegistry, C.TypeOpenVPN{kind}, p_openvpn.New{kind}Endpoint)")
        else:
            register("protocol/" + name, "p_" + name, "RegisterEndpoint", "endpoint")
    for name in features["dns"]:
        if name in {"tcp", "udp", "tls", "https"}:
            register("dns/transport", "dnstransport", "Register" + name.upper(), "dns")
        elif name in {"quic", "h3"}:
            register("dns/transport/quic", "dnsquic", "RegisterTransport" if name == "quic" else "RegisterHTTP3Transport", "dns")
        elif name in {"openconnect", "openvpn"}:
            register("protocol/" + name, "p_" + name, "RegisterDNSTransport", "dns")
        elif name == "tailscale":
            register("protocol/tailscale", "p_tailscale", "RegistryTransport", "dns")
        elif name == "resolved":
            register("service/resolved", "s_resolved", "RegisterTransport", "dns")
        else:
            register("dns/transport/" + name, "dns_" + name, "RegisterTransport", "dns")
    for name in features["services"]:
        if name in {"usbip-server", "usbip-client"}:
            imports[prefix + "service/usbip"] = "s_usbip"
            imports[prefix + "constant"] = "C"
            imports[prefix + "option"] = "option"
            kind = "Server" if name == "usbip-server" else "Client"
            lines.append(f"\tboxservice.Register[option.USBIP{kind}ServiceOptions](serviceRegistry, C.TypeUSBIP{kind}, s_usbip.New{kind}Service)")
            continue
        package = {"ssm-api": "ssmapi", "oom-killer": "oomkiller", "hysteria-realm": "hysteria2"}.get(name, name)
        path = ("protocol/" if name == "hysteria-realm" else "service/") + package
        function = {"derp": "Register", "hysteria-realm": "RegisterRealmService"}.get(name, "RegisterService")
        alias = "p_hysteria2" if name == "hysteria-realm" else "s_" + package
        register(path, alias, function, "service")
    for name in features["certificate_providers"]:
        path, alias = {"acme": ("service/acme", "acme"), "tailscale": ("protocol/tailscale", "p_tailscale"), "cloudflare-origin-ca": ("service/origin_ca", "originca")}[name]
        register(path, alias, "RegisterCertificateProvider", "certificate")
    for name in features["api"]:
        imports[prefix + "experimental/" + name.replace("_", "")] = "_"
    if "quic" in features["transports"]:
        imports[prefix + "transport/v2rayquic"] = "_"
    if "naive" in features["inbounds"]:
        # Naive's UDP listener calls this init-installed HTTP/3 constructor.
        imports[prefix + "protocol/naive/quic"] = "_"
    # Other V2Ray transports and TLS/ECH implementations are shared upstream;
    # there is no independent registration API for those capabilities at this pin.
    for extension in extensions or []:
        imports[extension["package"]] = "_"
    if "local" in features["dns"]:
        imports.update({prefix + "adapter": "adapter", prefix + "constant": "C",
                        prefix + "log": "log", prefix + "option": "option"})
        lines.extend([
            "\tif platformInterface != nil {",
            "\t\tif localTransport := platformInterface.LocalDNSTransport(); localTransport != nil {",
            "\t\t\tdns.RegisterTransport[option.LocalDNSServerOptions](dnsRegistry, C.DNSTypeLocal, func(ctx context.Context, logger log.ContextLogger, tag string, options option.LocalDNSServerOptions) (adapter.DNSTransport, error) {",
            "\t\t\t\treturn newPlatformTransport(ctx, logger, localTransport, tag, options)",
            "\t\t\t})", "\t\t}", "\t}"])
    lines.extend(["\tctx := filemanager.WithDefault(context.Background(), sWorkingPath, sTempPath, sUserID, sGroupID)",
                  "\treturn box.Context(ctx, inboundRegistry, outboundRegistry, endpointRegistry, dnsRegistry, serviceRegistry, certificateRegistry)"])
    import_lines = [f'\t{alias + " " if alias else ""}"{path}"' for path, alias in sorted(imports.items())]
    selected_json = json.dumps(features, sort_keys=True, separators=(",", ":"))
    return ("// Code generated by sing_box_flutter; DO NOT EDIT.\npackage libbox\n\nimport (\n"
            + "\n".join(import_lines) + "\n)\n\n"
            + "const selectedFeaturesJSON = `" + selected_json + "`\n\n"
            + "func baseContext(platformInterface PlatformInterface) context.Context {\n"
            + "\n".join(lines) + "\n}\n")
