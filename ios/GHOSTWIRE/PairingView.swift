import SwiftUI
import VisionKit

/// First screen: pair with a server by scanning the QR code from the web
/// interface (Settings → Pair iOS app) or by entering the details.
struct PairingView: View {
    @Environment(AppSession.self) private var session
    @State private var scanning = false
    @State private var manual = false
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        VStack(spacing: 24) {
            Spacer()
            KamonMark(size: 96)
            VStack(spacing: 6) {
                Text("GHOSTWIRE").font(.system(size: 30, weight: .semibold, design: .monospaced)).tracking(1)
                Text("ゴーストワイヤー").font(.footnote).tracking(4).foregroundStyle(Color.gwText2)
            }
            Text("In the web interface, open **Settings → Pair iOS app** and scan the QR code shown there.")
                .multilineTextAlignment(.center)
                .foregroundStyle(Color.gwText2)
                .padding(.horizontal, 8)
            Spacer()
            if let error { Notice(text: error, isError: true) }
            VStack(spacing: 12) {
                Button {
                    if QRScanner.isAvailable { scanning = true } else { error = "The camera isn't available. Enter the details instead." }
                } label: {
                    Label("Scan pairing QR code", systemImage: "qrcode.viewfinder")
                }
                .buttonStyle(PrimaryButtonStyle())
                Button("Enter manually") { manual = true }
                    .buttonStyle(SecondaryButtonStyle())
            }
            .disabled(busy)
        }
        .padding(24)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color.gwGround)
        .overlay { if busy { ProgressView().controlSize(.large) } }
        .sheet(isPresented: $scanning) {
            NavigationStack {
                QRScanner { code in
                    scanning = false
                    Task { await pair(code) }
                }
                .ignoresSafeArea()
                .navigationTitle("Scan pairing code")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar { ToolbarItem(placement: .cancellationAction) { Button("Cancel") { scanning = false } } }
            }
        }
        .sheet(isPresented: $manual) { ManualPairingView() }
    }

    private func pair(_ code: String) async {
        busy = true
        defer { busy = false }
        do {
            try await session.pair(try Pairing.parse(code))
        } catch {
            self.error = error.localizedDescription
        }
    }
}

struct ManualPairingView: View {
    @Environment(AppSession.self) private var session
    @Environment(\.dismiss) private var dismiss
    @State private var code = ""
    @State private var url = "https://"
    @State private var token = ""
    @State private var fingerprint = ""
    @State private var error: String?
    @State private var busy = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Paste the pairing code", text: $code, axis: .vertical)
                        .font(.mono(.footnote))
                        .lineLimit(3...6)
                } header: {
                    Text("Pairing code")
                } footer: {
                    Text("In the web interface: Settings → Pair iOS app → Copy pairing code.")
                }
                Section {
                    TextField("https://vpn.example.net", text: $url)
                        .keyboardType(.URL)
                    TextField("wgt_…", text: $token)
                        .font(.mono(.footnote))
                    TextField("Fingerprint (self-signed certificates only)", text: $fingerprint)
                        .font(.mono(.footnote))
                } header: {
                    Text("Or enter the details")
                }
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                if let error {
                    Section { Text(error).foregroundStyle(Color.gwErrInk) }
                }
            }
            .groundBackground()
            .navigationTitle("Pair manually")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Connect") { Task { await connect() } }.disabled(busy)
                }
            }
        }
    }

    private func connect() async {
        busy = true
        defer { busy = false }
        error = nil
        do {
            let p: Pairing
            if !code.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                p = try Pairing.parse(code)
            } else {
                p = Pairing(url: url.trimmingCharacters(in: .whitespaces), token: token.trimmingCharacters(in: .whitespaces),
                            fingerprint: fingerprint.trimmingCharacters(in: .whitespaces))
                guard p.token.hasPrefix("wgt_") else { throw APIError.badPairing("The token starts with wgt_.") }
            }
            try await session.pair(p)
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// Live QR scanning with VisionKit.
struct QRScanner: UIViewControllerRepresentable {
    let onCode: (String) -> Void

    static var isAvailable: Bool { DataScannerViewController.isSupported && DataScannerViewController.isAvailable }

    func makeUIViewController(context: Context) -> DataScannerViewController {
        let vc = DataScannerViewController(recognizedDataTypes: [.barcode(symbologies: [.qr])],
                                           qualityLevel: .balanced,
                                           recognizesMultipleItems: false,
                                           isHighFrameRateTrackingEnabled: false,
                                           isHighlightingEnabled: true)
        vc.delegate = context.coordinator
        DispatchQueue.main.async { try? vc.startScanning() }
        return vc
    }

    func updateUIViewController(_ vc: DataScannerViewController, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(onCode: onCode) }

    final class Coordinator: NSObject, DataScannerViewControllerDelegate {
        let onCode: (String) -> Void
        private var done = false

        init(onCode: @escaping (String) -> Void) { self.onCode = onCode }

        func dataScanner(_ dataScanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]) {
            guard !done else { return }
            for item in addedItems {
                if case .barcode(let code) = item, let text = code.payloadStringValue {
                    done = true
                    dataScanner.stopScanning()
                    onCode(text)
                    return
                }
            }
        }
    }
}
