import SwiftUI

struct DashboardView: View {
    @Environment(AppSession.self) private var session
    @State private var status: Status?
    @State private var peers: [Peer] = []
    @State private var points: [StatPoint] = []
    @State private var error: String?

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 16) {
                    if let error { Notice(text: error, isError: true) }
                    if let s = status {
                        content(s)
                    } else if error == nil {
                        ProgressView().padding(40)
                    }
                }
                .padding(16)
            }
            .background(Color.gwGround)
            .navigationTitle("Dashboard")
            .toolbar {
                ToolbarItem(placement: .topBarLeading) { KamonMark(size: 30) }
            }
            .navigationDestination(for: String.self) { PeerDetailView(peerID: $0) }
            .refreshable { await load() }
            .task {
                while !Task.isCancelled {
                    await load()
                    try? await Task.sleep(for: .seconds(30))
                }
            }
        }
    }

    @ViewBuilder private func content(_ s: Status) -> some View {
        let failing = s.checks.filter { !$0.ok }
        let ifUp = s.checks.first { $0.name == "WireGuard interface" }?.ok ?? false

        Text("Endpoint \(s.endpoint) · \(s.ipv4)")
            .font(.mono(.caption))
            .foregroundStyle(Color.gwText2)
            .frame(maxWidth: .infinity, alignment: .leading)

        if !failing.isEmpty {
            Notice(text: "Needs attention: " + failing.map { "\($0.name) (\($0.detail))" }.joined(separator: " · "), isError: true)
        }

        // A Grid (not LazyVGrid) gives both tiles of a row the same height.
        Grid(horizontalSpacing: 12, verticalSpacing: 12) {
            GridRow {
                Tile(title: "Peers online", value: "\(s.peers.online)", suffix: "/ \(s.peers.total)",
                     sub: "\(s.peers.disabled) disabled · \(s.peers.never) never connected")
                Tile(title: "Interface", value: ifUp ? "Up" : "Down", dot: ifUp ? .gwGood : .gwBad,
                     sub: s.healthy ? "All checks pass" : "\(failing.count) check(s) failing")
            }
            GridRow {
                Tile(title: "Last 24 h", value: fmtBytes(s.traffic24h.down + s.traffic24h.up),
                     sub: "Down \(fmtBytes(s.traffic24h.down)) · Up \(fmtBytes(s.traffic24h.up))")
                Tile(title: "Last 30 days", value: fmtBytes(s.traffic30d.down + s.traffic30d.up),
                     sub: s.topPeer30d.isEmpty ? "No traffic yet" : "Top peer: \(s.topPeer30d)")
            }
        }

        VStack(alignment: .leading, spacing: 10) {
            SectionTitle(text: "Traffic, all peers · 24 h")
            TrafficChart(points: points, range: "24h", mode: .total)
        }
        .card()

        VStack(alignment: .leading, spacing: 0) {
            SectionTitle(text: "Peers").padding(.bottom, 8)
            let top = peers.sorted { $0.stats.down24h + $0.stats.up24h > $1.stats.down24h + $1.stats.up24h }.prefix(6)
            if top.isEmpty {
                Text("No peers yet.").font(.footnote).foregroundStyle(Color.gwText2).padding(.vertical, 8)
            }
            ForEach(Array(top)) { p in
                NavigationLink(value: p.id) {
                    PeerRow(peer: p, period: .day)
                }
                .buttonStyle(.plain)
                if p.id != top.last?.id { Divider() }
            }
        }
        .card()
    }

    private func load() async {
        guard let api = session.api else { return }
        do {
            async let s: Status = api.get("/status")
            async let p: PeerList = api.get("/peers")
            async let st: StatsResponse = api.get("/stats?range=24h")
            let (a, b, c) = try await (s, p, st)
            status = a
            peers = b.peers
            points = c.points
            error = nil
        } catch {
            self.error = session.message(for: error)
        }
    }
}
