// SPDX-License-Identifier: GPL-3.0-or-later
import Darwin
import Foundation
import Libbox
import Network

private struct SmokeFailure: Error, CustomStringConvertible {
    let description: String
    init(_ description: String) { self.description = description }
}

private func require(_ condition: @autoclosure () throws -> Bool, _ message: String) throws {
    if try condition() == false { throw SmokeFailure(message) }
}

/// The listener is bound only to loopback. A successful response proves that the
/// nonce request crossed the reserved destination's utun route and Go's override.
private final class HTTPFixture {
    let payload: Data
    private let nonce: String
    private let listener: NWListener
    private let queue = DispatchQueue(label: "dev.idy.sing_box_flutter.ci-http")
    private var connections: [NWConnection] = []

    init(nonce: String) throws {
        self.nonce = nonce
        payload = Data("sing-box-ci-user-tun:\(nonce)\n".utf8)
        let parameters = NWParameters.tcp
        parameters.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: .any)
        listener = try NWListener(using: parameters)
    }

    func start() throws -> UInt16 {
        let ready = DispatchSemaphore(value: 0)
        listener.stateUpdateHandler = { state in
            switch state {
            case .ready, .failed, .cancelled: ready.signal()
            default: break
            }
        }
        listener.newConnectionHandler = { [weak self] connection in
            guard let self else { connection.cancel(); return }
            self.connections.append(connection)
            connection.start(queue: self.queue)
            self.receive(connection, accumulated: Data())
        }
        listener.start(queue: queue)
        guard ready.wait(timeout: .now() + 5) == .success, let port = listener.port else {
            throw SmokeFailure("Loopback HTTP fixture did not become ready within five seconds.")
        }
        return port.rawValue
    }

    private func receive(_ connection: NWConnection, accumulated: Data) {
        connection.receive(minimumIncompleteLength: 1, maximumLength: 4096) { [weak self] data, _, complete, error in
            guard let self else { connection.cancel(); return }
            var request = accumulated
            if let data { request.append(data) }
            guard request.count <= 8192, error == nil else { connection.cancel(); return }
            if request.range(of: Data("\r\n\r\n".utf8)) != nil {
                guard String(data: request, encoding: .utf8)?.hasPrefix("GET /\(self.nonce) HTTP/1.1\r\n") == true else {
                    connection.cancel()
                    return
                }
                var response = Data("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: \(self.payload.count)\r\nConnection: close\r\n\r\n".utf8)
                response.append(self.payload)
                connection.send(content: response, completion: .contentProcessed { _ in connection.cancel() })
            } else if complete {
                connection.cancel()
            } else {
                self.receive(connection, accumulated: request)
            }
        }
    }

    func close() {
        queue.sync {
            listener.cancel()
            for connection in connections { connection.cancel() }
            connections.removeAll()
        }
    }
}

private final class HTTPResult {
    private let lock = NSLock()
    private var value: Result<Data, Error>?
    func set(_ result: Result<Data, Error>) { lock.lock(); value = result; lock.unlock() }
    func get() throws -> Data {
        lock.lock()
        defer { lock.unlock() }
        guard let value else { throw SmokeFailure("HTTP request did not complete.") }
        return try value.get()
    }
}

private func fetch(_ url: URL) throws -> Data {
    let configuration = URLSessionConfiguration.ephemeral
    configuration.connectionProxyDictionary = [:]
    configuration.urlCache = nil
    configuration.httpCookieStorage = nil
    configuration.urlCredentialStorage = nil
    configuration.timeoutIntervalForRequest = 8
    configuration.timeoutIntervalForResource = 10
    let session = URLSession(configuration: configuration)
    defer { session.invalidateAndCancel() }
    let done = DispatchSemaphore(value: 0)
    let result = HTTPResult()
    let task = session.dataTask(with: url) { data, response, error in
        defer { done.signal() }
        if let error { result.set(.failure(error)); return }
        guard let response = response as? HTTPURLResponse, response.statusCode == 200, let data else {
            result.set(.failure(SmokeFailure("Reserved-address HTTP request did not return status 200.")))
            return
        }
        result.set(.success(data))
    }
    task.resume()
    guard done.wait(timeout: .now() + 12) == .success else {
        task.cancel()
        throw SmokeFailure("Reserved-address HTTP request timed out.")
    }
    return try result.get()
}

/// Read-only route queries. Refuse existing non-default routes before native TUN
/// startup, since the pinned sing-tun replaces a conflicting route on EEXIST.
private func route(_ destination: String) throws -> [String: String] {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/sbin/route")
    process.arguments = ["-n", "get", "-inet", destination]
    process.environment = ["PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL": "C"]
    let output = Pipe()
    process.standardOutput = output
    process.standardError = FileHandle.nullDevice
    let done = DispatchSemaphore(value: 0)
    process.terminationHandler = { _ in done.signal() }
    try process.run()
    guard done.wait(timeout: .now() + 3) == .success else {
        process.terminate()
        throw SmokeFailure("Read-only route query timed out.")
    }
    try require(process.terminationStatus == 0, "Cannot inspect the runner's IPv4 routes.")
    let text = String(decoding: output.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
    var fields: [String: String] = [:]
    for line in text.split(separator: "\n") {
        let parts = line.split(separator: ":", maxSplits: 1)
        if parts.count == 2 {
            fields[parts[0].trimmingCharacters(in: .whitespaces)] = parts[1].trimmingCharacters(in: .whitespaces)
        }
    }
    try require(fields["interface"] != nil && fields["destination"] != nil, "Route query returned incomplete ownership information.")
    return fields
}

private func requireDefault(_ fields: [String: String]) throws {
    try require(fields["destination"] == "default" || fields["destination"] == "0.0.0.0", "Reserved benchmark addresses already have a non-default route; refusing to replace it.")
}

private func waitForInterface(_ name: String, present: Bool) throws {
    let deadline = Date().addingTimeInterval(5)
    while (if_nametoindex(name) != 0) != present {
        try require(Date() < deadline, present ? "The requested utun interface did not appear." : "The smoke's utun interface survived service close.")
        Thread.sleep(forTimeInterval: 0.05)
    }
}

private final class Teardown {
    let service: NativeUserTunService
    let fixture: HTTPFixture
    init(service: NativeUserTunService, fixture: HTTPFixture) { self.service = service; self.fixture = fixture }
    func close() throws {
        // Closing Go first drains native activation and removes only its routes.
        defer { fixture.close() }
        try service.close()
    }
}

@main
struct CITunTrafficSmoke {
    private static let destination = "198.18.20.1"
    private static let address = "198.18.10.1"

    private static func config(interface: String, port: UInt16) -> String {
        // The pinned option/rule_action.go places both override fields directly
        // on the route action. Explicit inert UDP avoids implicit local DNS;
        // system DNS, platform proxy and default routes remain unchanged.
        """
        {
          "log": {"disabled": true},
          "dns": {"servers": [{"type": "udp", "tag": "ci-dns", "server": "127.0.0.1", "server_port": 9}], "final": "ci-dns"},
          "inbounds": [{
            "type": "tun", "tag": "ci-tun", "interface_name": "\(interface)",
            "address": ["\(address)/30"], "mtu": 1500, "stack": "system",
            "auto_route": true, "route_address": ["\(destination)/32"], "dns_mode": "disabled"
          }],
          "outbounds": [{"type": "direct", "tag": "fixture", "bind_interface": "lo0"}],
          "route": {
            "auto_detect_interface": true, "final": "fixture",
            "rules": [
              {"inbound": ["ci-tun"], "network": "tcp", "ip_cidr": ["\(destination)/32"],
               "action": "route", "outbound": "fixture", "override_address": "127.0.0.1", "override_port": \(port)},
              {"action": "reject"}
            ]
          }
        }
        """
    }

    static func main() {
        do { try run() }
        catch {
            fputs("CI user_tun traffic smoke failed: \(error)\n", stderr)
            exit(1)
        }
    }

    private static func run() throws {
        let arguments = Array(CommandLine.arguments.dropFirst())
        let validationOnly = arguments == ["--validate-only"]
        if !validationOnly {
            let environment = ProcessInfo.processInfo.environment
            try require(arguments == ["--ci-tun"] && environment["SINGBOX_CI_TUN_SMOKE"] == "1", "Real TUN traffic requires the explicit CI opt-in and --ci-tun.")
            try require(environment["GITHUB_ACTIONS"] == "true" && environment["RUNNER_OS"] == "macOS", "Real TUN traffic is restricted to GitHub Actions macOS runners.")
            try require(geteuid() == 0, "Real TUN traffic requires root in the isolated CI executable.")
        }
        try require(ProcessInfo.processInfo.environment["TS_PERMIT_TOOLCHAIN_MISMATCH"] != "1", "Do not bypass the pinned Go toolchain assertion.")
        let capabilities = try CoreCapabilities.load()
        try NativeCore.verify(capabilities)
        try require(capabilities.supports("inbounds", "tun") && capabilities.supports("outbounds", "direct") && capabilities.supports("dns", "udp"), "Selected core must contain tun, direct and UDP DNS.")
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent("sing-box-ci-tun-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: directory) }
        try NativeCore.setup(directory: directory, crashReportSource: "CI macOS user_tun traffic smoke")

        if validationOnly {
            // Preparation constructs/validates the engine without starting the
            // monitor, opening utun, binding the fixture or changing host state.
            let service = try NativeUserTunService()
            defer { try? service.close() }
            let content = config(interface: "utun500", port: 32123)
            try NativeCore.check(content)
            let plan = try service.prepare(content)
            try require(plan.hasTun && plan.servers.isEmpty, "Validation must prepare TUN with system DNS disabled.")
            print("CI traffic configuration parsed and prepared; no interface, route, DNS or fixture was started.")
            return
        }

        let beforeDefault = try route("default")
        for target in [destination, address, "198.18.10.2", "198.18.10.3"] { try requireDefault(try route(target)) }
        let candidates = (200..<1000).map { "utun\($0)" }
        guard let interface = candidates.first(where: { name in name.withCString { if_nametoindex($0) == 0 } }) else {
            throw SmokeFailure("No unused high-numbered utun name is available.")
        }
        let nonce = UUID().uuidString
        let fixture = try HTTPFixture(nonce: nonce)
        defer { fixture.close() }
        let port = try fixture.start()
        let service = try NativeUserTunService()
        let teardown = Teardown(service: service, fixture: fixture)

        // Native prepare/activate/close may block. A bounded watchdog and signal
        // handlers attempt normal teardown; a final process exit closes kernel
        // descriptors even if Go teardown hangs. No route-delete shell commands.
        let watchdogQueue = DispatchQueue(label: "dev.idy.sing_box_flutter.ci-watchdog", attributes: .concurrent)
        let watchdog = DispatchWorkItem {
            fputs("CI user_tun traffic smoke exceeded its 60-second deadline; closing service.\n", stderr)
            watchdogQueue.asyncAfter(deadline: .now() + 8) { _exit(1) }
            try? teardown.close()
            _exit(1)
        }
        watchdogQueue.asyncAfter(deadline: .now() + 60, execute: watchdog)
        var signals: [DispatchSourceSignal] = []
        for number in [SIGINT, SIGTERM] {
            signal(number, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: number, queue: watchdogQueue)
            source.setEventHandler {
                watchdogQueue.asyncAfter(deadline: .now() + 8) { _exit(1) }
                try? teardown.close()
                _exit(1)
            }
            source.resume()
            signals.append(source)
        }
        defer { watchdog.cancel(); for source in signals { source.cancel() } }

        var failure: Error?
        do {
            let content = config(interface: interface, port: port)
            try NativeCore.check(content)
            let plan = try service.prepare(content)
            try require(plan.hasTun && plan.servers.isEmpty, "Traffic smoke must prepare TUN with system DNS disabled.")
            // Recheck immediately before activation so an existing route is never
            // intentionally replaced, including one appearing during preparation.
            try require(if_nametoindex(interface) == 0, "Chosen utun name became occupied.")
            try requireDefault(try route(destination))
            try service.activate { fputs("Unexpected DNS failure in DNS-disabled CI smoke.\n", stderr) }
            try waitForInterface(interface, present: true)
            try require(try route(destination)["interface"] == interface, "Reserved /32 is not routed through the smoke's utun.")
            let activeDefault = try route("default")
            try require(activeDefault["gateway"] == beforeDefault["gateway"] && activeDefault["interface"] == beforeDefault["interface"], "Default IPv4 route changed during the smoke.")
            let data = try fetch(URL(string: "http://\(destination):\(port)/\(nonce)")!)
            try require(data == fixture.payload, "TUN traffic did not return the exact loopback fixture nonce payload.")
            print("Real user_tun HTTP traffic passed through \(interface): reserved /32 → pinned route override → loopback fixture.")
        } catch { failure = error }
        // Keep the watchdog armed until teardown and cleanup assertions finish.
        do {
            try teardown.close()
            try waitForInterface(interface, present: false)
            try requireDefault(try route(destination))
            let afterDefault = try route("default")
            try require(afterDefault["gateway"] == beforeDefault["gateway"] && afterDefault["interface"] == beforeDefault["interface"], "Default IPv4 route changed after teardown.")
        } catch {
            throw SmokeFailure("Service cleanup failed: \(error); traffic result: \(String(describing: failure))")
        }
        if let failure { throw failure }
        print("CI user_tun cleanup passed: owned utun and /32 route removed, default route preserved; system DNS/proxy configuration was never installed.")
    }
}
