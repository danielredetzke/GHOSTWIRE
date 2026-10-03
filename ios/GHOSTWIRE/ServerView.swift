import SwiftUI

struct ServerView: View {
    @Environment(AppSession.self) private var session
    @State private var original: ServerConfig?
    @State private var draft: ServerConfig?
    @State private var checks: [HealthCheck] = []
    @State private var error: String?
    @State private var busy = false
    @State private var detected: String?
    @State private var confirmRotate = false

    /// Fields that can be changed, with the label shown in the apply bar.
    private static let fields: [(String, String)] = [
        ("listenPort", "Listen port"), ("mtu", "MTU"), ("ipv4", "IPv4 network"), ("ipv6", "IPv6 network"),
        ("ipv6Enabled", "IPv6"), ("endpoint", "Endpoint host"), ("endpointPort", "Endpoint port"),
        ("uplinkV4", "IPv4 uplink"), ("uplinkV6", "IPv6 uplink"), ("nat", "NAT"), ("peerToPeer", "Peer-to-peer"),
        ("lanAccess", "LAN access"), ("openPort", "Open port"), ("clientDefaults", "Client defaults"),
    ]
    private static let disruptive: Set<String> = ["listenPort", "ipv4", "ipv6", "ipv6Enabled"]
    private static let quad9 = ["9.9.9.9", "149.112.112.112"]

    private func dict(_ c: ServerConfig) -> [String: Any] {
        guard let data = try? JSONEncoder().encode(c),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return [:] }
        return obj
    }

    private var changed: [String] {
        guard let original, let draft else { return [] }
        let a = dict(original), b = dict(draft)
        return Self.fields.map(\.0).filter { k in
            let x = try? JSONSerialization.data(withJSONObject: [a[k] ?? NSNull()], options: .sortedKeys)
            let y = try? JSONSerialization.data(withJSONObject: [b[k] ?? NSNull()], options: .sortedKeys)
            return x != y
        }
    }

    var body: some View {
        NavigationStack {
            Group {
                if draft != nil {
                    form
                } else if let error {
                    ScrollView { Notice(text: error, isError: true).padding(16) }.background(Color.gwGround)
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity).background(Color.gwGround)
                }
            }
            .navigationTitle("Server")
            .safeAreaInset(edge: .bottom) { applyBar }
            .refreshable { await load() }
            .task { await load() }
            .confirmationDialog("Rotate the server key?", isPresented: $confirmRotate, titleVisibility: .visible) {
                Button("Rotate key", role: .destructive) { Task { await rotate() } }
            } message: {
                Text("Every client config stops working until it is issued again. Use this only if the server key may have leaked.")
            }
        }
    }

    // Bindings into the draft; the form only shows once the draft exists.
    private func bind<T>(_ kp: WritableKeyPath<ServerConfig, T>) -> Binding<T> {
        Binding(get: { draft![keyPath: kp] }, set: { draft![keyPath: kp] = $0 })
    }

    private func listBind(_ kp: WritableKeyPath<ServerConfig, [String]>) -> Binding<String> {
        Binding(get: { draft![keyPath: kp].joined(separator: ", ") }, set: { draft![keyPath: kp] = splitList($0) })
    }

    private var form: some View {
        Form {
            Section("Health") {
                ForEach(checks, id: \.self) { c in
                    HStack(alignment: .firstTextBaseline, spacing: 10) {
                        Circle().fill(c.ok ? Color.gwGood : Color.gwBad).frame(width: 8, height: 8)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(c.name).font(.subheadline.weight(.medium))
                            Text(c.detail).font(.caption).foregroundStyle(Color.gwText2)
                        }
                    }
                    .accessibilityElement(children: .combine)
                    .accessibilityLabel((c.ok ? "OK: " : "Problem: ") + c.name + ", " + c.detail)
                }
            }

            Section {
                LabeledContent("Interface", value: draft!.interface)
                LabeledContent("Listen port") {
                    TextField("51820", value: bind(\.listenPort), format: .number.grouping(.never))
                        .keyboardType(.numberPad).multilineTextAlignment(.trailing)
                }
                LabeledContent("MTU") {
                    TextField("1420", value: bind(\.mtu), format: .number.grouping(.never))
                        .keyboardType(.numberPad).multilineTextAlignment(.trailing)
                }
                LabeledContent("IPv4 network") {
                    TextField("10.0.0.0/24", text: bind(\.ipv4)).font(.mono(.body)).multilineTextAlignment(.trailing)
                }
                LabeledContent("IPv6 network") {
                    TextField("fd00::/64", text: bind(\.ipv6)).font(.mono(.footnote)).multilineTextAlignment(.trailing)
                }
                Toggle("IPv6 in the tunnel", isOn: bind(\.ipv6Enabled))
            } header: {
                Text("Interface")
            } footer: {
                Text("Changing the port or the networks drops connected peers, and every device needs a new config.")
            }

            Section {
                TextField("vpn.example.net", text: bind(\.endpoint)).font(.mono(.body))
                Button("Detect public IP") { Task { await detect() } }
                LabeledContent("Port seen by clients") {
                    TextField(String(draft!.listenPort), value: bind(\.endpointPort), format: .number.grouping(.never))
                        .keyboardType(.numberPad).multilineTextAlignment(.trailing)
                }
            } header: {
                Text("Public endpoint")
            } footer: {
                Text(detected.map { "Detected public IP: \($0)" } ?? "Where clients connect. 0 for the port means the listen port.")
            }

            Section {
                Picker("DNS provider", selection: Binding(
                    get: { draft!.clientDefaults.dns == Self.quad9 ? "quad9" : "custom" },
                    set: { if $0 == "quad9" { draft!.clientDefaults.dns = Self.quad9 } }
                )) {
                    Text("Quad9").tag("quad9")
                    Text("Custom").tag("custom")
                }
                LabeledContent("DNS servers") {
                    TextField("9.9.9.9", text: listBind(\.clientDefaults.dns)).font(.mono(.footnote)).multilineTextAlignment(.trailing)
                }
                LabeledContent("AllowedIPs") {
                    TextField("0.0.0.0/0, ::/0", text: listBind(\.clientDefaults.allowedIPs)).font(.mono(.footnote)).multilineTextAlignment(.trailing)
                }
                LabeledContent("Keepalive (s)") {
                    TextField("0", value: bind(\.clientDefaults.keepalive), format: .number.grouping(.never))
                        .keyboardType(.numberPad).multilineTextAlignment(.trailing)
                }
            } header: {
                Text("Client defaults")
            } footer: {
                Text("For new configs and peers set to \"Server default\". Existing devices pick up changes after their config is issued again.")
            }

            Section {
                LabeledContent("IPv4 uplink") {
                    TextField("auto: \(draft!.detectedUplinkV4)", text: bind(\.uplinkV4)).font(.mono(.body)).multilineTextAlignment(.trailing)
                }
                LabeledContent("IPv6 uplink") {
                    TextField("auto: \(draft!.detectedUplinkV6)", text: bind(\.uplinkV6)).font(.mono(.body)).multilineTextAlignment(.trailing)
                }
                Toggle("NAT to the internet", isOn: bind(\.nat))
                Toggle("Peers reach each other", isOn: bind(\.peerToPeer))
                Toggle("Peers reach the server's LAN", isOn: bind(\.lanAccess))
                Toggle("Accept UDP \(String(draft!.listenPort)) in the input chain", isOn: bind(\.openPort))
            } header: {
                Text("Routing & firewall")
            } footer: {
                Text("Rules live in their own nftables table. If you also run ufw or firewalld, allow the port there.")
            }
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()

            Section {
                KV(key: "Public key", value: draft!.publicKey, mono: true)
                LabeledContent("Created", value: fmtDate(draft!.keyCreated))
                Button("Rotate server key…", role: .destructive) { confirmRotate = true }
            } header: {
                Text("Server key")
            }

            if let error {
                Section { Text(error).foregroundStyle(Color.gwErrInk) }
            }
        }
        .groundBackground()
        .textInputAutocapitalization(.never)
        .autocorrectionDisabled()
    }

    @ViewBuilder private var applyBar: some View {
        let c = changed
        if !c.isEmpty {
            let labels = Dictionary(uniqueKeysWithValues: Self.fields)
            VStack(alignment: .leading, spacing: 10) {
                Text("\(c.count) unsaved change\(c.count > 1 ? "s" : ""): " + c.compactMap { labels[$0] }.joined(separator: ", "))
                    .font(.footnote)
                if c.contains(where: Self.disruptive.contains) {
                    Text("Connected peers drop and need new configs.").font(.footnote.weight(.semibold))
                }
                HStack(spacing: 10) {
                    Button("Discard") { draft = original }
                        .buttonStyle(SecondaryButtonStyle())
                    Button("Apply") { Task { await apply(c) } }
                        .buttonStyle(PrimaryButtonStyle())
                        .disabled(busy)
                }
            }
            .padding(14)
            .background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: 14))
            .padding(.horizontal, 12)
            .padding(.bottom, 6)
        }
    }

    private func load() async {
        guard let api = session.api else { return }
        do {
            async let s: ServerConfig = api.get("/server")
            async let st: Status = api.get("/status")
            let (server, status) = try await (s, st)
            original = server
            draft = server
            checks = status.checks
            error = nil
        } catch {
            self.error = session.message(for: error)
        }
    }

    private func apply(_ keys: [String]) async {
        guard let api = session.api, let draft else { return }
        busy = true
        defer { busy = false }
        let d = dict(draft)
        var body: [String: Any?] = [:]
        for k in keys { body[k] = d[k] }
        do {
            let r: ServerResult = try await api.send("PATCH", "/server", body)
            original = r.server
            self.draft = r.server
            error = nil
            session.reportApply(r.applyError)
            if r.reissueNeeded == true && r.applyError.isEmpty {
                session.alert = "Applied. Existing devices need a new config: the endpoint, port or addresses changed."
            }
            if let st: Status = try? await api.get("/status") { checks = st.checks }
        } catch {
            self.error = session.message(for: error)
        }
    }

    private func detect() async {
        guard let api = session.api else { return }
        do {
            let r: DetectedIP = try await api.get("/server/detect-ip")
            detected = r.ip
            if draft?.endpoint.isEmpty == true { draft?.endpoint = r.ip }
        } catch {
            session.alert = session.message(for: error)
        }
    }

    private func rotate() async {
        guard let api = session.api else { return }
        do {
            let r: ServerResult = try await api.send("POST", "/server/rotate-key")
            original = r.server
            draft = r.server
            session.reportApply(r.applyError)
        } catch {
            session.alert = session.message(for: error)
        }
    }
}
