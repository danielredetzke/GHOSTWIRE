import SwiftUI

struct PeerRow: View {
    enum Period { case day, month }
    let peer: Peer
    let period: Period

    var body: some View {
        let down = period == .day ? peer.stats.down24h : peer.stats.down30d
        let up = period == .day ? peer.stats.up24h : peer.stats.up30d
        HStack(alignment: .center, spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                Text(peer.name).font(.body.weight(.semibold)).foregroundStyle(Color.gwText)
                if !peer.note.isEmpty {
                    Text(peer.note).font(.caption).foregroundStyle(Color.gwText2).lineLimit(1)
                }
                StatusBadge(state: PeerState(peer))
            }
            Spacer(minLength: 8)
            VStack(alignment: .trailing, spacing: 4) {
                Text("↓ " + fmtBytes(down)).font(.footnote.monospacedDigit())
                Text("↑ " + fmtBytes(up)).font(.footnote.monospacedDigit()).foregroundStyle(Color.gwText2)
            }
            .accessibilityElement(children: .ignore)
            .accessibilityLabel("Download \(fmtBytes(down)), upload \(fmtBytes(up))")
        }
        .padding(.vertical, 8)
        .contentShape(Rectangle())
    }
}

struct PeersView: View {
    @Environment(AppSession.self) private var session
    @State private var list: PeerList?
    @State private var query = ""
    @State private var filter = "all"
    @State private var error: String?
    @State private var adding = false
    @State private var deleting: Peer?
    @State private var path = NavigationPath()

    private var filtered: [Peer] {
        let q = query.lowercased()
        return (list?.peers ?? []).filter { p in
            let hit = q.isEmpty || "\(p.name) \(p.ipv4) \(p.note)".lowercased().contains(q)
            return hit && (filter == "all" || PeerState(p).key == filter)
        }
    }

    var body: some View {
        NavigationStack(path: $path) {
            List {
                Section {
                    Picker("Status", selection: $filter) {
                        Text("All").tag("all")
                        Text("Online").tag("online")
                        Text("Offline").tag("offline")
                        Text("Disabled").tag("disabled")
                    }
                    .pickerStyle(.segmented)
                    .listRowBackground(Color.clear)
                    .listRowInsets(EdgeInsets())
                }
                if let error {
                    Section { Notice(text: error, isError: true) }
                        .listRowBackground(Color.clear)
                        .listRowInsets(EdgeInsets())
                }
                Section {
                    ForEach(filtered) { p in
                        NavigationLink(value: p.id) { PeerRow(peer: p, period: .month) }
                            .swipeActions(edge: .trailing) {
                                // The app-wide ink tint would override the destructive red.
                                Button(role: .destructive) { deleting = p } label: { Label("Delete", systemImage: "trash") }
                                    .tint(Color.gwBad)
                                Button { Task { await toggle(p) } } label: {
                                    Label(p.enabled ? "Disable" : "Enable", systemImage: p.enabled ? "pause.circle" : "play.circle")
                                }
                                .tint(.gray)
                            }
                    }
                    if list != nil && filtered.isEmpty {
                        Text(list?.peers.isEmpty == true ? "No peers yet. Tap + to add one." : "No peers match this filter.")
                            .font(.footnote)
                            .foregroundStyle(Color.gwText2)
                    }
                } footer: {
                    if let list {
                        Text("\(list.peers.count) of \(list.capacity) addresses in \(list.network) used. Traffic is for the last 30 days, from the peer's side.")
                    }
                }
            }
            .groundBackground()
            .searchable(text: $query, prompt: "Name, address or note")
            .navigationTitle("Peers")
            .toolbar {
                Button { adding = true } label: { Label("Add peer", systemImage: "plus") }
            }
            .navigationDestination(for: String.self) { PeerDetailView(peerID: $0) }
            .sheet(isPresented: $adding, onDismiss: { Task { await load() } }) { AddPeerView() }
            .confirmationDialog("Delete peer?", isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } }),
                                titleVisibility: .visible, presenting: deleting) { p in
                Button("Delete \(p.name)", role: .destructive) { Task { await delete(p) } }
            } message: { _ in
                Text("The device loses access immediately. Its traffic history is deleted too.")
            }
            .refreshable { await load() }
            .task {
                await load()
                #if DEBUG
                // Development: `-openFirstPeer YES` and `-addPeer YES`.
                if UserDefaults.standard.bool(forKey: "openFirstPeer"), let first = list?.peers.first { path.append(first.id) }
                if UserDefaults.standard.bool(forKey: "addPeer") { adding = true }
                #endif
                while !Task.isCancelled {
                    try? await Task.sleep(for: .seconds(15))
                    await load()
                }
            }
        }
    }

    private func load() async {
        guard let api = session.api else { return }
        do {
            list = try await api.get("/peers")
            error = nil
        } catch {
            self.error = session.message(for: error)
        }
    }

    private func toggle(_ p: Peer) async {
        guard let api = session.api else { return }
        do {
            let r: PeerResult = try await api.send("POST", "/peers/\(p.id)/" + (p.enabled ? "disable" : "enable"))
            session.reportApply(r.applyError)
            await load()
        } catch {
            session.alert = session.message(for: error)
        }
    }

    private func delete(_ p: Peer) async {
        guard let api = session.api else { return }
        do {
            let r: ApplyResult = try await api.send("DELETE", "/peers/\(p.id)")
            session.reportApply(r.applyError)
            await load()
        } catch {
            session.alert = session.message(for: error)
        }
    }
}
