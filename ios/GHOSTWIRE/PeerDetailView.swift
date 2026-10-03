import SwiftUI

struct PeerDetailView: View {
    let peerID: String
    @Environment(AppSession.self) private var session
    @Environment(\.dismiss) private var dismiss
    @State private var peer: Peer?
    @State private var server: ServerConfig?
    @State private var range = "7d"
    @State private var points: [StatPoint] = []
    @State private var error: String?
    @State private var issued: IssuedConfig?
    @State private var confirmIssue = false
    @State private var confirmDelete = false
    @State private var editing = false
    @State private var sessions: [ConnSession] = []
    @State private var allSessions = false

    var body: some View {
        ScrollViewReader { proxy in
        ScrollView {
            VStack(spacing: 16) {
                if let error { Notice(text: error, isError: true) }
                if let p = peer {
                    header(p)
                    traffic
                    connection(p)
                    history.id("history")
                    clientConfig(p)
                    settings(p)
                } else if error == nil {
                    ProgressView().padding(40)
                }
            }
            .padding(16)
        }
        .background(Color.gwGround)
        #if DEBUG
        // Development: `-scrollToHistory YES` for screenshots of the history.
        .task(id: sessions.count) {
            if UserDefaults.standard.bool(forKey: "scrollToHistory"), !sessions.isEmpty { proxy.scrollTo("history", anchor: .top) }
        }
        #endif
        }
        .navigationTitle(peer?.name ?? "Peer")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if let p = peer {
                Menu {
                    Button { Task { await toggle(p) } } label: {
                        Label(p.enabled ? "Disable" : "Enable", systemImage: p.enabled ? "pause.circle" : "play.circle")
                    }
                    Button(role: .destructive) { confirmDelete = true } label: { Label("Delete", systemImage: "trash") }
                } label: {
                    Label("Actions", systemImage: "ellipsis.circle")
                }
            }
        }
        .confirmationDialog("Delete \(peer?.name ?? "peer")?", isPresented: $confirmDelete, titleVisibility: .visible) {
            Button("Delete peer", role: .destructive) { Task { await delete() } }
        } message: {
            Text("The device loses access immediately. Its traffic history is deleted too. This cannot be undone.")
        }
        .confirmationDialog("Issue a new config?", isPresented: $confirmIssue, titleVisibility: .visible) {
            Button("Issue new config") { Task { await issue() } }
        } message: {
            Text("New keys are created. The device that uses the current config stops working until it gets the new one.")
        }
        .sheet(item: $issued, onDismiss: { Task { await load() } }) { IssuedConfigView(issued: $0) }
        .sheet(isPresented: $editing, onDismiss: { Task { await load() } }) {
            if let p = peer, let s = server { PeerEditView(peer: p, server: s) }
        }
        .refreshable { await load() }
        .task { await load() }
    }

    private func header(_ p: Peer) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(p.name).font(.title2.weight(.semibold))
            StatusBadge(state: PeerState(p))
            Text((p.note.isEmpty ? "" : p.note + " · ") + "created " + fmtDate(p.created))
                .font(.footnote)
                .foregroundStyle(Color.gwText2)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var traffic: some View {
        VStack(alignment: .leading, spacing: 10) {
            SectionTitle(text: "Traffic")
            Picker("Range", selection: $range) {
                Text("24 h").tag("24h")
                Text("7 days").tag("7d")
                Text("30 days").tag("30d")
            }
            .pickerStyle(.segmented)
            .onChange(of: range) { Task { await loadStats() } }
            TrafficTotals(points: points)
            if !points.isEmpty { TrafficChart(points: points, range: range, mode: .pair) }
        }
        .card()
    }

    private func connection(_ p: Peer) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            SectionTitle(text: "Connection")
            KV(key: "Tunnel address", value: p.ipv4 + "/32" + (p.ipv6.map { "\n" + $0 + "/128" } ?? ""), mono: true)
            KV(key: "Endpoint", value: p.stats.endpoint.isEmpty ? "–" : p.stats.endpoint, mono: true)
            KV(key: "Location", value: p.stats.location?.label.isEmpty == false ? p.stats.location!.label : "–")
            KV(key: "Latest handshake", value: ago(p.stats.lastHandshake))
            KV(key: "Public key", value: p.publicKey.isEmpty ? "–" : p.publicKey, mono: true)
            KV(key: "Preshared key", value: p.hasPresharedKey ? "Set" : "None")
            KV(key: "All-time traffic", value: "Download \(fmtBytes(p.stats.downTotal)) · Upload \(fmtBytes(p.stats.upTotal))")
        }
        .card()
    }

    private var history: some View {
        VStack(alignment: .leading, spacing: 0) {
            SectionTitle(text: "Connection history")
            Text("Newest first. A new row starts when the device changes networks.")
                .font(.caption)
                .foregroundStyle(Color.gwText2)
                .padding(.bottom, 8)
            if sessions.isEmpty {
                Text("No connections recorded yet.").font(.footnote).foregroundStyle(Color.gwText2).padding(.vertical, 6)
            }
            let shown = allSessions ? sessions : Array(sessions.prefix(8))
            ForEach(shown) { se in
                HStack(alignment: .top, spacing: 12) {
                    VStack(alignment: .leading, spacing: 4) {
                        HStack(spacing: 6) {
                            Text(se.start.formatted(.dateTime.day().month(.abbreviated).hour().minute()))
                                .font(.subheadline.weight(.medium))
                            if se.open {
                                HStack(spacing: 4) {
                                    Circle().fill(Color.gwGood).frame(width: 6, height: 6)
                                    Text("online")
                                }
                                .font(.caption.weight(.medium))
                                .padding(.horizontal, 6).padding(.vertical, 2)
                                .background(Color.gwBadge, in: Capsule())
                            }
                        }
                        Text(se.geo?.label.isEmpty == false ? se.geo!.label : "Unknown location")
                            .font(.footnote)
                            .foregroundStyle(se.geo == nil ? Color.gwText2 : Color.gwText)
                        Text(se.ip + " · " + fmtDuration(se.seconds))
                            .font(.mono(.caption))
                            .foregroundStyle(Color.gwText2)
                    }
                    Spacer(minLength: 8)
                    VStack(alignment: .trailing, spacing: 4) {
                        Text("↓ " + fmtBytes(se.down)).font(.footnote.monospacedDigit())
                        Text("↑ " + fmtBytes(se.up)).font(.footnote.monospacedDigit()).foregroundStyle(Color.gwText2)
                    }
                }
                .padding(.vertical, 8)
                .accessibilityElement(children: .combine)
                if se.id != shown.last?.id { Divider() }
            }
            if sessions.count > 8 {
                Button(allSessions ? "Show fewer" : "Show all \(sessions.count)") { allSessions.toggle() }
                    .font(.footnote.weight(.medium))
                    .padding(.top, 8)
            }
            HStack(spacing: 4) {
                Text("Country and network:")
                Link("IP Geolocation by DB-IP", destination: URL(string: "https://db-ip.com")!).underline()
            }
            .font(.caption2)
            .foregroundStyle(Color.gwText2)
            .padding(.top, 10)
        }
        .card()
    }

    private func clientConfig(_ p: Peer) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            SectionTitle(text: "Client configuration")
            Text("This server doesn't keep the peer's private key. To set up a device again, issue a new config. The old one stops working.")
                .font(.footnote)
                .foregroundStyle(Color.gwText2)
            Button { confirmIssue = true } label: { Label("Issue new config & QR", systemImage: "qrcode") }
                .buttonStyle(PrimaryButtonStyle())
            if let s = p.setup {
                Text(s.expired ? "The setup link expired \(fmtDate(s.expires)). Manage setup links in the web interface."
                               : "A setup link is waiting to be opened (until \(fmtDate(s.expires))). Issuing a config here replaces it.")
                    .font(.footnote)
                    .foregroundStyle(Color.gwWarnInk)
            }
            Text(p.configIssued.map { "Last issued \(fmtDate($0))." } ?? "No config issued yet.")
                .font(.caption)
                .foregroundStyle(Color.gwText2)
        }
        .card()
    }

    private func settings(_ p: Peer) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                SectionTitle(text: "Settings")
                Spacer()
                Button("Edit") { editing = true }.disabled(server == nil)
            }
            KV(key: "AllowedIPs (client)", value: p.effectiveAllowedIPs.joined(separator: ", ") + (p.allowedIPs == nil ? " · server default" : ""), mono: true)
            KV(key: "DNS", value: (p.effectiveDNS.isEmpty ? "none" : p.effectiveDNS.joined(separator: ", ")) + (p.dns == nil ? " · server default" : ""), mono: true)
            KV(key: "Persistent keepalive", value: (p.effectiveKeepalive > 0 ? "\(p.effectiveKeepalive) s" : "off") + (p.keepalive == nil ? " · server default" : ""))
        }
        .card()
    }

    private func load() async {
        guard let api = session.api else { return }
        do {
            async let p: Peer = api.get("/peers/\(peerID)")
            async let s: ServerConfig = api.get("/server")
            (peer, server) = try await (p, s)
            error = nil
            await loadStats()
            if let r: SessionsResponse = try? await api.get("/peers/\(peerID)/sessions?limit=100") { sessions = r.sessions }
        } catch {
            self.error = session.message(for: error)
        }
    }

    private func loadStats() async {
        guard let api = session.api else { return }
        if let r: StatsResponse = try? await api.get("/peers/\(peerID)/stats?range=\(range)") { points = r.points }
    }

    private func toggle(_ p: Peer) async {
        guard let api = session.api else { return }
        do {
            let r: PeerResult = try await api.send("POST", "/peers/\(p.id)/" + (p.enabled ? "disable" : "enable"))
            session.reportApply(r.applyError)
            peer = r.peer
        } catch {
            session.alert = session.message(for: error)
        }
    }

    private func delete() async {
        guard let api = session.api else { return }
        do {
            let r: ApplyResult = try await api.send("DELETE", "/peers/\(peerID)")
            session.reportApply(r.applyError)
            dismiss()
        } catch {
            session.alert = session.message(for: error)
        }
    }

    private func issue() async {
        guard let api = session.api else { return }
        do {
            let body: [String: Any?]? = nil
            issued = try await api.send("POST", "/peers/\(peerID)/issue-config", body)
        } catch {
            session.alert = session.message(for: error)
        }
    }
}
