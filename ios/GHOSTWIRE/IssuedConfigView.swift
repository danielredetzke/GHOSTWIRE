import CoreTransferable
import SwiftUI
import UniformTypeIdentifiers

/// A client config as a .conf file for the share sheet.
nonisolated struct ConfFile: Transferable {
    let name: String
    let text: String

    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(exportedContentType: .plainText) { file in
            let url = FileManager.default.temporaryDirectory.appendingPathComponent("\(file.name).conf")
            try file.text.write(to: url, atomically: true, encoding: .utf8)
            return SentTransferredFile(url)
        }
    }
}

/// Shows a freshly issued config once: QR code, share, copy.
struct IssuedConfigContent: View {
    let issued: IssuedConfig
    @State private var copied = false

    private var qrImage: UIImage? {
        guard let qr = issued.qr, let comma = qr.firstIndex(of: ","),
              let data = Data(base64Encoded: String(qr[qr.index(after: comma)...])) else { return nil }
        return UIImage(data: data)
    }

    var body: some View {
        ScrollView {
            VStack(spacing: 16) {
                Notice(text: "This is the only time the private key is shown. Scan or share it now: it is not stored on the server.")
                if let img = qrImage {
                    Image(uiImage: img)
                        .interpolation(.none)
                        .resizable()
                        .scaledToFit()
                        .frame(maxWidth: 280)
                        .padding(12)
                        .background(.white, in: RoundedRectangle(cornerRadius: 12))
                        .accessibilityLabel("QR code of the client config for \(issued.peer.name)")
                    Text("Scan with the WireGuard app: + → Create from QR code.")
                        .font(.footnote)
                        .foregroundStyle(Color.gwText2)
                }
                HStack(spacing: 12) {
                    ShareLink(item: ConfFile(name: issued.peer.name, text: issued.config),
                              preview: SharePreview("\(issued.peer.name).conf")) {
                        Label("Share .conf", systemImage: "square.and.arrow.up")
                    }
                    .buttonStyle(SecondaryButtonStyle())
                    Button {
                        UIPasteboard.general.string = issued.config
                        copied = true
                    } label: {
                        Label(copied ? "Copied" : "Copy", systemImage: copied ? "checkmark" : "doc.on.doc")
                    }
                    .buttonStyle(SecondaryButtonStyle())
                }
                Text(issued.config)
                    .font(.mono(.caption))
                    .foregroundStyle(Color(hex: 0xE6E6E1))
                    .textSelection(.enabled)
                    .padding(14)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(Color(hex: 0x16171A), in: RoundedRectangle(cornerRadius: 10))
            }
            .padding(16)
        }
        .background(Color.gwGround)
    }
}
