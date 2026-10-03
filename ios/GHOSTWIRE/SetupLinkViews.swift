import SwiftUI

/// "Show it here" or "Send a setup link", with the link's options. Used when
/// a peer is created and when its config is issued again.
struct Handover {
    var link = false
    var hours = 24
    var pin = true

    var body: [String: Any?] {
        link ? ["delivery": "link", "linkHours": hours, "linkPIN": pin] : [:]
    }
}

struct HandoverSection: View {
    @Binding var h: Handover
    var showHint = "QR code and .conf right after you tap Create. Best when the device is next to you."
    var linkHint = "A one-time link you send to the device's owner. Keys are made when the link is opened and never stored."

    var body: some View {
        Section {
            Picker("Hand over the config", selection: $h.link) {
                Text("Show it here").tag(false)
                Text("Send a setup link").tag(true)
            }
            .pickerStyle(.inline)
            .labelsHidden()
            if h.link {
                Picker("Link valid for", selection: $h.hours) {
                    Text("1 hour").tag(1)
                    Text("24 hours").tag(24)
                    Text("7 days").tag(168)
                }
                Toggle("Require a PIN", isOn: $h.pin)
            }
        } header: {
            Text("Hand over the config")
        } footer: {
            Text(h.link ? linkHint + (h.pin ? " Send the PIN by another channel than the link." : "") : showHint)
        }
    }
}

func qrImage(_ dataURL: String?) -> UIImage? {
    guard let s = dataURL, let comma = s.firstIndex(of: ","),
          let data = Data(base64Encoded: String(s[s.index(after: comma)...])) else { return nil }
    return UIImage(data: data)
}

/// Shows a setup link to send: share, copy, PIN, QR code of the link.
struct SetupLinkContent: View {
    let name: String
    let setup: SetupSecret
    @State private var copied: String?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                Notice(text: setup.pin != nil
                       ? "Anyone with this link and the PIN can set up this peer once. Send the PIN separately, e.g. by phone or another messenger."
                       : "Anyone with this link can set up this peer once. Send it only to the device's owner.")
                VStack(alignment: .leading, spacing: 12) {
                    KV(key: "Link", value: setup.url, mono: true)
                    HStack(spacing: 12) {
                        if let url = URL(string: setup.url) {
                            ShareLink(item: url, subject: Text("VPN setup"), message: Text("Your VPN setup link for \(name)")) {
                                Label("Share link", systemImage: "square.and.arrow.up")
                            }
                            .buttonStyle(PrimaryButtonStyle())
                        }
                        copyButton("Copy", value: setup.url)
                    }
                }
                .card()
                if let pin = setup.pin {
                    HStack {
                        VStack(alignment: .leading, spacing: 2) {
                            Text("PIN").font(.caption).foregroundStyle(Color.gwText2)
                            Text(pin).font(.mono(.title, weight: .semibold)).tracking(6).textSelection(.enabled)
                        }
                        Spacer()
                        copyButton("Copy PIN", value: pin).frame(maxWidth: 150)
                    }
                    .card()
                }
                VStack(alignment: .leading, spacing: 12) {
                    KV(key: "Valid until", value: fmtStamp(setup.expires))
                    KV(key: "Uses", value: "Once. Then the link stops working.")
                }
                .card()
                if let img = qrImage(setup.qr) {
                    VStack(spacing: 8) {
                        Image(uiImage: img)
                            .interpolation(.none)
                            .resizable()
                            .scaledToFit()
                            .frame(maxWidth: 220)
                            .padding(12)
                            .background(.white, in: RoundedRectangle(cornerRadius: 12))
                            .accessibilityLabel("QR code of the setup link for \(name)")
                        Text("The QR code holds only the link, not the config.")
                            .font(.footnote)
                            .foregroundStyle(Color.gwText2)
                    }
                    .frame(maxWidth: .infinity)
                }
                Text("Until the link is used, you can share it again or revoke it on the peer's page.")
                    .font(.footnote)
                    .foregroundStyle(Color.gwText2)
            }
            .padding(16)
        }
        .background(Color.gwGround)
    }

    private func copyButton(_ title: String, value: String) -> some View {
        Button {
            UIPasteboard.general.string = value
            copied = value
        } label: {
            Label(copied == value ? "Copied" : title, systemImage: copied == value ? "checkmark" : "doc.on.doc")
        }
        .buttonStyle(SecondaryButtonStyle())
    }
}

/// The result of issuing: the config, or the setup link.
struct IssueOutcomeContent: View {
    let outcome: IssueOutcome
    var body: some View {
        switch outcome {
        case .config(let c): IssuedConfigContent(issued: c)
        case .link(let l): SetupLinkContent(name: l.peer.name, setup: l.setup)
        }
    }
}

func outcomeTitle(_ o: IssueOutcome) -> String {
    switch o {
    case .config(let c): "Config for \(c.peer.name)"
    case .link(let l): "Setup link for \(l.peer.name)"
    }
}

/// Issue a config for an existing peer: choose how to hand it over, then
/// show the result in the same sheet.
struct IssueSheet: View {
    let peer: Peer
    var startWithLink = false
    @Environment(AppSession.self) private var session
    @Environment(\.dismiss) private var dismiss
    @State private var h = Handover()
    @State private var outcome: IssueOutcome?
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Group {
                if let outcome {
                    IssueOutcomeContent(outcome: outcome)
                } else {
                    Form {
                        Section {
                            if !peer.publicKey.isEmpty {
                                Text("New keys are created. The device that uses the current config stops working once it is replaced.")
                            }
                            if peer.setup != nil {
                                Text("This replaces the current setup link.")
                            }
                        }
                        .font(.footnote)
                        .foregroundStyle(Color.gwText2)
                        HandoverSection(h: $h,
                                        showHint: "New keys now; QR code and .conf on this screen.",
                                        linkHint: peer.publicKey.isEmpty ? "A one-time link you send to the device's owner."
                                                                         : "The current config keeps working until the link is opened.")
                        if let error {
                            Section { Text(error).foregroundStyle(Color.gwErrInk) }
                        }
                    }
                    .groundBackground()
                }
            }
            .navigationTitle(outcome.map(outcomeTitle) ?? (peer.publicKey.isEmpty ? "Issue config" : "Issue new config"))
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                if outcome == nil {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Continue") { Task { await issue() } }.disabled(busy)
                    }
                } else {
                    ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
                }
            }
        }
        .interactiveDismissDisabled(outcome != nil)
        .onAppear { h.link = startWithLink }
    }

    private func issue() async {
        guard let api = session.api else { return }
        busy = true
        defer { busy = false }
        error = nil
        do {
            let o = try await api.issue("/peers/\(peer.id)/issue-config", h.link ? h.body : nil)
            session.reportApply(o.applyError)
            outcome = o
        } catch {
            self.error = session.message(for: error)
        }
    }
}

/// Fetches a pending setup link again and shows it.
struct SetupLinkSheet: View {
    let peer: Peer
    @Environment(AppSession.self) private var session
    @Environment(\.dismiss) private var dismiss
    @State private var setup: SetupSecret?
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Group {
                if let setup {
                    SetupLinkContent(name: peer.name, setup: setup)
                } else if let error {
                    ScrollView { Notice(text: error, isError: true).padding(16) }.background(Color.gwGround)
                } else {
                    ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity).background(Color.gwGround)
                }
            }
            .navigationTitle("Setup link for \(peer.name)")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } } }
            .task {
                guard let api = session.api else { return }
                do { setup = try await api.get("/peers/\(peer.id)/setup") } catch { self.error = session.message(for: error) }
            }
        }
    }
}
