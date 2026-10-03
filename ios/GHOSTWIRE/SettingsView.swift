import SwiftUI

struct SettingsView: View {
    @Environment(AppSession.self) private var session
    @State private var settings: AppSettings?
    @State private var web: WebSettings?
    @State private var log: LogSettings?
    @State private var stats: StatsSettings?
    @State private var error: String?
    @State private var busy = false
    @State private var offerRestart = false
    @State private var confirmRestart = false
    @State private var confirmShrink = false
    @State private var confirmDisconnect = false

    private static let hourly: [(Int, String)] = [(24, "1 day"), (48, "2 days"), (168, "7 days"), (336, "14 days"), (744, "31 days")]
    private static let daily: [(Int, String)] = [(30, "30 days"), (90, "90 days"), (180, "6 months"), (400, "13 months"),
                                                 (730, "2 years"), (1825, "5 years"), (3660, "10 years")]

    var body: some View {
        NavigationStack {
            Form {
                deviceSection
                if let error {
                    Section { Text(error).foregroundStyle(Color.gwErrInk) }
                }
                if web != nil { webSection }
                if log != nil, stats != nil { retentionSection }
                if log != nil { logSection }
                Section {
                    Button("Restart service…") { confirmRestart = true }
                } footer: {
                    Text("Password, API tokens and backups are managed in the web interface.")
                }
            }
            .groundBackground()
            .navigationTitle("Settings")
            .refreshable { await load() }
            .task { await load() }
            .alert("Restart to apply?", isPresented: $offerRestart) {
                Button("Later", role: .cancel) {}
                Button("Restart now") { Task { await restart() } }
            } message: {
                Text("The web interface uses the new settings after the service restarts. VPN connections stay up.")
            }
            .confirmationDialog("Restart the service?", isPresented: $confirmRestart, titleVisibility: .visible) {
                Button("Restart") { Task { await restart() } }
            } message: {
                Text("The web interface and API are gone for a few seconds. VPN connections stay up.")
            }
            .confirmationDialog("Delete older data?", isPresented: $confirmShrink, titleVisibility: .visible) {
                Button("Save and delete", role: .destructive) { Task { await saveRetention() } }
            } message: {
                Text("The new limits are lower: older log files and traffic history beyond them are deleted. This cannot be undone.")
            }
            .confirmationDialog("Disconnect this iPhone?", isPresented: $confirmDisconnect, titleVisibility: .visible) {
                Button("Disconnect", role: .destructive) { session.disconnect() }
            } message: {
                Text("The token is removed from this iPhone. Revoke it under Settings → API tokens in the web interface too.")
            }
        }
    }

    private var deviceSection: some View {
        Section {
            HStack {
                Lockup(size: 40)
                Spacer()
            }
            .padding(.vertical, 4)
            LabeledContent("Server", value: session.pairing?.url ?? "–")
            if let me = session.me {
                LabeledContent("Signed in as", value: "\(me.name) · \(me.scope == "ro" ? "read only" : "full access")")
                LabeledContent("Server version", value: me.version)
            }
            if let fp = session.pairing?.fingerprint, !fp.isEmpty {
                KV(key: "Pinned certificate (SHA-256)", value: fp, mono: true)
            }
            LabeledContent("App version", value: Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "–")
            Button("Disconnect this iPhone…", role: .destructive) { confirmDisconnect = true }
        } header: {
            Text("This iPhone")
        }
    }

    private var webSection: some View {
        let w = Binding(get: { web! }, set: { web = $0 })
        let opt = { (kp: WritableKeyPath<TLSSettings, String?>) in
            Binding<String>(get: { web!.tls[keyPath: kp] ?? "" }, set: { web!.tls[keyPath: kp] = $0 })
        }
        return Section {
            LabeledContent("Listen address") {
                TextField(":443", text: w.listen).font(.mono(.body)).multilineTextAlignment(.trailing)
            }
            LabeledContent("HTTP listen address") {
                TextField("off", text: w.httpListen).font(.mono(.body)).multilineTextAlignment(.trailing)
            }
            Picker("HTTPS", selection: w.tls.mode) {
                Text("Let's Encrypt").tag("acme")
                Text("Self-signed").tag("selfsigned")
                Text("Certificate files").tag("files")
                Text("Off (reverse proxy)").tag("off")
            }
            if web!.tls.mode == "acme" {
                TextField("Domain", text: opt(\.domain)).font(.mono(.body))
                TextField("Email for Let's Encrypt (optional)", text: opt(\.email)).keyboardType(.emailAddress)
                Toggle("Use the staging CA", isOn: Binding(get: { web!.tls.staging ?? false }, set: { web!.tls.staging = $0 }))
            }
            if web!.tls.mode == "files" {
                TextField("Certificate file", text: opt(\.certFile)).font(.mono(.footnote))
                TextField("Key file", text: opt(\.keyFile)).font(.mono(.footnote))
            }
            Picker("Session length", selection: w.sessionHours) {
                Text("1 hour").tag(1)
                Text("12 hours").tag(12)
                Text("1 day").tag(24)
                Text("7 days").tag(168)
            }
            Button("Save web settings") { Task { await saveWeb() } }
                .disabled(busy || web == settings?.web)
        } header: {
            Text("Web interface")
        } footer: {
            Text("Takes effect after the service restarts.")
        }
        .textInputAutocapitalization(.never)
        .autocorrectionDisabled()
    }

    private var retentionSection: some View {
        let l = Binding(get: { log! }, set: { log = $0 })
        let s = Binding(get: { stats! }, set: { stats = $0 })
        return Section {
            Stepper("Log file size: \(log!.maxSizeMB) MB", value: l.maxSizeMB, in: 1...1000)
            Stepper("Old log files kept: \(log!.maxFiles)", value: l.maxFiles, in: 1...100)
            Picker("Hourly traffic history", selection: s.hourlyHours) {
                ForEach(options(Self.hourly, current: stats!.hourlyHours, unit: "hours"), id: \.0) { Text($0.1).tag($0.0) }
            }
            Picker("Daily traffic history", selection: s.dailyDays) {
                ForEach(options(Self.daily, current: stats!.dailyDays, unit: "days"), id: \.0) { Text($0.1).tag($0.0) }
            }
            Toggle(isOn: Binding(get: { stats!.geoip ?? true }, set: { stats!.geoip = $0 })) {
                VStack(alignment: .leading, spacing: 2) {
                    Text("Show country and network")
                    Text(geoStatusText).font(.caption).foregroundStyle(Color.gwText2)
                }
            }
            Button("Save retention") {
                guard let old = settings, let log, let stats else { return }
                if log.maxFiles < old.log.maxFiles || stats.hourlyHours < old.stats.hourlyHours || stats.dailyDays < old.stats.dailyDays {
                    confirmShrink = true
                } else {
                    Task { await saveRetention() }
                }
            }
            .disabled(busy || (log == settings?.log && stats == settings?.stats))
        } header: {
            Text("Data retention")
        } footer: {
            Text("The log uses up to \(log!.maxSizeMB * (log!.maxFiles + 1)) MB on disk. Connection history is kept as long as the daily traffic history; all-time totals are always kept. Country and network come from the free DB-IP Lite databases, downloaded monthly and looked up on the server only. Applies immediately.")
        }
    }

    private var logSection: some View {
        Section {
            Picker("Log level", selection: Binding(get: { log!.level }, set: { level in
                log!.level = level
                Task { await saveLevel(level) }
            })) {
                ForEach(["debug", "info", "warn", "error"], id: \.self) { Text($0).tag($0) }
            }
            NavigationLink("View log") { LogView() }
        } header: {
            Text("Log")
        } footer: {
            Text(settings?.logPath ?? "")
        }
    }

    private var geoStatusText: String {
        guard let updated = settings?.geo?.updated else { return "Database not downloaded yet" }
        return "Database from \(fmtDate(updated))"
    }

    private func options(_ presets: [(Int, String)], current: Int, unit: String) -> [(Int, String)] {
        presets.contains { $0.0 == current } ? presets : (presets + [(current, "\(current) \(unit)")]).sorted { $0.0 < $1.0 }
    }

    private func load() async {
        guard let api = session.api else { return }
        do {
            let s: AppSettings = try await api.get("/settings")
            settings = s
            web = s.web
            log = s.log
            stats = s.stats
            error = nil
        } catch {
            self.error = session.message(for: error)
        }
    }

    private func patch(_ body: [String: Any?]) async throws -> SettingsResult {
        guard let api = session.api else { throw APIError.unauthorized }
        return try await api.send("PATCH", "/settings", body)
    }

    private func encoded<T: Encodable>(_ v: T) -> Any {
        (try? JSONSerialization.jsonObject(with: JSONEncoder().encode(v))) ?? NSNull()
    }

    private func saveWeb() async {
        guard let web else { return }
        busy = true
        defer { busy = false }
        do {
            let r = try await patch(["web": encoded(web)])
            settings?.web = web
            error = nil
            if r.restartRequired { offerRestart = true }
        } catch {
            self.error = session.message(for: error)
        }
    }

    private func saveRetention() async {
        guard let log, let stats else { return }
        busy = true
        defer { busy = false }
        do {
            _ = try await patch(["log": encoded(log), "stats": encoded(stats)])
            settings?.log = log
            settings?.stats = stats
            error = nil
        } catch {
            self.error = session.message(for: error)
        }
    }

    private func saveLevel(_ level: String) async {
        guard var l = settings?.log else { return }
        l.level = level
        do {
            _ = try await patch(["log": encoded(l)])
            settings?.log.level = level
        } catch {
            self.error = session.message(for: error)
        }
    }

    private func restart() async {
        guard let api = session.api else { return }
        _ = try? await api.data("POST", "/restart")
        session.alert = "Restarting. The app reconnects in a few seconds."
    }
}

struct LogView: View {
    @Environment(AppSession.self) private var session
    @State private var level = "all"
    @State private var lines: [String] = []
    @State private var error: String?

    var body: some View {
        List {
            Section {
                Picker("Level", selection: $level) {
                    Text("All").tag("all")
                    Text("Info").tag("info")
                    Text("Warn").tag("warn")
                    Text("Error").tag("error")
                }
                .pickerStyle(.segmented)
                .listRowBackground(Color.clear)
                .listRowInsets(EdgeInsets())
            }
            if let error {
                Section { Text(error).foregroundStyle(Color.gwErrInk) }
            }
            Section {
                if lines.isEmpty && error == nil {
                    Text("No entries at this level.").foregroundStyle(Color.gwText2)
                }
                ForEach(Array(lines.enumerated()), id: \.offset) { _, line in
                    Text(line)
                        .font(.mono(.caption2))
                        .textSelection(.enabled)
                }
            } footer: {
                Text("Newest first.")
            }
        }
        .groundBackground()
        .navigationTitle("Log")
        .navigationBarTitleDisplayMode(.inline)
        .onChange(of: level) { Task { await load() } }
        .refreshable { await load() }
        .task { await load() }
    }

    private func load() async {
        guard let api = session.api else { return }
        do {
            let data = try await api.data("GET", "/logs?limit=200&level=\(level)")
            let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any]
            let recs = obj?["lines"] as? [[String: Any]] ?? []
            lines = recs.map(formatLogLine)
            error = nil
        } catch {
            self.error = session.message(for: error)
        }
    }
}
