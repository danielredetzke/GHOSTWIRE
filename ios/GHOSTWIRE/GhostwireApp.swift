import SwiftUI

@main
struct GhostwireApp: App {
    @State private var session = AppSession()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(session)
                .tint(Color.gwAccent)
        }
    }
}

struct RootView: View {
    @Environment(AppSession.self) private var session

    var body: some View {
        @Bindable var session = session
        Group {
            if session.api == nil {
                PairingView()
            } else {
                MainTabView()
            }
        }
        .alert("GHOSTWIRE", isPresented: Binding(get: { session.alert != nil }, set: { if !$0 { session.alert = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(session.alert ?? "")
        }
    }
}

struct MainTabView: View {
    enum Tab: String { case dashboard, peers, server, settings }

    @Environment(AppSession.self) private var session
    @State private var tab: Tab = {
        #if DEBUG
        // Development: `-tab peers` opens a tab directly.
        if let t = UserDefaults.standard.string(forKey: "tab"), let tab = Tab(rawValue: t) { return tab }
        #endif
        return .dashboard
    }()

    var body: some View {
        TabView(selection: $tab) {
            DashboardView()
                .tabItem { Label("Dashboard", systemImage: "square.grid.2x2") }
                .tag(Tab.dashboard)
            PeersView()
                .tabItem { Label("Peers", systemImage: "person.2") }
                .tag(Tab.peers)
            ServerView()
                .tabItem { Label("Server", systemImage: "server.rack") }
                .tag(Tab.server)
            SettingsView()
                .tabItem { Label("Settings", systemImage: "slider.horizontal.3") }
                .tag(Tab.settings)
        }
        .task { await session.loadMe() }
    }
}
