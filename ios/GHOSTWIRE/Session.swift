import Foundation
import Observation
import Security

/// What the web interface's "Pair iOS app" QR code contains.
struct Pairing: Codable, Equatable {
    var url: String
    var token: String
    var fingerprint: String

    /// Parses the pairing JSON from the QR code or the "Copy pairing code" button.
    static func parse(_ text: String) throws -> Pairing {
        guard let p = try? JSONDecoder().decode(Pairing.self, from: Data(text.utf8)),
              p.url.hasPrefix("https://") || p.url.hasPrefix("http://"),
              p.token.hasPrefix("wgt_") else {
            throw APIError.badPairing("This is not a GHOSTWIRE pairing code.")
        }
        return p
    }
}

/// The pairing is stored in the keychain, readable only on this device.
enum Keychain {
    private static let base: [String: Any] = [
        kSecClass as String: kSecClassGenericPassword,
        kSecAttrService as String: "aero.redetzke.ghostwire",
        kSecAttrAccount as String: "pairing",
    ]

    static func save(_ p: Pairing) {
        delete()
        var q = base
        q[kSecValueData as String] = try? JSONEncoder().encode(p)
        q[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        SecItemAdd(q as CFDictionary, nil)
    }

    static func load() -> Pairing? {
        var q = base
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess, let data = out as? Data else { return nil }
        return try? JSONDecoder().decode(Pairing.self, from: data)
    }

    static func delete() {
        SecItemDelete(base as CFDictionary)
    }
}

/// App-wide state: the paired server and messages shown as alerts.
@Observable
final class AppSession {
    private(set) var pairing: Pairing?
    private(set) var api: API?
    var me: Me?
    var alert: String?

    init() {
        #if DEBUG
        // Development: `-pairing '<json>'` as a launch argument pairs the app.
        // Read the raw arguments: UserDefaults would parse the JSON as a plist.
        let args = ProcessInfo.processInfo.arguments
        if let i = args.firstIndex(of: "-pairing"), i + 1 < args.count, let p = try? Pairing.parse(args[i + 1]) {
            Keychain.save(p)
        }
        #endif
        if let p = Keychain.load() {
            pairing = p
            api = API(pairing: p)
        }
    }

    func pair(_ p: Pairing) async throws {
        let api = API(pairing: p)
        let me: Me = try await api.get("/auth/me")
        Keychain.save(p)
        self.pairing = p
        self.api = api
        self.me = me
    }

    func loadMe() async {
        guard let api, me == nil else { return }
        me = try? await api.get("/auth/me")
    }

    func disconnect() {
        Keychain.delete()
        pairing = nil
        api = nil
        me = nil
    }

    /// Turns an error into a message; a revoked token returns to pairing.
    func message(for error: Error) -> String {
        if case APIError.unauthorized = error {
            disconnect()
        }
        return error.localizedDescription
    }

    /// Reports a kernel apply failure after a successful save.
    func reportApply(_ applyError: String?) {
        if let e = applyError, !e.isEmpty {
            alert = "Saved, but applying to WireGuard failed: " + e
        }
    }
}
