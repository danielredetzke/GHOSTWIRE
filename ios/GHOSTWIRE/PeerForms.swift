import SwiftUI

/// The three per-peer overrides. Each is "server default" (sent as null) or
/// a value of its own.
struct PeerOverrides {
    enum Route: String { case serverDefault, vpnOnly, custom }
    enum Choice: String { case serverDefault, custom }
    enum Keepalive: String { case serverDefault, off, custom }

    var route: Route = .serverDefault
    var routeText = ""
    var dns: Choice = .serverDefault
    var dnsText = ""
    var keepalive: Keepalive = .serverDefault
    var keepaliveText = ""

    init() {}

    init(peer p: Peer, server s: ServerConfig) {
        if let a = p.allowedIPs {
            route = a == s.networks ? .vpnOnly : .custom
            routeText = a.joined(separator: ", ")
        }
        if let d = p.dns {
            dns = .custom
            dnsText = d.joined(separator: ", ")
        }
        if let k = p.keepalive {
            keepalive = k == 0 ? .off : .custom
            keepaliveText = k == 0 ? "" : String(k)
        }
    }

    func body(server s: ServerConfig) throws -> [String: Any?] {
        var out: [String: Any?] = [:]
        switch route {
        case .serverDefault: out["allowedIPs"] = nil as [String]?
        case .vpnOnly: out["allowedIPs"] = s.networks
        case .custom: out["allowedIPs"] = splitList(routeText)
        }
        out["dns"] = dns == .serverDefault ? nil as [String]? : splitList(dnsText)
        switch keepalive {
        case .serverDefault: out["keepalive"] = nil as Int?
        case .off: out["keepalive"] = 0
        case .custom:
            guard let k = Int(keepaliveText), k >= 0 else { throw APIError.server("Keepalive must be a number of seconds.") }
            out["keepalive"] = k
        }
        return out
    }
}

/// Form sections for the overrides, shared by Add and Edit.
struct OverrideSections: View {
    @Binding var o: PeerOverrides
    let server: ServerConfig

    var body: some View {
        let d = server.clientDefaults
        Section {
            Picker("Route", selection: $o.route) {
                Text("Server default · \(d.allowedIPs.joined(separator: ", "))").tag(PeerOverrides.Route.serverDefault)
                Text("Only the VPN network").tag(PeerOverrides.Route.vpnOnly)
                Text("Custom").tag(PeerOverrides.Route.custom)
            }
            .pickerStyle(.inline)
            .labelsHidden()
            if o.route == .custom {
                TextField("10.0.0.0/24, 192.168.1.0/24", text: $o.routeText).font(.mono(.footnote))
            }
        } header: {
            Text("Route through the VPN (AllowedIPs)")
        }
        Section("DNS") {
            Picker("DNS", selection: $o.dns) {
                Text("Server default · \(d.dns.isEmpty ? "none" : d.dns.joined(separator: ", "))").tag(PeerOverrides.Choice.serverDefault)
                Text("Custom").tag(PeerOverrides.Choice.custom)
            }
            .pickerStyle(.inline)
            .labelsHidden()
            if o.dns == .custom {
                TextField("9.9.9.9, 149.112.112.112", text: $o.dnsText).font(.mono(.footnote))
            }
        }
        Section {
            Picker("Keepalive", selection: $o.keepalive) {
                Text("Server default · \(d.keepalive > 0 ? "\(d.keepalive) s" : "off")").tag(PeerOverrides.Keepalive.serverDefault)
                Text("Off").tag(PeerOverrides.Keepalive.off)
                Text("Custom").tag(PeerOverrides.Keepalive.custom)
            }
            if o.keepalive == .custom {
                TextField("Seconds", text: $o.keepaliveText).keyboardType(.numberPad)
            }
        } header: {
            Text("Persistent keepalive")
        } footer: {
            Text("Keeps the tunnel open behind NAT.")
        }
        .textInputAutocapitalization(.never)
        .autocorrectionDisabled()
    }
}

struct AddPeerView: View {
    @Environment(AppSession.self) private var session
    @Environment(\.dismiss) private var dismiss
    @State private var server: ServerConfig?
    @State private var name = ""
    @State private var note = ""
    @State private var ipv4 = ""
    @State private var overrides = PeerOverrides()
    @State private var psk = true
    @State private var error: String?
    @State private var busy = false
    @State private var issued: IssuedConfig?

    var body: some View {
        NavigationStack {
            Group {
                if let issued {
                    IssuedConfigContent(issued: issued)
                } else if let server {
                    form(server)
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity).background(Color.gwGround)
                }
            }
            .navigationTitle(issued == nil ? "Add peer" : "Config for \(issued?.peer.name ?? "")")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                if issued == nil {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Create") { Task { await create() } }.disabled(busy || name.isEmpty || server == nil)
                    }
                } else {
                    ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
                }
            }
            .task {
                guard let api = session.api else { return }
                do { server = try await api.get("/server") } catch { self.error = session.message(for: error) }
            }
        }
        .interactiveDismissDisabled(issued != nil)
    }

    private func form(_ server: ServerConfig) -> some View {
        Form {
            Section {
                TextField("Name", text: $name)
                TextField("Note (optional)", text: $note)
                TextField("IPv4 address (next free if empty)", text: $ipv4)
                    .font(.mono(.body))
                    .keyboardType(.numbersAndPunctuation)
            } footer: {
                Text("Names: letters, numbers and . _ @ - · max 32 · unique. Network \(server.ipv4).")
            }
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()

            OverrideSections(o: $overrides, server: server)

            Section {
                Toggle("Add a preshared key", isOn: $psk)
            } header: {
                Text("Keys")
            } footer: {
                Text("The private key appears once in the config and QR code. It isn't stored.")
            }

            if let error {
                Section { Text(error).foregroundStyle(Color.gwErrInk) }
            }
        }
        .groundBackground()
    }

    private func create() async {
        guard let api = session.api, let server else { return }
        busy = true
        defer { busy = false }
        error = nil
        do {
            var body = try overrides.body(server: server)
            body["name"] = name.trimmingCharacters(in: .whitespaces)
            body["note"] = note.trimmingCharacters(in: .whitespaces)
            body["ipv4"] = ipv4.trimmingCharacters(in: .whitespaces)
            body["presharedKey"] = psk
            let r: IssuedConfig = try await api.send("POST", "/peers", body)
            session.reportApply(r.applyError)
            issued = r
        } catch {
            self.error = session.message(for: error)
        }
    }
}

struct PeerEditView: View {
    let peer: Peer
    let server: ServerConfig
    @Environment(AppSession.self) private var session
    @Environment(\.dismiss) private var dismiss
    @State private var name = ""
    @State private var note = ""
    @State private var ipv4 = ""
    @State private var overrides = PeerOverrides()
    @State private var error: String?
    @State private var busy = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Name", text: $name)
                    TextField("Note", text: $note)
                    TextField("IPv4 address", text: $ipv4)
                        .font(.mono(.body))
                        .keyboardType(.numbersAndPunctuation)
                } footer: {
                    Text("Name and address changes apply immediately. A new address needs a new client config.")
                }
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()

                OverrideSections(o: $overrides, server: server)

                Section {
                    Text("DNS, AllowedIPs and keepalive are part of the client config: they take effect after the config is issued again.")
                        .font(.footnote)
                        .foregroundStyle(Color.gwText2)
                }
                if let error {
                    Section { Text(error).foregroundStyle(Color.gwErrInk) }
                }
            }
            .groundBackground()
            .navigationTitle("Edit \(peer.name)")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") { Task { await save() } }.disabled(busy)
                }
            }
            .onAppear {
                name = peer.name
                note = peer.note
                ipv4 = peer.ipv4
                overrides = PeerOverrides(peer: peer, server: server)
            }
        }
    }

    private func save() async {
        guard let api = session.api else { return }
        busy = true
        defer { busy = false }
        error = nil
        do {
            var body = try overrides.body(server: server)
            body["name"] = name
            body["note"] = note
            body["ipv4"] = ipv4.trimmingCharacters(in: .whitespaces)
            let r: PeerResult = try await api.send("PATCH", "/peers/\(peer.id)", body)
            session.reportApply(r.applyError)
            dismiss()
        } catch {
            self.error = session.message(for: error)
        }
    }
}
